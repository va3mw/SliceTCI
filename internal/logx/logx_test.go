package logx

import "testing"

// TestRingWraps: once the ring is full the oldest entries are dropped and
// Since still returns entries in order.
func TestRingWraps(t *testing.T) {
	l := New(t.TempDir())
	defer l.Close()
	for i := 0; i < ringSize+10; i++ {
		l.Log(Info, "test", "%d", i)
	}
	all := l.Since(0)
	if len(all) != ringSize {
		t.Fatalf("got %d entries, want %d", len(all), ringSize)
	}
	if all[0].ID != 11 || all[len(all)-1].ID != ringSize+10 {
		t.Fatalf("first/last ID %d/%d, want 11/%d", all[0].ID, all[len(all)-1].ID, ringSize+10)
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID != all[i-1].ID+1 {
			t.Fatalf("out of order at %d: %d after %d", i, all[i].ID, all[i-1].ID)
		}
	}
	if got := l.Since(ringSize + 8); len(got) != 2 {
		t.Fatalf("Since near the end returned %d entries, want 2", len(got))
	}
}
