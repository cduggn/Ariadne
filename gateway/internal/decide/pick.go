package decide

import (
	"strconv"
	"strings"
	"time"
)

// PickPolicy selects which admitted worker Pick chooses when more than one
// passes ShouldShed. It is the policy argument of the course's
// pick(req, workers, *, policy).
type PickPolicy string

const (
	// PolicyPrefixThenLoad keeps a request on its bound worker while that
	// worker is admitted and not much busier than the others. Otherwise it
	// takes the least loaded admitted worker. It is the default, because it
	// keeps the run's cached history without pinning the run to a worker
	// that has fallen far behind.
	PolicyPrefixThenLoad PickPolicy = "prefix_then_load"
	// PolicyLeastLoaded takes the least loaded admitted worker and ignores
	// stickiness. It is the control arm of the stickiness A/B.
	PolicyLeastLoaded PickPolicy = "least_loaded"
	// PolicyP2C samples two admitted workers at random and takes the less
	// loaded one. With two workers it behaves like PolicyLeastLoaded.
	PolicyP2C PickPolicy = "p2c"
)

// ParsePickPolicy parses a policy flag value, trimming spaces and ignoring
// case. An unknown value returns ("", false), so a typo fails startup instead
// of changing routing.
func ParsePickPolicy(s string) (PickPolicy, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(PolicyPrefixThenLoad):
		return PolicyPrefixThenLoad, true
	case string(PolicyLeastLoaded):
		return PolicyLeastLoaded, true
	case string(PolicyP2C):
		return PolicyP2C, true
	default:
		return "", false
	}
}

// Sticky is the outcome label on orch_sticky_total{outcome}. It records what
// happened relative to the request's bound worker. Pick sets it even when it
// sheds, so the metric alone can score the stickiness A/B.
type Sticky string

const (
	// StickyNone means the request has no run, so stickiness did not apply.
	StickyNone Sticky = "none"
	// StickyNew means the run has no bound worker yet. A successful placement
	// creates the binding.
	StickyNew Sticky = "new"
	// StickyHit means Pick chose the bound worker.
	StickyHit Sticky = "hit"
	// StickyBrokenLoad means the bound worker passed ShouldShed but was more
	// than Fleet.StickSlack busier than the best other worker.
	StickyBrokenLoad Sticky = "broken_load"
	// StickyBrokenShed means the bound worker was a candidate and ShouldShed
	// refused it.
	StickyBrokenShed Sticky = "broken_shed"
	// StickyBrokenGone means the bound worker was never a candidate, because
	// it was not Ready, it was Down, or the staleness floor dropped it.
	StickyBrokenGone Sticky = "broken_gone"
)

// WorkerState is one worker as Pick sees it. It holds the worker's view,
// whether the warm-up gate has marked it Ready, and the time since its own
// last successful scrape. Each worker has its own Age, because workers go
// stale independently.
type WorkerState struct {
	View  WorkerView
	Ready bool
	Age   time.Duration
}

// Staleness holds the staleness rule. StaleAfter and DownAfter separate
// fresh, stale and Down workers. CapacityFloor is the share of the eligible
// fleet's MaxInflight that must stay fresh before Pick drops stale workers.
// Below it, stale workers stay and Pick uses their last-known telemetry.
type Staleness struct {
	StaleAfter    time.Duration
	DownAfter     time.Duration
	CapacityFloor float64
}

// DefaultStaleness marks a worker stale 2s after its last scrape and Down
// after 10s. Stale workers are dropped only while 0.75 of capacity stays
// fresh.
var DefaultStaleness = Staleness{StaleAfter: 2 * time.Second, DownAfter: 10 * time.Second, CapacityFloor: 0.75}

// Load is the course's score_load. It adds in-flight plus queued requests as
// a fraction of the worker's cap to the fraction of KV in use. The gateway's
// own counts replace vLLM's running and waiting, and KV in use comes from
// FreeRatio, which includes the gateway's reservations. An unconfigured
// worker with MaxInflight 0 divides by 1 instead of 0.
func (w WorkerView) Load() float64 {
	denom := w.MaxInflight
	if denom <= 0 {
		denom = 1
	}
	return float64(w.InFlight+w.Queued)/float64(denom) + (1 - w.FreeRatio())
}

// DefaultStickSlack is 0.25 load units, which is four of the default sixteen
// in-flight slots.
const DefaultStickSlack = 0.25

// Fleet is everything Pick reads about a request's candidate workers.
type Fleet struct {
	Now        time.Time
	Workers    []WorkerState
	Tenant     TenantState
	EstTokens  int
	Bound      string // pod this request's run is bound to, "" if none
	Shed       Policy // admission thresholds passed through to ShouldShed
	Staleness  Staleness
	StickSlack float64 // load units the bound worker may exceed the best other
}

// Placement is Pick's result, the Worker or Shed of the course's
// pick(req, workers, *, policy). Pod is "" when Pick shed the request, and
// Verdict then says why. When Pod is set, Verdict is zero. Sticky and Unknown
// are metric labels and are set in both cases.
type Placement struct {
	Pod     string  // "" when shed
	Verdict Verdict // zero when placed
	Policy  PickPolicy
	Sticky  Sticky
	Unknown bool // chosen worker's telemetry was stale
}

// Placed reports whether Pick chose a worker, as opposed to shedding.
func (p Placement) Placed() bool {
	return p.Pod != ""
}

// reasonOrder is ShouldShed's gate order, from tenant_tokens to p99_spread.
// When candidates refuse for different reasons, the earliest gate wins, so a
// 429 outranks a 503.
var reasonOrder = map[Reason]int{
	ReasonTenantTokens: 0,
	ReasonKVFree:       1,
	ReasonTimeoutQueue: 2,
	ReasonP99Spread:    3,
}

// Pick is the course's pick(req, workers, *, policy). It keeps Ready workers
// that are not Down and applies the staleness floor. It then runs ShouldShed
// on each candidate, as the simulator's H4 check does, so routing and
// admission cannot disagree. Last, it chooses among the admitted workers by
// policy. rnd must return a value in [0, n). Only PolicyP2C calls it, and
// only when more than one worker is admitted.
func Pick(r Request, f Fleet, p PickPolicy, rnd func(n int) int) Placement {
	eligible := eligibleWorkers(f.Workers, f.Staleness.DownAfter)
	remaining := dropStaleBelowFloor(eligible, f.Staleness)

	if len(remaining) == 0 {
		return Placement{
			Verdict: Verdict{
				Shed:       true,
				Code:       503,
				Reason:     ReasonNoEligiblePod,
				RetryAfter: 2 * time.Second,
				Detail: strconv.Itoa(len(f.Workers)) + " workers, " + strconv.Itoa(len(eligible)) +
					" ready, none survive staleness filtering",
			},
			Policy: p,
			Sticky: stickyOnRefusal(r.Run, f.Bound, nil),
		}
	}

	admitted, refusals := admitCandidates(r, f, remaining)
	if len(admitted) == 0 {
		return Placement{
			Verdict: worstRefusal(refusals),
			Policy:  p,
			Sticky:  stickyOnRefusal(r.Run, f.Bound, remaining),
		}
	}

	chosen, sticky := choose(p, r, f, remaining, admitted, rnd)
	return Placement{
		Pod:     chosen.View.Pod,
		Policy:  p,
		Sticky:  sticky,
		Unknown: chosen.Age >= f.Staleness.StaleAfter,
	}
}

// eligibleWorkers keeps Ready workers whose last scrape is newer than
// downAfter. A Down worker is excluded before ShouldShed runs.
func eligibleWorkers(workers []WorkerState, downAfter time.Duration) []WorkerState {
	out := make([]WorkerState, 0, len(workers))
	for _, w := range workers {
		if w.Ready && w.Age < downAfter {
			out = append(out, w)
		}
	}
	return out
}

// dropStaleBelowFloor applies the staleness floor. It drops stale workers
// only if the fresh ones hold at least CapacityFloor of the eligible fleet's
// MaxInflight. Otherwise the stale workers stay, and Pick uses their
// last-known telemetry.
func dropStaleBelowFloor(eligible []WorkerState, st Staleness) []WorkerState {
	var totalCap, freshCap int
	hasStale := false
	for _, w := range eligible {
		totalCap += w.View.MaxInflight
		if w.Age >= st.StaleAfter {
			hasStale = true
		} else {
			freshCap += w.View.MaxInflight
		}
	}
	if !hasStale || totalCap <= 0 || float64(freshCap) < st.CapacityFloor*float64(totalCap) {
		return eligible
	}

	fresh := make([]WorkerState, 0, len(eligible))
	for _, w := range eligible {
		if w.Age < st.StaleAfter {
			fresh = append(fresh, w)
		}
	}
	return fresh
}

// admitCandidates runs ShouldShed on every remaining worker. Only the run's
// bound worker gets the resident exemption. It returns the admitted workers
// and the refusals, so Pick can report the earliest gate when none admit.
func admitCandidates(r Request, f Fleet, remaining []WorkerState) (admitted []WorkerState, refusals []Verdict) {
	for _, w := range remaining {
		snap := Snap{
			Now:       f.Now,
			Tenant:    f.Tenant,
			Worker:    w.View,
			Resident:  f.Bound != "" && w.View.Pod == f.Bound,
			EstTokens: f.EstTokens,
			Policy:    f.Shed,
		}
		if v := ShouldShed(r, snap); v.Shed {
			refusals = append(refusals, v)
		} else {
			admitted = append(admitted, w)
		}
	}
	return admitted, refusals
}

// worstRefusal returns the refusal from the earliest gate in ShouldShed's
// order, so a 429 outranks a 503 from another worker.
func worstRefusal(refusals []Verdict) Verdict {
	best := refusals[0]
	bestOrder := reasonOrder[best.Reason]
	for _, v := range refusals[1:] {
		if order := reasonOrder[v.Reason]; order < bestOrder {
			best = v
			bestOrder = order
		}
	}
	return best
}

// stickyOnRefusal is the Sticky outcome when Pick places nothing. A bound
// worker in remaining was a candidate that ShouldShed refused, so the
// outcome is broken_shed. A bound worker missing from remaining was never a
// candidate, so the outcome is broken_gone.
func stickyOnRefusal(run RunID, bound string, remaining []WorkerState) Sticky {
	if bound == "" {
		if run == "" {
			return StickyNone
		}
		return StickyNew
	}
	if containsPod(remaining, bound) {
		return StickyBrokenShed
	}
	return StickyBrokenGone
}

// choose picks among admitted workers by policy. An unknown policy value
// uses PolicyPrefixThenLoad, the default, as unknown header values fall back
// to safe defaults elsewhere in this package.
func choose(p PickPolicy, r Request, f Fleet, remaining, admitted []WorkerState, rnd func(int) int) (WorkerState, Sticky) {
	switch p {
	case PolicyLeastLoaded:
		return chooseLeastLoaded(r, f, remaining, admitted)
	case PolicyP2C:
		return chooseP2C(r, f, remaining, admitted, rnd)
	default:
		return choosePrefixThenLoad(r, f, remaining, admitted)
	}
}

// chooseLeastLoaded takes the least loaded admitted worker and ignores
// Bound. It still reports Sticky for the metric. The outcome is hit when the
// choice equals Bound, and otherwise says why the binding was not kept.
func chooseLeastLoaded(r Request, f Fleet, remaining, admitted []WorkerState) (WorkerState, Sticky) {
	chosen := leastLoadedOf(admitted)
	return chosen, classifySticky(f.Bound, chosen.View.Pod, r.Run, remaining, admitted)
}

// chooseP2C samples two distinct admitted workers with rnd and takes the
// less loaded one, breaking ties by pod name. With one admitted worker it
// takes that worker and never calls rnd. It reports Sticky as
// chooseLeastLoaded does.
func chooseP2C(r Request, f Fleet, remaining, admitted []WorkerState, rnd func(int) int) (WorkerState, Sticky) {
	chosen := admitted[0]
	if n := len(admitted); n > 1 {
		i := rnd(n)
		j := rnd(n - 1)
		if j >= i {
			j++
		}
		a, b := admitted[i], admitted[j]
		chosen = a
		if lb, la := b.View.Load(), a.View.Load(); lb < la || (lb == la && b.View.Pod < a.View.Pod) {
			chosen = b
		}
	}
	return chosen, classifySticky(f.Bound, chosen.View.Pod, r.Run, remaining, admitted)
}

// choosePrefixThenLoad keeps Bound while it is admitted and within
// StickSlack load units of the best other worker. Otherwise it takes the
// least loaded worker, and Sticky says why the binding broke. A Bound over
// the slack cannot be the least loaded, so that fallback never reports hit.
func choosePrefixThenLoad(r Request, f Fleet, remaining, admitted []WorkerState) (WorkerState, Sticky) {
	if bound, ok := findByPod(admitted, f.Bound); ok {
		others := excludePod(admitted, f.Bound)
		if len(others) == 0 || bound.View.Load() <= minLoad(others)+f.StickSlack {
			return bound, StickyHit
		}
	}
	return chooseLeastLoaded(r, f, remaining, admitted)
}

// classifySticky returns the Sticky outcome of a chosen worker relative to
// Bound, for policies that ignore stickiness.
func classifySticky(bound, chosenPod string, run RunID, remaining, admitted []WorkerState) Sticky {
	if bound == "" {
		if run == "" {
			return StickyNone
		}
		return StickyNew
	}
	if chosenPod == bound {
		return StickyHit
	}
	if containsPod(admitted, bound) {
		return StickyBrokenLoad
	}
	if containsPod(remaining, bound) {
		return StickyBrokenShed
	}
	return StickyBrokenGone
}

// leastLoadedOf returns the worker with the lowest Load. Ties go to the
// lower pod name, so the choice is deterministic.
func leastLoadedOf(ws []WorkerState) WorkerState {
	best := ws[0]
	bestLoad := best.View.Load()
	for _, w := range ws[1:] {
		if l := w.View.Load(); l < bestLoad || (l == bestLoad && w.View.Pod < best.View.Pod) {
			best = w
			bestLoad = l
		}
	}
	return best
}

// minLoad returns the lowest Load in ws. choosePrefixThenLoad compares Bound
// against this value and does not need the worker.
func minLoad(ws []WorkerState) float64 {
	m := ws[0].View.Load()
	for _, w := range ws[1:] {
		if l := w.View.Load(); l < m {
			m = l
		}
	}
	return m
}

// findByPod returns the worker with the given pod name, and false if there
// is none.
func findByPod(ws []WorkerState, pod string) (WorkerState, bool) {
	for _, w := range ws {
		if w.View.Pod == pod {
			return w, true
		}
	}
	return WorkerState{}, false
}

// excludePod returns ws without the worker with the given pod name.
func excludePod(ws []WorkerState, pod string) []WorkerState {
	out := make([]WorkerState, 0, len(ws))
	for _, w := range ws {
		if w.View.Pod != pod {
			out = append(out, w)
		}
	}
	return out
}

// containsPod reports whether ws holds a worker with the given pod name.
func containsPod(ws []WorkerState, pod string) bool {
	for _, w := range ws {
		if w.View.Pod == pod {
			return true
		}
	}
	return false
}
