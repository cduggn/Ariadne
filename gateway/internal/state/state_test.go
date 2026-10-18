package state

import (
	"testing"
	"time"

	"github.com/cduggn/ariadne/gateway/internal/decide"
)

func TestBatchEmpty(t *testing.T) {
	if !(Batch{}).Empty() {
		t.Fatal("zero Batch is not Empty")
	}
	cases := map[string]Batch{
		"bucket": {Buckets: map[string]Bucket{"t": {}}},
		"run":    {Runs: map[decide.RunID]Run{"r": {}}},
		"drop":   {DropRuns: []decide.RunID{"r"}},
		"pod":    {Pods: map[string]Pod{"p": {}}},
	}
	for name, b := range cases {
		if b.Empty() {
			t.Errorf("Batch with a %s is Empty", name)
		}
	}
}

func TestMemoryKeepsNothing(t *testing.T) {
	var s Store = Memory{}
	if err := s.Apply(Batch{Runs: map[decide.RunID]Run{"r": {Pod: "a", Seen: time.Now()}}}); err != nil {
		t.Fatalf("Apply error = %v", err)
	}
	snap, err := s.Load()
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if len(snap.Buckets)+len(snap.Runs)+len(snap.Pods) != 0 {
		t.Fatalf("Load = %+v, want empty", snap)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close error = %v", err)
	}
}
