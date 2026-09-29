package decide

import (
	"math"
	"strconv"
	"time"
)

// Reason is the metric label value for a shed decision, the "reason" label on
// orch_shed_total{reason,code}. The values are the class-10 shed reasons. Pick
// uses the same type and adds only no_eligible_pod.
type Reason string

const (
	// ReasonTenantTokens is charged when a request would overdraw its
	// tenant's token bucket.
	ReasonTenantTokens Reason = "tenant_tokens"
	// ReasonKVFree is charged when a worker's effective free KV would fall
	// below the admission line (or, for a resident run, its floor).
	ReasonKVFree Reason = "kv_free"
	// ReasonTimeoutQueue is charged when a request's projected wait behind
	// the worker's queue would carry it past its deadline.
	ReasonTimeoutQueue Reason = "timeout_queue"
	// ReasonP99Spread is charged when a batch worker's tail latency has
	// diverged from its median while its queue is still growing.
	ReasonP99Spread Reason = "p99_spread"
	// ReasonNoEligiblePod is charged by Pick, not ShouldShed, when no
	// worker survives the Ready and staleness filters. It is declared here
	// because it shares the Reason type and the metric label space.
	ReasonNoEligiblePod Reason = "no_eligible_pod"
)

// Verdict is ShouldShed's result. The zero value is Admit: Shed is false and
// every other field is meaningless. A refusal always sets Code to either 429
// (ReasonTenantTokens) or 503 (every other reason), per the client contract:
// it understands only those two codes.
type Verdict struct {
	Shed       bool
	Code       int
	Reason     Reason
	RetryAfter time.Duration
	Detail     string
}

// Stays reports whether a refusal must be answered locally rather than
// considered for the overflow decision. Only a 503 refusal is eligible to
// leave the box; admission (Shed false) and the 429 tenant refusal both
// stay.
func (v Verdict) Stays() bool {
	return v.Shed && v.Code != 503
}

// WorkerView is one worker's state as the gate sees it: a scraped snapshot
// corrected by the gateway's own reservations since that scrape. It carries
// no clock; staleness is Pick's concern, not ShouldShed's.
type WorkerView struct {
	Pod            string
	KVPoolTokens   int
	KVUsage        float64 // scraped fraction of the KV pool in use, 0..1
	ReservedTokens int     // gateway's estimate for requests dispatched here since the last scrape
	InFlight       int
	Queued         int
	MaxInflight    int
	MeanServiceS   float64 // recent mean seconds per request on this pod; 0 if unknown
	TTFTp50S       float64
	TTFTp99S       float64
	WaitingRising  bool
}

// FreeTokens is the worker's effective free KV budget: the scraped free
// pool, minus tokens the gateway has already reserved against it since the
// last scrape. It never goes negative, so a burst of reservations can push
// it to 0 but no further.
func (w WorkerView) FreeTokens() int {
	scrapedFree := int(math.Round(float64(w.KVPoolTokens) * (1 - w.KVUsage)))
	free := scrapedFree - w.ReservedTokens
	if free < 0 {
		return 0
	}
	return free
}

// FreeRatio is FreeTokens as a fraction of the pool. A pool of 0 (an unknown
// or misconfigured worker) reports 0 rather than dividing by zero.
func (w WorkerView) FreeRatio() float64 {
	if w.KVPoolTokens == 0 {
		return 0
	}
	return float64(w.FreeTokens()) / float64(w.KVPoolTokens)
}

// TenantState is one tenant's token bucket at decide time: what is left,
// and the rate it refills at.
type TenantState struct {
	AvailableTokens float64
	RatePerS        float64
}

// Policy is the tunable admission thresholds. The zero value is not safe;
// callers use DefaultPolicy or a variant built from it.
type Policy struct {
	KVLine        float64 // fraction of the KV pool that must stay free, e.g. 0.80
	ResidentFloor float64 // lower free-ratio floor granted to a resident run, e.g. 0.05
	SpreadRatio   float64 // p99/p50 multiple past which a batch worker looks unstable, e.g. 4
}

// DefaultPolicy holds the gateway's shipped admission thresholds.
var DefaultPolicy = Policy{KVLine: 0.80, ResidentFloor: 0.05, SpreadRatio: 4}

// Snap is everything ShouldShed reads about one candidate placement: the
// request's tenant and token estimate, the worker it might land on, whether
// that worker already holds the run's history, and the policy to gate with.
// Snap carries Now explicitly; ShouldShed never reads a clock.
type Snap struct {
	Now       time.Time
	Tenant    TenantState
	Worker    WorkerView
	Resident  bool // this request's run is bound to Worker
	EstTokens int  // prompt estimate plus MaxOut
	Policy    Policy
}

// ShouldShed is the course's should_shed(req, snap): a pure admission check
// against one candidate worker. Gates run in this fixed order, and the first
// one that refuses wins, so a request can never be shed for two reasons at
// once:
//
//  1. tenant_tokens: the tenant's bucket cannot cover the estimate (429).
//  2. kv_free: the worker's free KV is below the admission line, or below
//     the resident floor for a run continuing on this worker (503).
//  3. timeout_queue: the request's projected wait would carry it past its
//     deadline (503).
//  4. p99_spread: for batch only, the worker's tail latency has spread from
//     its median while its queue is still growing (503).
//
// A Snap that clears every gate returns Verdict{} (admit).
func ShouldShed(r Request, s Snap) Verdict {
	if float64(s.EstTokens) > s.Tenant.AvailableTokens {
		return Verdict{
			Shed:       true,
			Code:       429,
			Reason:     ReasonTenantTokens,
			RetryAfter: tenantRetryAfter(s.EstTokens, s.Tenant),
			Detail: "need " + strconv.Itoa(s.EstTokens) + " tokens, tenant has " +
				formatFloat(s.Tenant.AvailableTokens) + " available at " +
				formatFloat(s.Tenant.RatePerS) + "/s",
		}
	}

	if v, shed := shouldShedKVFree(s); shed {
		return v
	}

	if v, shed := shouldShedTimeoutQueue(r, s); shed {
		return v
	}

	if r.Priority == Batch {
		if v, shed := shouldShedP99Spread(s.Worker, s.Policy); shed {
			return v
		}
	}

	return Verdict{}
}

// tenantRetryAfter is the RetryAfter for a tenant_tokens refusal: the time
// until the bucket refills enough to cover the shortfall, clamped to
// [1s, 60s]. A tenant with no positive refill rate gets the ceiling, since
// there is nothing else to estimate from.
func tenantRetryAfter(estTokens int, tenant TenantState) time.Duration {
	if tenant.RatePerS <= 0 {
		return 60 * time.Second
	}
	deficit := float64(estTokens) - tenant.AvailableTokens
	seconds := math.Ceil(deficit / tenant.RatePerS)
	if seconds < 1 {
		seconds = 1
	} else if seconds > 60 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}

// shouldShedKVFree applies the kv_free gate. A resident run (its history is
// already on this worker) is exempt down to the policy's resident floor. A
// new or non-resident run sheds at the ordinary line, and also whenever its
// own estimate alone would not fit in what is currently free.
func shouldShedKVFree(s Snap) (Verdict, bool) {
	free := s.Worker.FreeRatio()

	if s.Resident {
		if free < s.Policy.ResidentFloor {
			return Verdict{
				Shed:       true,
				Code:       503,
				Reason:     ReasonKVFree,
				RetryAfter: 2 * time.Second,
				Detail: "resident free ratio " + formatFloat(free) + " below floor " +
					formatFloat(s.Policy.ResidentFloor),
			}, true
		}
		return Verdict{}, false
	}

	line := 1 - s.Policy.KVLine
	if free < line {
		return Verdict{
			Shed:       true,
			Code:       503,
			Reason:     ReasonKVFree,
			RetryAfter: 2 * time.Second,
			Detail:     "free ratio " + formatFloat(free) + " below line " + formatFloat(line),
		}, true
	}
	if s.EstTokens > s.Worker.FreeTokens() {
		return Verdict{
			Shed:       true,
			Code:       503,
			Reason:     ReasonKVFree,
			RetryAfter: 2 * time.Second,
			Detail: "estimate " + strconv.Itoa(s.EstTokens) + " exceeds free tokens " +
				strconv.Itoa(s.Worker.FreeTokens()),
		}, true
	}
	return Verdict{}, false
}

// shouldShedTimeoutQueue applies the timeout_queue gate. The projected wait
// is 0 while the worker has an open in-flight slot. Once full, it is the
// queue draining at the worker's own mean service time; an unknown mean (0,
// no history yet) is treated as no wait, since a guess would be as likely to
// shed a request that would have finished in time.
func shouldShedTimeoutQueue(r Request, s Snap) (Verdict, bool) {
	var wait time.Duration
	if s.Worker.InFlight >= s.Worker.MaxInflight && s.Worker.MeanServiceS > 0 {
		aheadOf := float64(s.Worker.Queued + 1)
		wait = time.Duration(aheadOf * s.Worker.MeanServiceS * float64(time.Second))
	}

	projected := s.Now.Add(wait)
	if projected.After(r.Deadline) {
		return Verdict{
			Shed:       true,
			Code:       503,
			Reason:     ReasonTimeoutQueue,
			RetryAfter: time.Second,
			Detail: "projected wait " + formatFloat(wait.Seconds()) + "s past deadline, " +
				strconv.Itoa(s.Worker.InFlight) + "/" + strconv.Itoa(s.Worker.MaxInflight) + " in flight",
		}, true
	}
	return Verdict{}, false
}

// shouldShedP99Spread applies the p99_spread gate: a batch-only signal that
// a worker's tail has decoupled from its median while its queue is still
// growing, rather than merely being briefly loaded. All three conditions
// must hold, so a worker with no p50 history (0) or a shrinking queue never
// sheds on this gate alone.
func shouldShedP99Spread(w WorkerView, p Policy) (Verdict, bool) {
	if w.TTFTp50S > 0 && w.TTFTp99S > p.SpreadRatio*w.TTFTp50S && w.WaitingRising {
		return Verdict{
			Shed:       true,
			Code:       503,
			Reason:     ReasonP99Spread,
			RetryAfter: 5 * time.Second,
			Detail: "p99 " + formatFloat(w.TTFTp99S) + "s exceeds " + formatFloat(p.SpreadRatio) +
				"x p50 " + formatFloat(w.TTFTp50S) + "s with a rising queue",
		}, true
	}
	return Verdict{}, false
}

// formatFloat renders f with no trailing zeros, for Detail strings. This
// package has no fmt, per the import allowlist.
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// RunUsage is a run's real token usage as vLLM reported it after some prior
// step, kept by the gateway's run table and fed back into EstimateTokens for
// that run's next step.
type RunUsage struct {
	PromptTokens     int
	CompletionTokens int
	BodyBytes        int
}

// EstimateTokens is the tokenizer-free prompt-size estimate: bytes divided
// by 3.5 on a run's first step, then anchored to vLLM's own usage from the
// prior step plus an estimate of only the bytes added since. It returns the
// prompt estimate plus the request's own MaxOut, since that is what must fit
// in a tenant's budget and a worker's KV pool.
func EstimateTokens(r Request, prior *RunUsage) int {
	if prior == nil {
		return bytesToTokens(r.BodyBytes) + r.MaxOut
	}
	growth := r.BodyBytes - prior.BodyBytes
	if growth < 0 {
		growth = 0
	}
	promptEst := prior.PromptTokens + prior.CompletionTokens + bytesToTokens(growth)
	return promptEst + r.MaxOut
}

// bytesToTokens is the shared bytes-to-tokens rounding: ceil(bytes / 3.5).
func bytesToTokens(bytes int) int {
	return int(math.Ceil(float64(bytes) / 3.5))
}
