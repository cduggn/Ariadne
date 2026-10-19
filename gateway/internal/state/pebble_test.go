package state

import (
	"testing"
	"time"

	"github.com/cduggn/ariadne/gateway/internal/decide"
)

func openTestPebble(t *testing.T, dir string) *Pebble {
	t.Helper()
	p, err := OpenPebble(dir)
	if err != nil {
		t.Fatalf("OpenPebble(%s) error = %v", dir, err)
	}
	return p
}

func TestPebbleRoundTripsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	p := openTestPebble(t, dir)
	at := time.Date(2026, 10, 18, 9, 30, 0, 123_000_000, time.UTC)
	batch := Batch{
		Buckets: map[string]Bucket{"platform": {Tokens: 1234.5, Last: at}, "*": {Tokens: 7, Last: at.Add(time.Second)}},
		Runs: map[decide.RunID]Run{
			"run1": {Pod: "vllm-0", Started: at, Usage: decide.RunUsage{PromptTokens: 9000, CompletionTokens: 60, BodyBytes: 7000}, Seen: at},
			"run2": {Pod: "vllm-1", Started: at, Seen: at},
		},
		Pods: map[string]Pod{"vllm-0": {Started: at, MeanServiceS: 0.3}},
	}
	if err := p.Apply(batch); err != nil {
		t.Fatalf("Apply error = %v", err)
	}
	if err := p.Apply(Batch{
		Runs:     map[decide.RunID]Run{"run1": {Pod: "vllm-1", Started: at, Seen: at.Add(time.Minute)}},
		DropRuns: []decide.RunID{"run2", "never-there"},
		Buckets:  map[string]Bucket{"platform": {Tokens: 99, Last: at.Add(time.Minute)}},
	}); err != nil {
		t.Fatalf("second Apply error = %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close error = %v", err)
	}

	p = openTestPebble(t, dir)
	defer p.Close()
	snap, err := p.Load()
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if got := snap.Buckets["platform"]; got.Tokens != 99 || !got.Last.Equal(at.Add(time.Minute)) {
		t.Fatalf("platform bucket = %+v, want the overwrite", got)
	}
	if got := snap.Buckets["*"]; got.Tokens != 7 || !got.Last.Equal(at.Add(time.Second)) {
		t.Fatalf("* bucket = %+v, want tokens 7", got)
	}
	if len(snap.Runs) != 1 {
		t.Fatalf("runs = %+v, want run1 only", snap.Runs)
	}
	if got := snap.Runs["run1"]; got.Pod != "vllm-1" || !got.Started.Equal(at) || !got.Seen.Equal(at.Add(time.Minute)) || got.Usage != (decide.RunUsage{}) {
		t.Fatalf("run1 = %+v, want the overwrite", got)
	}
	if got := snap.Pods["vllm-0"]; !got.Started.Equal(at) || got.MeanServiceS != 0.3 {
		t.Fatalf("vllm-0 = %+v, want started %v and 0.3", got, at)
	}
}

func TestPebbleEmptyDirectoryLoadsEmpty(t *testing.T) {
	p := openTestPebble(t, t.TempDir())
	defer p.Close()
	snap, err := p.Load()
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if len(snap.Buckets)+len(snap.Runs)+len(snap.Pods) != 0 {
		t.Fatalf("Load = %+v, want empty", snap)
	}
	if err := p.Apply(Batch{}); err != nil {
		t.Fatalf("Apply(empty) error = %v", err)
	}
}

func TestPebbleSkipsAnEntryItCannotDecode(t *testing.T) {
	p := openTestPebble(t, t.TempDir())
	defer p.Close()
	if err := p.Apply(Batch{Runs: map[decide.RunID]Run{"good": {Pod: "a"}}}); err != nil {
		t.Fatalf("Apply error = %v", err)
	}
	if err := p.db.Set([]byte(runPrefix+"bad"), []byte{9, '{'}, nil); err != nil {
		t.Fatalf("Set error = %v", err)
	}
	snap, err := p.Load()
	if err == nil {
		t.Fatal("Load error = nil, want the bad entry reported")
	}
	if got := snap.Runs["good"]; got.Pod != "a" || len(snap.Runs) != 1 {
		t.Fatalf("runs = %+v, want good only", snap.Runs)
	}
}
