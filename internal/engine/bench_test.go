package engine

import (
	"bytes"
	"io"
	"testing"

	"github.com/gur/goo/internal/goo"
)

func BenchmarkPut(b *testing.B) {
	e, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()
	data := bytes.Repeat([]byte("x"), 1024)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bucket := "b"
		key := "k" + itoa(i)
		if _, err := e.Put(bucket, key, bytes.NewReader(data), true); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGet(b *testing.B) {
	e, _ := Open(b.TempDir())
	defer e.Close()
	data := bytes.Repeat([]byte("x"), 4096)
	e.Put("b", "k", bytes.NewReader(data), true)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rc, _, err := e.Get("b", "k")
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, rc)
		rc.Close()
	}
}

func BenchmarkEventAppend(b *testing.B) {
	e, _ := Open(b.TempDir())
	defer e.Close()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := e.Log().Append(goo.ActionPut, "b", "k"+itoa(i), int64(i), "h", uint64(i+1)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReplay(b *testing.B) {
	e, _ := Open(b.TempDir())
	defer e.Close()
	const n = 5000
	for i := 0; i < n; i++ {
		e.Log().Append(goo.ActionPut, "b", "k"+itoa(i), 10, "h", uint64(i+1))
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		evs, err := e.Log().Replay(1)
		if err != nil {
			b.Fatal(err)
		}
		if len(evs) != n {
			b.Fatalf("replay got %d, want %d", len(evs), n)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
