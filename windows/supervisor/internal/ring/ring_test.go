package ring

import "testing"

func TestEvictionAndQuery(t *testing.T) {
	b := New(4)
	for i := 0; i < 6; i++ {
		kind := "a.x"
		if i%2 == 1 {
			kind = "b.y"
		}
		b.Add(kind, "t", "", nil)
	}
	oldest, next := b.Bounds()
	if oldest != 3 || next != 7 {
		t.Fatalf("bounds = %d,%d want 3,7", oldest, next)
	}
	all := b.Read(Query{})
	if len(all) != 4 || all[0].Seq != 3 || all[3].Seq != 6 {
		t.Fatalf("read all = %+v", all)
	}
	// Requesting evicted range starts at oldest held: caller sees the gap.
	if got := b.Read(Query{FromSeq: 1, Limit: 1}); got[0].Seq != 3 {
		t.Fatalf("from evicted seq returned %d", got[0].Seq)
	}
	if got := b.Read(Query{Kinds: []string{"b."}}); len(got) != 2 || got[0].Seq != 4 {
		t.Fatalf("prefix filter = %+v", got)
	}
	if got := b.Read(Query{FromSeq: 4, ToSeq: 6}); len(got) != 2 {
		t.Fatalf("window = %+v", got)
	}
}
