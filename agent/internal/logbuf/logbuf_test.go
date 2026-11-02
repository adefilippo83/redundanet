package logbuf

import (
	"io"
	"log/slog"
	"testing"
)

func TestRing(t *testing.T) {
	b := New(slog.NewTextHandler(io.Discard, nil), 3)
	lg := slog.New(b)
	lg.Info("one", "a", 1)
	lg.With("component", "x").Warn("two")
	lg.WithGroup("g").Info("three", "k", "v")
	lg.Error("four")
	got := b.Last(0)
	if len(got) != 3 || got[0].Msg != "two" || got[2].Msg != "four" {
		t.Fatalf("%+v", got)
	}
	if got[0].Attrs != "component=x" || got[1].Attrs != "g.k=v" {
		t.Fatalf("attrs: %q %q", got[0].Attrs, got[1].Attrs)
	}
	if n := len(b.Last(2)); n != 2 {
		t.Fatalf("Last(2) = %d", n)
	}
}
