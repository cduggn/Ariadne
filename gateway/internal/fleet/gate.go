package fleet

import (
	"context"
	"errors"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/cduggn/cluster-doctor/gateway/internal/decide"
)

// TenantLimit is one tenant's token bucket shape. Burst is the bucket's
// capacity and RatePerS its refill rate.
type TenantLimit struct {
	RatePerS, Burst float64
}

// Config holds everything the Gate needs to decide, reserve and queue. Pods
// is the fixed worker set in the order View reports them. Tenants maps a
// tenant name to its limit, and the key "*" is the shared bucket for every
// tenant not listed. ReserveGrace is how far before a scrape a reservation
// still counts, because a scrape may not yet reflect a request dispatched
// just before it. RunTTL is how long an idle run keeps its binding.
type Config struct {
	Pods         []string
	MaxInflight  int
	MaxQueued    int
	Policy       decide.PickPolicy
	Shed         decide.Policy
	Staleness    decide.Staleness
	StickSlack   float64
	Budgets      decide.Budgets
	Tenants      map[string]TenantLimit
	ReserveGrace time.Duration
	RunTTL       time.Duration
}

// maxRequestTokens is the largest context plus output, 32768 tokens.
const maxRequestTokens = 32768

// DefaultConfig is the shipped configuration for pods. The platform tenant
// is the doctor itself, and its bucket is sized so it never binds before
// the KV and queue gates do. Its burst fills every in-flight slot at the
// largest request size, and it refills in 4 s. The charge counts cached
// prompt tokens, so a run of chained steps spends far more than its new
// tokens. Every other tenant shares one bucket of one largest request
// refilled at 833.33 tokens/s, which is where the 429 path shows.
func DefaultConfig(pods []string) Config {
	platformBurst := float64(len(pods) * 16 * maxRequestTokens)
	return Config{
		Pods:        pods,
		MaxInflight: 16,
		MaxQueued:   32,
		Policy:      decide.PolicyPrefixThenLoad,
		Shed:        decide.DefaultPolicy,
		Staleness:   decide.DefaultStaleness,
		StickSlack:  decide.DefaultStickSlack,
		Budgets:     decide.DefaultBudgets,
		Tenants: map[string]TenantLimit{
			"platform": {RatePerS: platformBurst / 4, Burst: platformBurst},
			"*":        {RatePerS: 833.33, Burst: maxRequestTokens},
		},
		ReserveGrace: time.Second,
		RunTTL:       10 * time.Minute,
	}
}

// Scrape is one worker's telemetry as the scraper read it. At is the time
// of the read, which the Gate uses for the worker's Age and for deciding
// which reservations the scrape already reflects.
type Scrape struct {
	At            time.Time
	KVPoolTokens  int
	KVUsage       float64
	TTFTp50S      float64
	TTFTp99S      float64
	WaitingRising bool
}

// reservation is the Gate's estimate for one request dispatched to a
// worker. It counts against the worker's free KV until the request settles
// or a scrape newer than at minus ReserveGrace supersedes it.
type reservation struct {
	id     string
	tokens int
	at     time.Time
}

// podState is one worker as the Gate tracks it. Every field is guarded by
// Gate.mu. boot counts restarts, so a run binding carries the boot it was
// made under and is void once the worker restarts. meanServiceS is an EWMA
// with alpha 0.2 and is 0 until the first sample.
type podState struct {
	scrape       Scrape
	scraped      bool
	ready        bool
	boot         uint64
	reservations []reservation
	meanServiceS float64
}

// reserved sums the reservations made after since.
func (p *podState) reserved(since time.Time) int {
	total := 0
	for _, r := range p.reservations {
		if r.at.After(since) {
			total += r.tokens
		}
	}
	return total
}

// unreserve drops the reservation with the given id. A missing id is a
// no-op, because a restart may already have cleared it.
func (p *podState) unreserve(id string) {
	for i, r := range p.reservations {
		if r.id == id {
			p.reservations = append(p.reservations[:i], p.reservations[i+1:]...)
			return
		}
	}
}

// observeService folds one request's latency into meanServiceS.
func (p *podState) observeService(latency time.Duration) {
	s := latency.Seconds()
	if p.meanServiceS == 0 {
		p.meanServiceS = s
		return
	}
	p.meanServiceS = 0.2*s + 0.8*p.meanServiceS
}

// run is one agent run's binding and the usage vLLM reported for its last
// settled step. boot is the worker's boot when the binding was made.
type run struct {
	pod   string
	boot  uint64
	usage decide.RunUsage
	seen  time.Time
}

// bucket is one tenant's token bucket. It refills lazily on every read,
// starts full and never holds more than burst. It may go negative when one
// request's estimate exceeds burst, which only the shortfall correction at
// settle can cause.
type bucket struct {
	tokens, rate, burst float64
	last                time.Time
}

// refill credits the tokens earned since last and moves last to now. A
// clock that has not moved credits nothing.
func (b *bucket) refill(now time.Time) {
	if !now.After(b.last) {
		return
	}
	b.tokens = math.Min(b.burst, b.tokens+b.rate*now.Sub(b.last).Seconds())
	b.last = now
}

// add credits n tokens, capped at burst. A negative n charges.
func (b *bucket) add(n float64) {
	b.tokens = math.Min(b.burst, b.tokens+n)
}

// Ticket is one admitted request's hold on the fleet. It names the
// reservation and the queue slot that Settle returns. Only Admit builds one.
type Ticket struct {
	id       string
	run      decide.RunID
	pod      string
	tenant   string
	est      int
	resID    string
	slot     *Slot
	settled  bool
	admitted time.Time
}

// Pod is the worker the request was placed on.
func (t *Ticket) Pod() string {
	return t.pod
}

// Decision is Admit's result. Placement says where the request goes, or why
// it was refused. Est is the token estimate the decision used. Queued is
// the time the request spent waiting for a slot.
type Decision struct {
	Placement decide.Placement
	Est       int
	Queued    time.Duration
}

// Outcome is what the handler learned from the worker's response. The token
// counts and latency are meaningful only when OK is set.
type Outcome struct {
	OK                             bool
	PromptTokens, CompletionTokens int
	Latency                        time.Duration
}

// Gate is the gateway's stateful core. It owns the worker views, the
// reservations, the run table and the tenant buckets, all under one mutex,
// and the Queue behind them. The mutex may be held while reading the queue's
// Depth, and is never held while waiting in Acquire. The queue never calls
// back into the Gate.
type Gate struct {
	mu      sync.Mutex
	cfg     Config
	now     func() time.Time
	rnd     func(int) int
	queue   *Queue
	pods    map[string]*podState
	runs    map[decide.RunID]*run
	buckets map[string]*bucket
	seq     uint64
}

// NewGate builds a Gate and its Queue from cfg. now is the clock and rnd
// the random source for PolicyP2C. Every configured tenant starts with a
// full bucket. When cfg has no "*" tenant, unlisted tenants share an empty
// bucket and are refused.
func NewGate(cfg Config, now func() time.Time, rnd func(int) int) *Gate {
	g := &Gate{
		cfg:     cfg,
		now:     now,
		rnd:     rnd,
		queue:   NewQueue(cfg.Pods, cfg.MaxInflight, cfg.MaxQueued),
		pods:    make(map[string]*podState, len(cfg.Pods)),
		runs:    make(map[decide.RunID]*run),
		buckets: make(map[string]*bucket, len(cfg.Tenants)+1),
	}
	for _, name := range cfg.Pods {
		g.pods[name] = &podState{}
	}
	start := now()
	for name, lim := range cfg.Tenants {
		g.buckets[name] = &bucket{tokens: lim.Burst, rate: lim.RatePerS, burst: lim.Burst, last: start}
	}
	if _, ok := g.buckets["*"]; !ok {
		g.buckets["*"] = &bucket{last: start}
	}
	return g
}

// Observe records a worker's scrape and prunes runs idle past RunTTL. An
// unknown pod is ignored.
func (g *Gate) Observe(pod string, s Scrape) {
	g.mu.Lock()
	defer g.mu.Unlock()

	p, ok := g.pods[pod]
	if !ok {
		return
	}
	p.scrape = s
	p.scraped = true

	cutoff := g.now().Add(-g.cfg.RunTTL)
	for id, r := range g.runs {
		if r.seen.Before(cutoff) {
			delete(g.runs, id)
		}
	}
}

// SetReady records the warm-up gate's verdict for a worker. An unknown pod
// is ignored.
func (g *Gate) SetReady(pod string, ready bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if p, ok := g.pods[pod]; ok {
		p.ready = ready
	}
}

// Restarted records that a worker came back from a Down period. Its boot
// advances, so every run bound under the old boot is unbound, its
// reservations are cleared because the requests behind them are gone, and
// it stays not ready until warm-up says otherwise.
func (g *Gate) Restarted(pod string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	p, ok := g.pods[pod]
	if !ok {
		return
	}
	p.boot++
	p.ready = false
	p.reservations = nil
}

// Admit decides, reserves and queues one request. It runs Pick under the
// lock, and when a worker is chosen it charges the tenant, records the
// reservation and binds the run before releasing the lock to wait for a
// slot. A refusal from Pick charges nothing and returns a nil Ticket with
// a nil error. A refusal from the queue undoes the charge and the
// reservation and returns a 503 Placement with no Pod. A context error undoes
// the same and returns the error. The run binding outlives a queue refusal,
// because the run's history is wherever its last settled step ran.
func (g *Gate) Admit(ctx context.Context, r decide.Request) (Decision, *Ticket, error) {
	g.mu.Lock()
	start := g.now()

	var prior *decide.RunUsage
	bound := ""
	if r.Run != "" {
		if existing, ok := g.runs[r.Run]; ok {
			usage := existing.usage
			prior = &usage
			bound = g.boundPod(r.Run)
		}
	}
	est := decide.EstimateTokens(r, prior)

	b := g.bucketFor(r.Tenant)
	b.refill(start)
	fleet := decide.Fleet{
		Now:        start,
		Workers:    g.workerStates(start),
		Tenant:     decide.TenantState{AvailableTokens: b.tokens, RatePerS: b.rate},
		EstTokens:  est,
		Bound:      bound,
		Shed:       g.cfg.Shed,
		Staleness:  g.cfg.Staleness,
		StickSlack: g.cfg.StickSlack,
	}
	p := decide.Pick(r, fleet, g.cfg.Policy, g.rnd)
	if !p.Placed() {
		g.mu.Unlock()
		return Decision{Placement: p, Est: est}, nil, nil
	}

	b.add(-float64(est))
	chosen := g.pods[p.Pod]
	resID := r.ID
	if resID == "" {
		g.seq++
		resID = "res-" + strconv.FormatUint(g.seq, 10)
	}
	chosen.reservations = append(chosen.reservations, reservation{id: resID, tokens: est, at: start})
	if r.Run != "" {
		existing, ok := g.runs[r.Run]
		if !ok {
			existing = &run{}
			g.runs[r.Run] = existing
		}
		existing.pod = p.Pod
		existing.boot = chosen.boot
		existing.seen = start
	}
	g.mu.Unlock()

	slot, err := g.queue.Acquire(ctx, p.Pod, r.Priority, r.Deadline)
	if err != nil {
		g.mu.Lock()
		g.bucketFor(r.Tenant).add(float64(est))
		chosen.unreserve(resID)
		g.mu.Unlock()

		var refusal *Refusal
		if errors.As(err, &refusal) {
			p.Pod = ""
			p.Verdict = decide.Verdict{Shed: true, Code: 503, Reason: refusal.Reason, RetryAfter: time.Second}
			return Decision{Placement: p, Est: est}, nil, nil
		}
		return Decision{Placement: p, Est: est}, nil, err
	}

	t := &Ticket{
		id:       r.ID,
		run:      r.Run,
		pod:      p.Pod,
		tenant:   r.Tenant,
		est:      est,
		resID:    resID,
		slot:     slot,
		admitted: start,
	}
	return Decision{Placement: p, Est: est, Queued: g.now().Sub(start)}, t, nil
}

// Settle closes a Ticket once the worker has answered. It drops the
// reservation, corrects the tenant's charge to the real usage, or refunds
// it fully when the request failed, records the run's usage for its next
// step's estimate, folds the latency into the worker's mean service time
// and releases the queue slot. A second call is a no-op, so a handler that
// settles on every exit path cannot refund twice.
func (g *Gate) Settle(t *Ticket, o Outcome, bodyBytes int) {
	g.mu.Lock()
	if t.settled {
		g.mu.Unlock()
		return
	}
	t.settled = true

	now := g.now()
	p := g.pods[t.pod]
	p.unreserve(t.resID)
	b := g.bucketFor(t.tenant)
	b.refill(now)
	if o.OK {
		b.add(float64(t.est - o.PromptTokens - o.CompletionTokens))
		if existing, ok := g.runs[t.run]; ok && t.run != "" {
			existing.usage = decide.RunUsage{
				PromptTokens:     o.PromptTokens,
				CompletionTokens: o.CompletionTokens,
				BodyBytes:        bodyBytes,
			}
			existing.seen = now
		}
		p.observeService(o.Latency)
	} else {
		b.add(float64(t.est))
	}
	g.mu.Unlock()

	t.slot.Release()
}

// View returns the per-worker states Admit would build right now, in
// cfg.Pods order.
func (g *Gate) View() []decide.WorkerState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.workerStates(g.now())
}

// Bound returns the worker a run is bound to, or "" when the run is unknown
// or its worker has restarted since the binding.
func (g *Gate) Bound(id decide.RunID) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.boundPod(id)
}

// boundPod is Bound under the lock.
func (g *Gate) boundPod(id decide.RunID) string {
	r, ok := g.runs[id]
	if !ok {
		return ""
	}
	if p, ok := g.pods[r.pod]; !ok || p.boot != r.boot {
		return ""
	}
	return r.pod
}

// bucketFor returns the tenant's own bucket, or the shared "*" bucket for
// a tenant not in cfg.Tenants. It must be called under the lock.
func (g *Gate) bucketFor(tenant string) *bucket {
	if b, ok := g.buckets[tenant]; ok {
		return b
	}
	return g.buckets["*"]
}

// workerStates builds one WorkerState per configured pod at now. A worker
// never scraped reports Age equal to DownAfter, so Pick treats it as Down.
// ReservedTokens counts the reservations the last scrape cannot yet
// reflect, those made after the scrape minus ReserveGrace. It must be called
// under the lock.
func (g *Gate) workerStates(now time.Time) []decide.WorkerState {
	out := make([]decide.WorkerState, 0, len(g.cfg.Pods))
	for _, name := range g.cfg.Pods {
		p := g.pods[name]
		age := g.cfg.Staleness.DownAfter
		if p.scraped {
			age = now.Sub(p.scrape.At)
		}
		inFlight, interactive, batch := g.queue.Depth(name)
		out = append(out, decide.WorkerState{
			View: decide.WorkerView{
				Pod:            name,
				KVPoolTokens:   p.scrape.KVPoolTokens,
				KVUsage:        p.scrape.KVUsage,
				ReservedTokens: p.reserved(p.scrape.At.Add(-g.cfg.ReserveGrace)),
				InFlight:       inFlight,
				Queued:         interactive + batch,
				MaxInflight:    g.cfg.MaxInflight,
				MeanServiceS:   p.meanServiceS,
				TTFTp50S:       p.scrape.TTFTp50S,
				TTFTp99S:       p.scrape.TTFTp99S,
				WaitingRising:  p.scrape.WaitingRising,
			},
			Ready: p.ready && p.scraped,
			Age:   age,
		})
	}
	return out
}
