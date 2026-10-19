# Gateway state backend: in-memory by default, pebble per shard for more than one replica

Status: phases 1 and 2 built on 2026-10-18 (`gateway/internal/state`, `--state pebble`);
phase 3 is a plan. Companion to D-53 (overflow) and the "one replica, because the run table
lives in memory" line in `design/decisions.md`.

## What an embedded store does and does not buy

RocksDB, and pebble with it, is an embedded store. One process owns one directory. It does
not share state between gateway replicas. What it gives a replica is a durable, crash-safe copy of its own state,
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
| Per-worker gateway-side state: `podState` (scrape, ready, started, reservations, meanServiceS), `Restarted`, `workerStates` | `gateway/internal/fleet/gate.go` | 95-134, 290-301, 561-589 |
| Per-worker queue: `Queue`, `worker` (inFlight, lanes), `waiter`, `Slot` | `gateway/internal/fleet/queue.go` | 37-83 |
| Scraper and warm-up: `member` (phase, ring, passes), `Fleet` | `gateway/internal/fleet/worker.go` | 104-147 |
| Hop permits: `Hopper.slots` | `gateway/internal/hop/hopper.go` | 42, 59 |
| Overflow circuit: `up`, `probes`, `slots` | `gateway/internal/overflow/overflow.go` | 107-109 |
| Prometheus registry and the fleet collector | `gateway/internal/metrics/metrics.go` | 72-93, 296-306 |
| Callers of the Gate | `gateway/internal/serve/server.go` | 177, 197, 246, 254, 267 |
| Flags and wiring | `gateway/cmd/gateway/main.go` | 36-66, 95-100 |

## What is persisted, what is divided, what stays local

| State | Multi-replica treatment | Persisted in pebble |
|---|---|---|
| Run table | Sharded by run id. Owner replica only. | Yes |
| Tenant buckets | Divided: each replica gets rate/N and burst/N. | Yes (the replica's share) |
| Pod process start, mean service EWMA | Replica-local, but kept across restart | Yes |
| Reservations | Replica-local. Never persisted: the requests behind them die with the process. | No |
| Queue, waiters, in-flight | Replica-local. max_inflight/N and max_queued/N per replica. The scrape is the truth for the worker's real load across replicas. | No |
| Scrape, ready, warm-up ring | Replica-local, rebuilt within one scrape window | No |
| Hop permits, overflow circuit | Replica-local, max_inflight/N | No |
| Metrics | Replica-local, add a `replica` label, sum in PromQL | No |

## Configuration

Every flag keeps the `GW_<NAME>` environment default. A gateway started with no new flags
behaves exactly as today.

```
--state memory|pebble    GW_STATE       default memory
--state-dir PATH         GW_STATE_DIR   pebble only, the store's directory
--shard I/N              GW_SHARD       phase 3. default 0/1: this replica owns every run
--peers pod=url,...      GW_PEERS       phase 3. required when N > 1 unless the ingress hashes on the request id
```

Single instance is the default and needs nothing. `--state pebble` alone is a single
instance that survives restarts, useful on its own for rolling deploys. `--shard` with
N > 1 divides quota and worker slots and turns on forwarding.

Under Kubernetes the gateway is a StatefulSet with one PVC per replica for the state
directory (`deploy/k8s/gateway.yaml`, `GW_STATE=pebble`, `GW_STATE_DIR=/var/lib/gateway`).
Phase 3 builds `GW_SHARD` from the pod ordinal in the hostname and the replica count.

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
    Buckets  map[string]Bucket
    Runs     map[decide.RunID]Run
    DropRuns []decide.RunID
    Pods     map[string]Pod
}
```

`state.Memory` implements `Load` as empty and `Apply` as a no-op. The Gate's `bucket`,
`run` and the persisted part of `podState` move to exported `state` types so both packages
share one definition. `NewGate` takes a `state.Store`, loads the snapshot, and resets
anything not persisted.

Tests: `gate_test.go` becomes table-driven over the store constructors. The memory
constructor keeps every existing test green.

## Phase 2: the pebble store

The library is `github.com/cockroachdb/pebble/v2` v2.1.7, CockroachDB's pure-Go RocksDB.
It has the same LSM design, WriteBatch and WAL semantics, and no cgo, so the static
distroless multi-arch build stays as it is. Decided 2026-10-18. `github.com/linxGnu/grocksdb` (cgo, librocksdb in the
image, cross C toolchain) is not planned; it could sit behind a `rocksdb_cgo` build tag
later if librocksdb itself ever becomes a requirement.

The layout in `gateway/internal/state/pebble.go` is one DB with the key prefixes `b/<tenant>`,
`r/<run id>` and `p/<pod>`, and values of one format byte then JSON. `Apply` is one pebble
Batch committed with `NoSync`. A crash may lose the last few milliseconds of quota and binding
updates, which is acceptable for soft state, and the gateway avoids an fsync per request. `Load` is one bounded prefix
iteration per key family; an entry that fails to decode is reported and the rest is used.
`NewGate` prunes kept runs idle past RunTTL and caps a kept balance at the configured burst.
Pebble's own log lines go to the gateway's slog logger at debug level.

**Worker identity.** Before this change `Restarted` ran on every Down to Warming transition,
including the first one at gateway start, and bumped a replica-local `boot` counter. After a
restart every persisted binding would have been void, which defeats the point. The identity
is now the one the worker reports: `process_start_time_seconds` from vLLM's `/metrics`, which
every real body captured in `metrics/` carries, and which `fakevllm` emits too. `podState.started`
and `run.started` are that time; `Observe` voids bindings, clears reservations and waits for
warm-up when it changes, and a scrape outage with the same start keeps them. A worker that
reports no start keeps the old rule: `Restarted` moves its start to now on each Warming entry.

## Redundancy: replication sits above the embedded store

An embedded store is redundant the way RocksDB is redundant under CockroachDB, TiKV and
Kafka Streams. The owners replicate the write stream between processes, never the store
itself. There are three levels, cheapest first.

1. **Reattached disk.** A StatefulSet with one PVC per replica. A crashed pod reschedules
   and reopens its directory. Nothing is lost, and nothing is served during the gap. This
   comes free with phase 2.
2. **Standby mirror, async.** This is the chosen level. The owner of shard i commits each `Batch` locally and
   streams it to its successor, shard i+1 mod N, which applies it under a `s/<shard>/`
   prefix in its own pebble directory. When owner i is unreachable, the forwarder in
   phase 3 sends shard i's requests to i+1, which promotes the mirror and serves from it.
   When i returns it reloads from i+1 and takes the shard back. Lag is bounded by one
   in-cluster round trip, which matches the `Sync: false` loss window already accepted.
   It reuses `Apply(Batch)` and `--peers`. This is Kafka Streams' standby-task model.
3. **Raft per shard.** Dragonboat with pebble as the log store, buckets and runs as the
   replicated state machine, three copies, leader failover with no loss. It is exact and
   heavy. It sits behind the same `Store` interface if a tenant SLA ever requires exact quota.

Level 2 is enough because the state is soft. A lost binding costs one recompute, and a lost
bucket refills within seconds.

## Phase 3: ownership and forwarding for N > 1

- Owner of a run: `hash(run id) mod N`. A request with no run id is served locally.
- A request that lands on a non-owner is proxied to the owner over `--peers`. One
  in-cluster hop. If the ingress can hash on the request id header (Gateway API or Envoy
  `hash_policy`), set `--peers` empty and skip forwarding.
- Quota, worker slots, hop and overflow permits divide by N at startup. A later flag can
  override the share for uneven fleets.
- A replica joining or leaving changes N and reshuffles ownership. Runs owned by the wrong
  replica for one RunTTL recompute once. No migration is needed.
- Each owner mirrors its batches to its successor, as in the redundancy section. Failover
  is the forwarder's retry to i+1 after a failed call to i.

## Verification

- `go test -race ./...` over both stores.
- Restart test: admit and settle, close, reopen the same directory, `Bound` still answers,
  `TenantTokens` is continuous.
- `make demo` with two gateway processes at `--shard 0/2` and `--shard 1/2` behind the fake
  workers, golden set at concurrency 8, `orch_sticky_total` summed across replicas matches
  the single-replica run.
