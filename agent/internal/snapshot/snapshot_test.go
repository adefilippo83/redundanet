package snapshot

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

func TestDirectoryIsDeterministic(t *testing.T) {
	a := NewDirectory([]Entry{{Name: []byte("b.txt"), UTF8: true, Kind: KindFile, Size: 3, Cap: "rn:r1:x"}, {Name: []byte("a"), UTF8: true, Kind: KindDir, Cap: "rn:r1:y"}})
	b := NewDirectory([]Entry{{Name: []byte("a"), UTF8: true, Kind: KindDir, Cap: "rn:r1:y"}, {Name: []byte("b.txt"), UTF8: true, Kind: KindFile, Size: 3, Cap: "rn:r1:x"}})
	ea, _ := a.Encode()
	eb, _ := b.Encode()
	if !bytes.Equal(ea, eb) {
		t.Fatal("the same folder encoded differently")
	}
	back, err := DecodeDirectory(ea)
	if err != nil || len(back.Entries) != 2 || back.Entries[0].NameString() != "a" || back.Find("b.txt") == nil || back.Find("zz") != nil {
		t.Fatalf("decode: %v %+v", err, back)
	}
	name, ok := MakeName("caf\xc3\xa9")
	if !ok || string(name) != "café" {
		t.Fatal("utf8 name")
	}
	bad, ok := MakeName("\xff\xfe")
	if ok || (&Entry{Name: bad}).NameString() != `"\xff\xfe"` {
		t.Fatalf("invalid name: %q", (&Entry{Name: bad}).NameString())
	}
	if _, err := DecodeDirectory([]byte{0xa1, 0x67, 0x76, 0x65, 0x72, 0x73, 0x69, 0x6f, 0x6e, 0x09}); err == nil {
		t.Fatal("a future version decoded")
	}
}

func TestIndex(t *testing.T) {
	x := &Index{Version: IndexVersion, Device: "dev", Snapshots: []Snapshot{{ID: "s1", Top: "a"}, {ID: "s2", Top: "b"}}}
	enc, _ := x.Encode()
	back, err := DecodeIndex(enc)
	if err != nil || back.Latest().ID != "s2" || back.Find("s1").Top != "a" || back.Find("nope") != nil {
		t.Fatalf("index: %v", err)
	}
}

func TestPrune(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	var snaps []Snapshot
	// One snapshot a day for 400 days, with a partial one beside day 3.
	for d := 400; d >= 0; d-- {
		s := now.Add(-time.Duration(d) * 24 * time.Hour)
		snaps = append(snaps, Snapshot{ID: fmt.Sprintf("d%d", d), Set: "home", Started: s})
		if d == 3 {
			snaps = append(snaps, Snapshot{ID: "d3-partial", Set: "home", Started: s.Add(time.Hour), Partial: true})
		}
	}
	kept := Prune(snaps, DefaultRetention, now)
	ids := map[string]bool{}
	for _, s := range kept {
		ids[s.ID] = true
	}
	for d := 0; d <= 7; d++ {
		if !ids[fmt.Sprintf("d%d", d)] {
			t.Fatalf("day %d within 7 days dropped", d)
		}
	}
	if !ids["d3-partial"] {
		t.Fatal("a partial snapshot within 7 days must stay")
	}
	if !ids["d29"] || ids["d45"] && !ids["d42"] {
		t.Fatalf("daily window: %v", ids)
	}
	if len(kept) > 7+1+30+12+12 || len(kept) < 30 {
		t.Fatalf("kept %d snapshots", len(kept))
	}
	if ids["d399"] || ids["d380"] {
		t.Fatal("a year-old daily snapshot survived")
	}
	// Order is preserved.
	for i := 1; i < len(kept); i++ {
		if kept[i].Started.Before(kept[i-1].Started) {
			t.Fatal("order lost")
		}
	}
}
