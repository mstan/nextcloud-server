package ring

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJournalSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	j, err := OpenJournal(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	b := NewJournaled(8, j, nil)
	for i := 0; i < 5; i++ {
		b.Add("test.a", "t", "", map[string]string{"i": string(rune('0' + i))})
	}
	j.Close()

	prior, err := LoadJournal(path, 8)
	if err != nil || len(prior) != 5 {
		t.Fatalf("loaded %d events, err %v", len(prior), err)
	}
	j2, _ := OpenJournal(path, 1<<20)
	defer j2.Close()
	b2 := NewJournaled(8, j2, prior)
	if seq := b2.Add("test.b", "t", "", nil); seq != 6 {
		t.Fatalf("seq after restart = %d, want 6 (continuing)", seq)
	}
	got := b2.Read(Query{})
	if len(got) != 6 || got[0].Seq != 1 || got[0].Fields["i"] != "0" || got[5].Kind != "test.b" {
		t.Fatalf("ring after restart: %+v", got)
	}
}

func TestJournalRotatesAndLoadsBothGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	j, err := OpenJournal(path, 600) // a few events per generation
	if err != nil {
		t.Fatal(err)
	}
	b := NewJournaled(1000, j, nil)
	for i := 0; i < 40; i++ {
		b.Add("test.r", "t", "padding padding padding", nil)
	}
	j.Close()
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("no rotated generation: %v", err)
	}
	for _, p := range []string{path, path + ".1"} {
		if st, _ := os.Stat(p); st.Size() > 600 {
			t.Fatalf("%s is %d bytes, max 600", p, st.Size())
		}
	}
	ev, err := LoadJournal(path, 0)
	if err != nil || len(ev) == 0 || ev[len(ev)-1].Seq != 40 {
		t.Fatalf("loaded %d events (last %+v), err %v", len(ev), ev[len(ev)-1:], err)
	}
	for i := 1; i < len(ev); i++ {
		if ev[i].Seq != ev[i-1].Seq+1 {
			t.Fatalf("generations not contiguous at %d: %d after %d", i, ev[i].Seq, ev[i-1].Seq)
		}
	}
}

func TestLoadJournalSkipsTornLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	os.WriteFile(path, []byte(`{"seq":1,"time":"2026-10-16T00:00:00Z","kind":"a","source":"s"}`+"\n"+`{"seq":2,"ti`), 0o644)
	ev, err := LoadJournal(path, 10)
	if err != nil || len(ev) != 1 || ev[0].Seq != 1 {
		t.Fatalf("got %+v, %v", ev, err)
	}
}

func TestSeedWithGapKeepsWindowContiguous(t *testing.T) {
	now := time.Now()
	prior := []Event{{Seq: 1, Time: now, Kind: "a"}, {Seq: 2, Time: now, Kind: "a"}, {Seq: 7, Time: now, Kind: "b"}, {Seq: 8, Time: now, Kind: "b"}}
	b := NewJournaled(16, nil, prior)
	got := b.Read(Query{})
	if len(got) != 2 || got[0].Seq != 7 || got[1].Seq != 8 {
		t.Fatalf("window after gap: %+v", got)
	}
	if seq := b.Add("c", "s", "", nil); seq != 9 {
		t.Fatalf("next seq %d", seq)
	}
}

func TestSearchFiltersJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	j, _ := OpenJournal(path, 1<<20)
	b := NewJournaled(8, j, nil)
	b.Add("watch.event", "w", "", nil)
	b.Add("proc.start", "web", "", nil)
	b.Add("watch.rescan", "w", "", nil)
	j.Close()
	ev, err := Search(path, Query{Kinds: []string{"watch."}})
	if err != nil || len(ev) != 2 || ev[1].Kind != "watch.rescan" {
		t.Fatalf("got %+v, %v", ev, err)
	}
}
