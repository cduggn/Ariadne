// Package state is the Gate's durable store. The Gate keeps its working
// state in memory and writes each change through as a Batch. A Store
// answers Load once at startup with what it kept, so a gateway restart
// keeps run bindings, tenant balances and worker identities.
package state

import (
	"time"

	"github.com/cduggn/ariadne/gateway/internal/decide"
)

// Bucket is the persisted part of a tenant's token bucket. Its rate and
// burst come from config, so a config change applies on the next start.
type Bucket struct {
	Tokens float64   `json:"tokens"`
	Last   time.Time `json:"last"`
}

// Run is one run's binding and the usage its last settled step reported.
// Started is the bound worker's process start when the binding was made,
// so a binding to a worker that has since restarted reads as void.
type Run struct {
	Pod     string          `json:"pod"`
	Started time.Time       `json:"started"`
	Usage   decide.RunUsage `json:"usage"`
	Seen    time.Time       `json:"seen"`
}

// Pod is the persisted part of a worker: its process start, which is its
// identity, and its service-time EWMA.
type Pod struct {
	Started      time.Time `json:"started"`
	MeanServiceS float64   `json:"mean_service_s"`
}

// Snapshot is everything a Store kept.
type Snapshot struct {
	Buckets map[string]Bucket
	Runs    map[decide.RunID]Run
	Pods    map[string]Pod
}

// Batch is one mutation's changes. Put maps overwrite whole entries and
// DropRuns removes runs.
type Batch struct {
	Buckets  map[string]Bucket
	Runs     map[decide.RunID]Run
	DropRuns []decide.RunID
	Pods     map[string]Pod
}

// Empty reports whether the batch changes nothing.
func (b Batch) Empty() bool {
	return len(b.Buckets) == 0 && len(b.Runs) == 0 && len(b.DropRuns) == 0 && len(b.Pods) == 0
}

// Store keeps the Gate's state across restarts. Load runs once at startup.
// Apply commits one Batch atomically and runs under the Gate's lock, so it
// must be quick. Close releases the store.
type Store interface {
	Load() (Snapshot, error)
	Apply(Batch) error
	Close() error
}

// Memory is the Store for a gateway whose state lives only in memory. It
// keeps nothing.
type Memory struct{}

func (Memory) Load() (Snapshot, error) { return Snapshot{}, nil }
func (Memory) Apply(Batch) error       { return nil }
func (Memory) Close() error            { return nil }
