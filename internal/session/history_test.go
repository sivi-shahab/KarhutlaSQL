package session

import "testing"

func TestHistory_Ring(t *testing.T) {
	h := NewHistory(3)
	ids := func() (out []string) {
		for _, q := range h.Snapshot() {
			out = append(out, q.RequestID)
		}
		return out
	}
	for i, want := range [][]string{{"a"}, {"a", "b"}, {"a", "b", "c"}, {"b", "c", "d"}, {"c", "d", "e"}} {
		h.Add(Query{RequestID: string(rune('a' + i))})
		if got := ids(); len(got) != len(want) || got[0] != want[0] || got[len(got)-1] != want[len(want)-1] {
			t.Fatalf("step %d: %v want %v", i, got, want)
		}
	}
	var nilH *History
	nilH.Add(Query{})
	if nilH.Snapshot() != nil {
		t.Fatal("nil history")
	}
}
