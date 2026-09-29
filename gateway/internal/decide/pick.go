package decide

import (
	"strconv"
	"strings"
	"time"
)

// PickPolicy selects how Pick breaks a tie among admitted workers. It is the
// course's "policy" argument to pick(req, workers, *, policy): which worker
// wins when more than one clears ShouldShed.
type PickPolicy string

const (
	// PolicyPrefixThenLoad keeps a request's bound worker while it is
	// admitted and not much busier than the alternatives, and falls back to
	// the least loaded admitted worker otherwise. This is the gateway's
	// shipped default: it protects the resident KV prefix without pinning a
	// request to a worker that has fallen far behind.
	PolicyPrefixThenLoad PickPolicy = "prefix_then_load"
	// PolicyLeastLoaded always takes the least loaded admitted worker,
	// ignoring stickiness. It is the stickiness A/B's control arm.
	PolicyLeastLoaded PickPolicy = "least_loaded"
	// PolicyP2C is power-of-two-choices: sample two admitted workers at
	// random and take the less loaded. At two workers it behaves the same
	// as PolicyLeastLoaded.
	PolicyP2C PickPolicy = "p2c"
)

// ParsePickPolicy parses a policy flag or header value. Matching is exact
// after trimming and folding case. An unrecognised value returns ("", false)
// rather than a silently wrong default: a policy typo should fail startup,
// not change routing.
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

// Sticky is the metric label value for a placement's outcome relative to the
// request's bound worker, orch_sticky_total{outcome}. Pick always reports
// one, even when it sheds, so the stickiness A/B can be scored from the
// metric alone.
type Sticky string

const (
	// StickyNone is reported when the request carries no run, so
	// stickiness never applied.
	StickyNone Sticky = "none"
	// StickyNew is reported when the run exists but has no worker bound to
	// it yet: this placement, if it succeeds, creates the binding.
	StickyNew Sticky = "new"
	// StickyHit is reported when the bound worker was chosen.
	StickyHit Sticky = "hit"
	// StickyBrokenLoad is reported when the bound worker was admitted but
	// too far behind the alternatives to keep, past Fleet.StickSlack.
	StickyBrokenLoad Sticky = "broken_load"
	// StickyBrokenShed is reported when the bound worker was a candidate
	// but ShouldShed refused it.
	StickyBrokenShed Sticky = "broken_shed"
	// StickyBrokenGone is reported when the bound worker was never a
	// candidate: it is not Ready, it is Down, or the staleness floor
	// dropped it.
	StickyBrokenGone Sticky = "broken_gone"
)

// WorkerState is one worker as Pick sees it: its scraped view, whether the
// warm-up gate has let it out of Warming, and how long it has been since its
// own last successful scrape. Age is tracked per worker, not read from a
// single fleet-wide clock, because two workers go stale independently.
type WorkerState struct {
	View  WorkerView
	Ready bool
	Age   time.Duration
}

// Staleness is the tunable staleness rule: the two age lines that separate
// fresh, stale and Down, and the fraction of the eligible fleet's total
// MaxInflight that must stay fresh before any stale worker is dropped rather
// than kept on its last-known telemetry.
type Staleness struct {
	StaleAfter    time.Duration
	DownAfter     time.Duration
	CapacityFloor float64
}

// DefaultStaleness is the gateway's shipped staleness rule: stale from 2s to
// 10s since the last scrape, Down after that, and a floor requiring 0.75 of
// the eligible fleet's capacity to stay fresh before any stale worker is
// dropped.
var DefaultStaleness = Staleness{StaleAfter: 2 * time.Second, DownAfter: 10 * time.Second, CapacityFloor: 0.75}

// Load is the course's score_load: in-flight plus queued depth as a fraction
// of the worker's own cap, plus the fraction of its KV pool already in use.
// The gateway substitutes its own in-flight and queue counts for vLLM's
// running and waiting, and uses FreeRatio, which is already net of the
// gateway's own reservations, in place of a raw scraped KV usage figure. A
// MaxInflight of 0 or less (an unconfigured worker) uses 1 for the first
// term's denominator instead of dividing by zero.
func (w WorkerView) Load() float64 {
	denom := w.MaxInflight
	if denom <= 0 {
		denom = 1
	}
	return float64(w.InFlight+w.Queued)/float64(denom) + (1 - w.FreeRatio())
}

// DefaultStickSlack is the default Fleet.StickSlack: 0.25 load units, four of
// sixteen in-flight slots at the gateway's default MaxInflight.
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

// Placement is Pick's result: the course's pick(req, workers, *, policy) ->
// Worker | Shed. Pod is "" exactly when the request was shed, in which case
// Verdict explains why; Verdict is the zero Verdict exactly when Pod is set.
// Sticky and Unknown are metric labels, meaningful in both cases.
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

// reasonOrder is ShouldShed's gate order, tenant_tokens first and
// p99_spread last. When candidate workers refuse for different reasons, the
// lowest gate wins, so a 429 always outranks a simultaneous 503.
var reasonOrder = map[Reason]int{
	ReasonTenantTokens: 0,
	ReasonKVFree:       1,
	ReasonTimeoutQueue: 2,
	ReasonP99Spread:    3,
}

// Pick is the course's pick(req, workers, *, policy) -> Worker | Shed. It
// filters to Ready, non-Down workers, applies the staleness floor, admits
// candidates through ShouldShed exactly as the simulator's H4 check does so
// routing and admission never disagree, and then breaks the tie among
// admitted workers per policy. rnd must return a value in [0, n) and is
// called only by PolicyP2C, and only when more than one worker is admitted.
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

// eligibleWorkers keeps Ready workers whose own scrape is recent enough not
// to be Down. A worker with no scrape for downAfter is excluded regardless
// of what ShouldShed might otherwise say about it.
func eligibleWorkers(workers []WorkerState, downAfter time.Duration) []WorkerState {
	out := make([]WorkerState, 0, len(workers))
	for _, w := range workers {
		if w.Ready && w.Age < downAfter {
			out = append(out, w)
		}
	}
	return out
}

// dropStaleBelowFloor applies the staleness floor: a stale worker (age
// between StaleAfter and DownAfter) is dropped only when the fleet can
// afford it, that is when the fresh workers left would still hold at least
// CapacityFloor of the eligible fleet's total MaxInflight. Otherwise every
// stale worker stays, to be picked on its last-known telemetry.
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

// admitCandidates runs ShouldShed against every remaining worker, with the
// resident exemption applied only to the worker this request's run is
// already bound to. It returns the admitted workers and, separately, every
// refusal, so Pick can report the highest-priority gate when nothing admits.
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

// worstRefusal returns the refusal whose gate comes first in ShouldShed's
// order, so a 429 always outranks a simultaneous 503 even when different
// workers refused for different reasons.
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

// stickyOnRefusal is the Sticky outcome when Pick places nothing, whether
// because no worker survived filtering or every candidate was refused. A
// bound worker still present in remaining was a candidate and was refused
// (broken_shed); one that had already dropped out of remaining was never a
// candidate (broken_gone).
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

// choose breaks the tie among admitted workers per policy. An unrecognised
// policy value falls back to PolicyPrefixThenLoad, the gateway's shipped
// default, the same way an unrecognised header value fails safe elsewhere in
// this package.
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

// chooseLeastLoaded takes the least loaded admitted worker regardless of
// Bound. Sticky is reported purely for metrics: hit when the choice happens
// to equal Bound, otherwise the same classification choosePrefixThenLoad
// would have used to decide whether to keep the binding.
func chooseLeastLoaded(r Request, f Fleet, remaining, admitted []WorkerState) (WorkerState, Sticky) {
	chosen := leastLoadedOf(admitted)
	return chosen, classifySticky(f.Bound, chosen.View.Pod, r.Run, remaining, admitted)
}

// chooseP2C samples two distinct admitted workers with rnd and takes the
// less loaded, tie broken by pod name ascending. With only one admitted
// worker it is taken directly and rnd is never called. Sticky is reported
// the same way as chooseLeastLoaded, purely for metrics.
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
// StickSlack load units of the best alternative. Otherwise it falls back to
// least loaded, and the Sticky outcome says why the binding broke. A Bound
// over the slack can never be the least loaded, so the fallback always
// reports broken_load, broken_shed or broken_gone, never hit.
func choosePrefixThenLoad(r Request, f Fleet, remaining, admitted []WorkerState) (WorkerState, Sticky) {
	if bound, ok := findByPod(admitted, f.Bound); ok {
		others := excludePod(admitted, f.Bound)
		if len(others) == 0 || bound.View.Load() <= minLoad(others)+f.StickSlack {
			return bound, StickyHit
		}
	}
	return chooseLeastLoaded(r, f, remaining, admitted)
}

// classifySticky is the Sticky value for a chosen worker relative to Bound,
// used by policies whose choice does not depend on stickiness.
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

// leastLoadedOf returns the worker with the lowest Load, ties broken by pod
// name ascending so the choice is deterministic.
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

// minLoad returns the lowest Load in ws. Unlike leastLoadedOf it discards
// identity: choosePrefixThenLoad only needs the value to compare Bound
// against.
func minLoad(ws []WorkerState) float64 {
	m := ws[0].View.Load()
	for _, w := range ws[1:] {
		if l := w.View.Load(); l < m {
			m = l
		}
	}
	return m
}

// findByPod returns the worker with the given pod name and whether it was
// found.
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
