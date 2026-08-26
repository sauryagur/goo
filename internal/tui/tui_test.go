package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/gur/goo/internal/engine"
	"github.com/gur/goo/internal/goo"
)

func TestFormatEventLine(t *testing.T) {
	ev := goo.Event{Sequence: 18421, Action: goo.ActionPut, Bucket: "images", Key: "cat.jpg"}
	line := formatEventLine(ev, 40)
	if !strings.Contains(line, "18421") || !strings.Contains(line, "PUT") || !strings.Contains(line, "images/cat.jpg") {
		t.Fatalf("bad line: %q", line)
	}
	// longer ref should be trimmed, not crash.
	long := goo.Event{Sequence: 1, Action: goo.ActionPut, Bucket: "b", Key: strings.Repeat("x", 200)}
	if len(formatEventLine(long, 30)) == 0 {
		t.Fatal("empty line")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.0KiB"},
		{1536, "1.5KiB"},
		{1048576, "1.0MiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.n); got != c.want {
			t.Fatalf("humanBytes(%d)=%q want %q", c.n, got, c.want)
		}
	}
}

func TestEventFilter(t *testing.T) {
	evs := []goo.Event{
		{Bucket: "images", Key: "a.jpg"},
		{Bucket: "videos", Key: "b.mp4"},
		{Bucket: "images", Key: "c.png"},
	}
	got := eventFilter(evs, "images")
	if len(got) != 2 {
		t.Fatalf("filter images -> %d, want 2", len(got))
	}
	if len(eventFilter(evs, "")) != 3 {
		t.Fatal("empty query should return all")
	}
	if len(eventFilter(evs, "zzz")) != 0 {
		t.Fatal("no match should return none")
	}
}

func TestStorageSummary(t *testing.T) {
	objs := []goo.Object{
		{Bucket: "b", Key: "1", Size: 10},
		{Bucket: "a", Key: "1", Size: 5},
		{Bucket: "a", Key: "2", Size: 7},
	}
	s := storageSummary(objs)
	if !strings.Contains(s, "a") || !strings.Contains(s, "b") {
		t.Fatalf("missing buckets: %q", s)
	}
	if !strings.Contains(storageSummary(nil), "no objects") {
		t.Fatal("empty summary should say no objects")
	}
}

func TestComputeTreemap(t *testing.T) {
	// empty and zero-value inputs must not panic and return nil/empty.
	if ComputeTreemap(nil) != nil {
		t.Fatal("nil items -> nil")
	}
	if ComputeTreemap([]TreemapItem{{Label: "x", Value: 0}}) != nil {
		t.Fatal("zero-value items -> nil")
	}
	items := []TreemapItem{
		{Label: "images", Value: 100, Color: 0},
		{Label: "videos", Value: 50, Color: 1},
		{Label: "docs", Value: 1, Color: 2},
	}
	tiles := ComputeTreemap(items)
	if len(tiles) != 3 {
		t.Fatalf("want 3 tiles, got %d", len(tiles))
	}
	// every tile area should be within the 0..100 coordinate space.
	for _, tl := range tiles {
		if tl.X < 0 || tl.Y < 0 || tl.W <= 0 || tl.H <= 0 || tl.X+tl.W > 101 || tl.Y+tl.H > 101 {
			t.Fatalf("tile out of bounds: %+v", tl)
		}
	}
	// single large tile should fill most of the space.
	one := ComputeTreemap([]TreemapItem{{Label: "only", Value: 10, Color: 0}})
	if len(one) != 1 || one[0].W < 90 {
		t.Fatalf("single tile should fill space: %+v", one)
	}
}

func TestRenderTreemapRobust(t *testing.T) {
	objs := []goo.Object{
		{Bucket: "images", Key: "a", Size: 1000},
		{Bucket: "videos", Key: "b", Size: 20},
		{Bucket: "logs", Key: "c", Size: 1}, // very small object
	}
	// tiny/odd dimensions must not crash.
	for _, d := range [][2]int{{40, 10}, {10, 5}, {4, 3}, {80, 20}} {
		out := RenderTreemap(objs, d[0], d[1])
		if out == "" && d[0] >= 4 && d[1] >= 3 {
			t.Fatalf("empty render for %dx%d", d[0], d[1])
		}
	}
	// empty objects -> "no data".
	if !strings.Contains(RenderTreemap(nil, 40, 10), "no data") {
		t.Fatal("empty -> no data")
	}
}

// TestModelKeyHandling drives Update with key messages and checks the model
// transitions, without rendering a terminal.
func TestModelKeyHandling(t *testing.T) {
	e, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.Put("b", "k", strings.NewReader("x"), true)
	m, err := NewModel(e)
	if err != nil {
		t.Fatal(err)
	}
	m.width = 120
	m.height = 30
	m.layout()

	// toggle treemap on.
	mm, _ := m.Update(tea.KeyPressMsg{Text: "t"})
	m2 := mm.(model)
	if !m2.showTmap {
		t.Fatal("t should toggle treemap on")
	}

	// search mode: '/' then type 'b' then enter.
	mm, _ = m2.Update(tea.KeyPressMsg{Text: "/"})
	m3 := mm.(model)
	if !m3.searching {
		t.Fatal("should be searching")
	}
	mm, _ = m3.Update(tea.KeyPressMsg{Text: "b"})
	m3 = mm.(model)
	if m3.search != "b" {
		t.Fatalf("search = %q", m3.search)
	}
	mm, _ = m3.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m3 = mm.(model)
	if m3.searching {
		t.Fatal("enter should end search")
	}

	// 'q' quits (returns tea.Quit cmd). We just check it doesn't panic and the
	// model type is preserved.
	mm, cmd := m3.Update(tea.KeyPressMsg{Text: "q"})
	if _, ok := mm.(model); !ok {
		t.Fatal("model type lost")
	}
	_ = cmd
}

// TestLiveEventArrival ensures incoming events append to the model via Update.
func TestLiveEventArrival(t *testing.T) {
	e, _ := engine.Open(t.TempDir())
	defer e.Close()
	e.Put("b", "k1", strings.NewReader("x"), true)
	m, _ := NewModel(e)
	before := len(m.events)
	ev := goo.Event{Sequence: 999, Action: goo.ActionDelete, Bucket: "b", Key: "k1", Timestamp: time.Now()}
	mm, _ := m.Update(evMsg(ev))
	m2 := mm.(model)
	if len(m2.events) != before+1 {
		t.Fatalf("event not appended: %d -> %d", before, len(m2.events))
	}
}
