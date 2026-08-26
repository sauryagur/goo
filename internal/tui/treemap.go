package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gur/goo/internal/goo"
	squarify "github.com/jeffwilliams/squarify"
)

// TreemapItem is one tile in the treemap: a bucket and the total bytes it holds.
type TreemapItem struct {
	Label string
	Value float64
	Color int // index into a small palette
}

// bucketSizer adapts a slice of TreemapItems to squarify's TreeSizer interface.
// The root has one child per bucket; each child's Size is its byte total.
type bucketSizer struct {
	items []TreemapItem
}

func (b bucketSizer) Size() float64 {
	var s float64
	for _, it := range b.items {
		s += it.Value
	}
	return s
}
func (b bucketSizer) NumChildren() int               { return len(b.items) }
func (b bucketSizer) Child(i int) squarify.TreeSizer { return bucketChild{b.items[i]} }

type bucketChild struct{ it TreemapItem }

func (c bucketChild) Size() float64                  { return c.it.Value }
func (c bucketChild) NumChildren() int               { return 0 }
func (c bucketChild) Child(i int) squarify.TreeSizer { return nil }

// palette is a small set of readable terminal-ish colors (0-7). Buckets cycle
// through it so the treemap stays legible.
var palette = []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14"}

// ComputeTreemap builds the treemap tiles for the given buckets. It always
// returns a slice (possibly empty) and never panics, even for empty or zero
// input. Each returned tile carries its grid-ready geometry derived from
// squarify, but the actual rasterization into a string happens in
// RenderTreemap so this stays a pure-ish data step that's easy to assert.
func ComputeTreemap(items []TreemapItem) []treemapTile {
	if len(items) == 0 {
		return nil
	}
	// drop zero-value items so a 0-byte bucket doesn't get a degenerate tile.
	nonzero := items[:0]
	for _, it := range items {
		if it.Value > 0 {
			nonzero = append(nonzero, it)
		}
	}
	if len(nonzero) == 0 {
		return nil
	}
	root := bucketSizer{items: nonzero}
	rect := squarify.Rect{X: 0, Y: 0, W: 100, H: 100}
	blocks, _ := squarify.Squarify(root, rect, squarify.Options{Sort: true})

	tiles := make([]treemapTile, 0, len(blocks))
	for i, b := range blocks {
		child := b.TreeSizer.(bucketChild)
		tiles = append(tiles, treemapTile{
			Label: child.it.Label,
			Value: child.it.Value,
			X:     b.X,
			Y:     b.Y,
			W:     b.W,
			H:     b.H,
			Color: nonzero[i%len(nonzero)].Color,
		})
	}
	return tiles
}

// treemapTile is one laid-out bucket rectangle in the 0..100 coordinate space.
type treemapTile struct {
	Label      string
	Value      float64
	X, Y, W, H float64
	Color      int
}

// bucketsForTreemap aggregates objects into TreemapItems sized by byte total.
// It sorts buckets by size descending for stable, readable layouts.
func bucketsForTreemap(objs []goo.Object) []TreemapItem {
	totals := make(map[string]float64)
	for _, o := range objs {
		totals[o.Bucket] += float64(o.Size)
	}
	names := make([]string, 0, len(totals))
	for n := range totals {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return totals[names[i]] > totals[names[j]] })

	items := make([]TreemapItem, 0, len(names))
	for i, n := range names {
		items = append(items, TreemapItem{
			Label: n,
			Value: totals[n],
			Color: i % len(palette),
		})
	}
	return items
}

// RenderTreemap rasterizes the bucket treemap into a width x height character
// grid. It is robust to tiny terminals and degenerate inputs:
//   - empty/no objects -> a centered "no data" message
//   - one bucket -> fills the whole area with its label
//   - very small buckets -> still get a tile (squarify handles proportionality)
func RenderTreemap(objs []goo.Object, width, height int) string {
	if width < 4 || height < 3 {
		return "" // too small to be useful
	}
	items := bucketsForTreemap(objs)
	if len(items) == 0 {
		return centerText("no data", width, height)
	}

	tiles := ComputeTreemap(items)
	if len(tiles) == 0 {
		return centerText("no data", width, height)
	}

	// rasterize: build a grid of color indices, then overlay labels.
	grid := make([][]int, height)
	for y := range grid {
		grid[y] = make([]int, width)
		for x := range grid[y] {
			grid[y][x] = -1
		}
	}
	for ti, t := range tiles {
		x0 := int(t.X / 100 * float64(width))
		x1 := int((t.X + t.W) / 100 * float64(width))
		y0 := int(t.Y / 100 * float64(height))
		y1 := int((t.Y + t.H) / 100 * float64(height))
		if x1 <= x0 {
			x1 = x0 + 1
		}
		if y1 <= y0 {
			y1 = y0 + 1
		}
		if x0 < 0 {
			x0 = 0
		}
		if y0 < 0 {
			y0 = 0
		}
		if x1 > width {
			x1 = width
		}
		if y1 > height {
			y1 = height
		}
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				grid[y][x] = ti
			}
		}
	}

	// choose which tile gets a label: the largest one(s) that fit a label.
	// for simplicity, label every tile if it's big enough.
	var b strings.Builder
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			ti := grid[y][x]
			if ti < 0 {
				b.WriteByte(' ')
				continue
			}
			// draw the first row of a tile as its label character when room.
			t := tiles[ti]
			color := palette[t.Color%len(palette)]
			ch := ' '
			if y == y0Of(tiles, ti, height) && fitsLabel(t, width) {
				// place a single bright marker; full labels drawn separately below
				ch = '█'
			}
			b.WriteString(fmt.Sprintf("\x1b[48;5;%sm%c\x1b[0m", color, ch))
		}
		b.WriteByte('\n')
	}

	// overlay a legend/labels line under the grid (separate, non-grid text).
	legend := renderTreemapLegend(objs, width)
	return strings.TrimRight(b.String(), "\n") + "\n" + legend
}

func y0Of(tiles []treemapTile, ti, height int) int {
	t := tiles[ti]
	y0 := int(t.Y / 100 * float64(height))
	if y0 < 0 {
		y0 = 0
	}
	return y0
}

func fitsLabel(t treemapTile, width int) bool {
	tw := int(t.W / 100 * float64(width))
	return tw >= 6 // need a few columns to show anything
}

// renderTreemapLegend prints bucket -> bytes below the grid.
func renderTreemapLegend(objs []goo.Object, width int) string {
	items := bucketsForTreemap(objs)
	var b strings.Builder
	for i, it := range items {
		color := palette[it.Color%len(palette)]
		fmt.Fprintf(&b, "\x1b[48;5;%sm \x1b[0m %-*s %s   ",
			color, 12, clip(it.Label, 12), humanBytes(int64(it.Value)))
		if i > 0 && i%2 == 1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func centerText(s string, width, height int) string {
	lines := make([]string, height)
	mid := height / 2
	for i := range lines {
		if i == mid {
			pad := (width - len(s)) / 2
			if pad < 0 {
				pad = 0
			}
			line := strings.Repeat(" ", pad) + s
			if len(line) < width {
				line += strings.Repeat(" ", width-len(line))
			}
			lines[i] = line
		} else {
			lines[i] = strings.Repeat(" ", width)
		}
	}
	return strings.Join(lines, "\n")
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
