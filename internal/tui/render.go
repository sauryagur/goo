package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gur/goo/internal/goo"
)

// formatEventLine renders one event as a dense, fixed-width stream line:
//
//	18421 PUT     images/cat.jpg
//
// It is a pure function so it can be unit-tested without a terminal.
func formatEventLine(ev goo.Event, width int) string {
	seq := fmt.Sprintf("%d", ev.Sequence)
	action := ev.Action
	if len(action) > 7 {
		action = action[:7]
	}
	// ref is bucket/key.
	ref := ev.Bucket + "/" + ev.Key

	// budget: seq(6) + " " + action(7) + "  " + ref. Trim ref if needed.
	const seqW, actW = 6, 7
	refW := width - seqW - actW - 3
	if refW < 8 {
		refW = 8
	}
	if len(ref) > refW {
		ref = ref[:refW-1] + "…"
	}
	return fmt.Sprintf("%-*s %-*s %s", seqW, seq, actW, action, ref)
}

// storageSummary builds the right-hand STORAGE panel content from the current
// object list. It groups by bucket and shows per-bucket object counts + bytes.
func storageSummary(objs []goo.Object) string {
	if len(objs) == 0 {
		return "  (no objects)"
	}
	type bucketStat struct {
		count int
		bytes int64
	}
	byBucket := make(map[string]*bucketStat)
	var order []string
	for _, o := range objs {
		bs, ok := byBucket[o.Bucket]
		if !ok {
			bs = &bucketStat{}
			byBucket[o.Bucket] = bs
			order = append(order, o.Bucket)
		}
		bs.count++
		bs.bytes += o.Size
	}
	sort.Strings(order)

	var b strings.Builder
	b.WriteString(fmt.Sprintf("  %d objects, %s\n\n", len(objs), humanBytes(totalBytes(objs))))
	for _, name := range order {
		bs := byBucket[name]
		nameW := 18
		n := name
		if len(n) > nameW {
			n = n[:nameW-1] + "…"
		}
		fmt.Fprintf(&b, "  %-*s %4d  %s\n", nameW, n, bs.count, humanBytes(bs.bytes))
	}
	return b.String()
}

func totalBytes(objs []goo.Object) int64 {
	var t int64
	for _, o := range objs {
		t += o.Size
	}
	return t
}

// humanBytes formats a byte count with binary units. Pure + tested.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTPE"[exp]) + "iB"
}

// eventFilter returns only the events whose ref contains q (case-insensitive).
// Used by the "/" search. A empty query returns all events unchanged.
func eventFilter(evs []goo.Event, q string) []goo.Event {
	if q == "" {
		return evs
	}
	q = strings.ToLower(q)
	out := evs[:0]
	for _, ev := range evs {
		if strings.Contains(strings.ToLower(ev.Bucket+"/"+ev.Key), q) {
			out = append(out, ev)
		}
	}
	return out
}
