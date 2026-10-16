# Gateway state backend: in-memory by default, RocksDB per shard for more than one replica

Status: plan, not built. Companion to D-53 (overflow) and the "one replica, because the run
table lives in memory" line in `design/decisions.md`.

## What RocksDB does and does not buy

RocksDB is an embedded store: one process, one directory. It does not share state between
gateway replicas. What it gives a replica is a durable, crash-safe copy of its own state,
so the replica can be rolled, rescheduled or restarted without losing run bindings and
tenant quota. Distribution comes from **ownership**: each run id (and the quota that run
spends) is owned by exactly one replica, chosen by a stable hash, and that replica is the
only one that reads or writes it. RocksDB is then the right store for a shard, exactly as
it is under Kafka Streams, Flink and TiKV.

Truly global, exactly-consistent counters across replicas (one tenant bucket all replicas
debit) would need Raft over RocksDB (dragonboat) or a network store. Phase 3 below. It is
probably not needed: divided quota is how every production rate limiter without a central
store works.

## Where each piece of state lives today

| State | File | Lines |
|---|---|---|
| Tenant buckets: `bucket`, `Gate.buckets`, `bucketFor`, charge in Admit, correction in Settle, off-box charge and settle | `gateway/internal/fleet/gate.go` | 149-167, 223, 332-351, 417-431, 472-516, 547-554 |
| Run table: `run`, `Gate.runs`, TTL prune in Observe, bind in Admit, usage in Settle and SettleOffbox, `boundPod` | `gateway/internal/fleet/gate.go` | 138-143, 222, 267-272, 359-368, 421-428, 513-516, 536-545 |
| Per-worker gateway-side state: `podState` (scrape, ready, boot, reservations, meanServiceS), `Restarted`, `workerStates` | `gateway/internal/fleet/gate.go` | 95-134, 290-301, 561-589 |
| Per-worker queue: `Queue`, `worker` (inFlight, lanes), `waiter`, `Slot` | `gateway/internal/fleet/queue.go` | 37-83 |
| Scraper and warm-up: `member` (phase, ring, passes), `Fleet` | `gateway/internal/fleet/worker.go` | 104-147 |
| Hop permits: `Hopper.slots` | `gateway/internal/hop/hopper.go` | 42, 59 |
| Overflow circuit: `up`, `probes`, `slots` | `gateway/internal/overflow/overflow.go` | 107-109 |
| Prometheus registry and the fleet collector | `gateway/internal/metrics/metrics.go` | 72-93, 296-306 |
| Callers of the Gate | `gateway/internal/serve/server.go` | 177, 197, 246, 254, 267 |
| Flags and wiring | `gateway/cmd/gateway/main.go` | 36-66, 95-100 |

## What is persisted, what is divided, what stays local

| State | Multi-replica treatment | Persisted in RocksDB |
|---|---|---|
| Run table | Sharded by run id. Owner replica only. | Yes |
| Tenant buckets | Divided: each replica gets rate/N and burst/N. | Yes (the replica's share) |
| Pod boot, mean service EWMA | Replica-local, but kept across restart | Yes |
| Reservations | Replica-local. Never persisted: the requests behind them die with the process. | No |
| Queue, waiters, in-flight | Replica-local. max_inflight/N and max_queued/N per replica. The scrape is the truth for the worker's real load across replicas. | No |
| Scrape, ready, warm-up ring | Replica-local, rebuilt within one scrape window | No |
| Hop permits, overflow circuit | Replica-local, max_inflight/N | No |
| Metrics | Replica-local, add a `replica` label, sum in PromQL | No |

## Configuration

Every flag keeps the `GW_<NAME>` environment default. A gateway started with no new flags
behaves exactly as today.

```
--state memory|rocksdb   GW_STATE       default memory
--state-dir PATH         GW_STATE_DIR   rocksdb only, the RocksDB directory
--shard I/N              GW_SHARD       default 0/1: this replica owns every run
--peers pod=url,...      GW_PEERS       required when N > 1 unless the ingress hashes on the request id
```

Single instance is the default and needs nothing. `--state rocksdb --shard 0/1` is a single
instance that survives restarts, useful on its own for rolling deploys. `--shard` with
N > 1 divides quota and worker slots and turns on forwarding.

Under Kubernetes the gateway becomes a StatefulSet with one PVC per replica for the state
directory, `GW_SHARD` built from the pod ordinal in the hostname and the replica count.

## Phase 1: extract the state behind an interface, behavior unchanged

New package `gateway/internal/state`. The hot path stays as it is: the Gate keeps its maps
and its one mutex, and the store is write-through. Reads never touch the store. Each
mutating Gate method appends to a batch while it holds the lock and commits it before
unlocking.

```go
package state

type Store interface {
    Load() (Snapshot, error)      // at startup
    Apply(b Batch) error          // atomic, after every mutation
    Close() error
}

type Snapshot struct {
    Buckets map[string]Bucket
    Runs    map[decide.RunID]Run
    Pods    map[string]Pod       // worker identity and meanServiceS only
}

type Batch struct {
    PutBuckets map[string]Bucket
    PutRuns    map[decide.RunID]Run
    DelRuns    []decide.RunID
    PutPods    map[string]Pod
}
```

`state.Memory` implements `Load` as empty and `Apply` as a no-op. The Gate's `bucket`,
`run` and the persisted part of `podState` move to exported `state` types so both packages
share one definition. `NewGate` takes a `state.Store`, loads the snapshot, and resets
anything not persisted.

Tests: `gate_test.go` becomes table-driven over the store constructors. The memory
constructor keeps every existing test green.

## Phase 2: the RocksDB store

Library choice, with a recommendation:

- `github.com/linxGnu/grocksdb` is the maintained binding to librocksdb. It needs cgo,
  librocksdb and its compressors in the image, and a cross C toolchain for the arm64 and
  amd64 build in the Dockerfile. The static distroless image goes away.
- `github.com/cockroachdb/pebble` is CockroachDB's pure-Go RocksDB. Same LSM design,
  bidirectionally compatible file format, WriteBatch and WAL semantics, no cgo. The build
  stays as it is.

Recommendation: implement `--state rocksdb` on pebble, and keep a `grocksdb` implementation
behind a `rocksdb_cgo` build tag only if librocksdb itself is a hard requirement. The flag
value and the on-disk format stay RocksDB's either way.

Layout: one DB, key prefixes `b/<tenant>`, `r/<run id>`, `p/<pod>`, values encoded with
`encoding/binary` behind a one-byte version. `Apply` is one WriteBatch. Writes use
`Sync: false`: a crash may lose the last few milliseconds of quota and binding updates,
which is acceptable for soft state, and avoids an fsync per request. `Load` is one
prefix iteration per key family and prunes runs idle past RunTTL.

**Worker identity.** Today `Restarted` runs on every Down to Warming transition, including
the first one at gateway start, and bumps a replica-local `boot` counter. After a restart
every persisted binding would be void, which defeats the point. Replace the counter with
an identity the worker reports: `process_start_time_seconds` from vLLM's `/metrics`
(`fakevllm` emits it too). `run.boot` becomes `run.workerStart time.Time`, `Restarted`
runs when the scraped start time changes, and the hop's engine id handling already follows
the same rule.

## Phase 3: ownership and forwarding for N > 1

- Owner of a run: `hash(run id) mod N`. A request with no run id is served locally.
- A request that lands on a non-owner is proxied to the owner over `--peers`. One
  in-cluster hop. If the ingress can hash on the request id header (Gateway API or Envoy
  `hash_policy`), set `--peers` empty and skip forwarding.
- Quota, worker slots, hop and overflow permits divide by N at startup. A later flag can
  override the share for uneven fleets.
- A replica joining or leaving changes N and reshuffles ownership. Runs owned by the wrong
  replica for one RunTTL recompute once. No migration is needed.

Exactly-global buckets, if a tenant SLA ever requires them: dragonboat Raft with pebble as
the log store, the bucket as the replicated state machine, and the same `Store` interface.

## Verification

- `go test -race ./...` over both stores.
- Restart test: admit and settle, close, reopen the same directory, `Bound` still answers,
  `TenantTokens` is continuous.
- `make demo` with two gateway processes at `--shard 0/2` and `--shard 1/2` behind the fake
  workers, golden set at concurrency 8, `orch_sticky_total` summed across replicas matches
  the single-replica run.
