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
	// ReasonTenantTokens means the request would overdraw its tenant's
	// token bucket.
	ReasonTenantTokens Reason = "tenant_tokens"
	// ReasonKVFree means the worker's effective free KV would fall below the
	// admission line, or below the resident floor for a run already there.
	ReasonKVFree Reason = "kv_free"
	// ReasonTimeoutQueue means the request's projected wait in the worker's
	// queue would take it past its deadline.
	ReasonTimeoutQueue Reason = "timeout_queue"
	// ReasonP99Spread means the worker's tail latency has moved far from its
	// median while its queue keeps growing. It applies to batch requests only.
	ReasonP99Spread Reason = "p99_spread"
	// ReasonNoEligiblePod means no worker survived Pick's Ready and staleness
	// filters. Pick uses it, not ShouldShed. It lives here because it shares
	// the Reason type and the metric label.
	ReasonNoEligiblePod Reason = "no_eligible_pod"
	// ReasonQueueFull means the worker's queue was full, or an interactive arrival displaced this batch request.
	ReasonQueueFull Reason = "queue_full"
)

// Verdict is ShouldShed's result. The zero value means admit, and then only
// Shed is meaningful. A refusal sets Code to 429 for ReasonTenantTokens and to
// 503 for every other reason, because the client understands only those two.
type Verdict struct {
	Shed       bool
	Code       int
	Reason     Reason
	RetryAfter time.Duration
	Detail     string
}

// Stays reports whether a refusal must be answered locally and kept away
// from the overflow decision. Only a 503 refusal may leave the box. A 429
// tenant refusal stays.
func (v Verdict) Stays() bool {
	return v.Shed && v.Code != 503
}

// WorkerView is one worker's state as the gate sees it. It combines the last
// scrape with the gateway's own reservations since that scrape. It has no
// clock, because Pick judges staleness and ShouldShed does not.
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

// FreeTokens is the worker's effective free KV. It is the scraped free pool
// minus the tokens the gateway reserved on this worker since the last
// scrape. It never goes below 0.
func (w WorkerView) FreeTokens() int {
	scrapedFree := int(math.Round(float64(w.KVPoolTokens) * (1 - w.KVUsage)))
	free := scrapedFree - w.ReservedTokens
	if free < 0 {
		return 0
	}
	return free
}

// FreeRatio is FreeTokens as a fraction of the pool. An unknown or
// misconfigured worker with a pool of 0 reports 0 instead of dividing by
// zero.
func (w WorkerView) FreeRatio() float64 {
	if w.KVPoolTokens == 0 {
		return 0
	}
	return float64(w.FreeTokens()) / float64(w.KVPoolTokens)
}

// TenantState is one tenant's token bucket at decision time, with the tokens
// left and the refill rate.
type TenantState struct {
	AvailableTokens float64
	RatePerS        float64
}

// Policy holds the admission thresholds. Its zero value is not safe, so
// callers start from DefaultPolicy.
type Policy struct {
	KVLine        float64 // fraction of the KV pool that must stay free, e.g. 0.80
	ResidentFloor float64 // lower free-ratio floor granted to a resident run, e.g. 0.05
	SpreadRatio   float64 // p99/p50 multiple past which a batch worker looks unstable, e.g. 4
}

// DefaultPolicy holds the gateway's shipped admission thresholds.
var DefaultPolicy = Policy{KVLine: 0.80, ResidentFloor: 0.05, SpreadRatio: 4}

// Snap is everything ShouldShed reads about one candidate placement. It
// holds the tenant, the token estimate, the candidate worker, whether that
// worker holds the run's history, and the thresholds. It carries Now, so
// ShouldShed never reads a clock.
type Snap struct {
	Now       time.Time
	Tenant    TenantState
	Worker    WorkerView
	Resident  bool // this request's run is bound to Worker
	EstTokens int  // prompt estimate plus MaxOut
	Policy    Policy
}

// ShouldShed is the course's should_shed(req, snap). It checks one candidate
// worker without side effects. The gates run in this order, and the first
// refusal wins, so a request is never shed for two reasons at once.
//
//  1. tenant_tokens (429): the tenant's bucket cannot cover the estimate.
//  2. kv_free (503): the worker's free KV is below the admission line, or
//     below the resident floor for a run continuing on this worker.
//  3. timeout_queue (503): the projected wait would pass the deadline.
//  4. p99_spread (503), batch only: the worker's tail latency has moved far
//     from its median while its queue keeps growing.
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

// tenantRetryAfter is the time until the tenant's bucket refills enough to
// cover the shortfall, kept between 1s and 60s. A tenant with no refill rate
// gets 60s, because there is nothing to estimate from.
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

// shouldShedKVFree applies the kv_free gate. A run whose history is on this
// worker may continue down to the resident floor. Any other request sheds at
// the admission line, or when its estimate does not fit in the free KV.
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
// is 0 while the worker has a free slot. When it is full, the wait is the
// queue ahead times the worker's mean service time. A worker with no service
// history counts as no wait, because a guess could shed a request that would
// have finished in time.
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

// shouldShedP99Spread applies the p99_spread gate. It sheds a batch request
// when the worker's p99 TTFT exceeds SpreadRatio times its p50 and its queue
// keeps growing. A worker with no p50 history or a shrinking queue never
// sheds here.
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

// formatFloat renders f with no trailing zeros for Detail strings. The
// import allowlist excludes fmt.
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// RunUsage is the token usage vLLM reported for a run's previous step. The
// gateway's run table keeps it and passes it to EstimateTokens for the next
// step.
type RunUsage struct {
	PromptTokens     int
	CompletionTokens int
	BodyBytes        int
}

// EstimateTokens estimates a request's tokens without a tokenizer. A run's
// first step uses body bytes divided by 3.5. Later steps start from vLLM's
// usage for the previous step and add the new bytes divided by 3.5. The
// result includes MaxOut, because the tenant's budget and the worker's KV must
// cover the output too.
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

// bytesToTokens returns ceil(bytes / 3.5).
func bytesToTokens(bytes int) int {
	return int(math.Ceil(float64(bytes) / 3.5))
}
