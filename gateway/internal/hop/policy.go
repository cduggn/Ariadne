// Package hop moves a run's KV cache to the worker the gateway is moving the
// run to, so that worker reads the run's history instead of recomputing it.
// It is off unless configured, and every failure falls back to recompute, so
// a hop can make a request faster but never makes it fail.
//
// The package has three parts. Config.Decide is the pure cost rule. It hops
// only when the history is long enough and it estimates the copy to be
// cheaper than the prefill. Mooncake speaks vLLM's MooncakeConnector protocol to the
// two workers. Hopper joins them for the serve package. Both workers are the
// fleet's own pods, so a hop never sends data off the box.
package hop

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/cduggn/ariadne/gateway/internal/decide"
)

// Outcome is what one move did. It is the result label on orch_hop_total.
type Outcome string

const (
	// BelowThreshold means the run's history was shorter than MinTokens,
	// so the destination recomputed it.
	BelowThreshold Outcome = "below_threshold"
	// RecomputeCheaper means the cost rule estimated the prefill to be
	// faster than the copy, so the destination recomputed it.
	RecomputeCheaper Outcome = "recompute_cheaper"
	// Hopped means the source held the KV and the destination was told to
	// pull it.
	Hopped Outcome = "hopped"
	// Failed means the hop was attempted and did not complete, so the
	// destination recomputed the history.
	Failed Outcome = "failed"
	// Busy means MaxInflight hops were already running, so this one was
	// skipped and the destination recomputed the history.
	Busy Outcome = "busy"
)

// Outcomes lists every Outcome, for pre-registering the metric's labels.
var Outcomes = []Outcome{BelowThreshold, RecomputeCheaper, Hopped, Failed, Busy}

// Config is the cost rule's inputs. The rates describe the deployment and
// should come from a measurement on the real node. Until then they are
// estimates, and the rule is only as good as they are.
type Config struct {
	// MinTokens is the shortest history worth hopping. A shorter one
	// always recomputes, whatever the rates say, because two extra round
	// trips are not worth saving a short prefill.
	MinTokens int
	// SharedPrefixTokens is the prefix every worker already holds from
	// warm-up. Neither path pays for it, so it is left out of both costs.
	SharedPrefixTokens int
	// KVBytesPerToken is the served model's KV size per token, which turns
	// tokens into bytes to copy.
	KVBytesPerToken int
	// TransferBytesPerS is the copy bandwidth between two workers.
	TransferBytesPerS float64
	// PrefillTokensPerS is a worker's uncached prefill rate.
	PrefillTokensPerS float64
	// Overhead is the fixed cost of a hop: the request that makes the
	// source hold its KV and the handshake before the pull.
	Overhead time.Duration
	// Timeout bounds the source request. A source too busy to answer in
	// time means a recompute rather than a stalled request.
	Timeout time.Duration
	// MaxInflight caps hops running at once. Each sends the source a request
	// outside the gateway's admission and may pin its blocks until pulled, so
	// a burst of moves must not turn into a burst of hops.
	MaxInflight int
}

// DefaultConfig is sized for Qwen3-8B-AWQ on a 20 GiB A100 slice. The KV
// size is from the model's config. The prefill rate is serving/fit.py's
// estimate for that slice, 12,000 tokens in 3.15 s. The bandwidth is a
// conservative guess for TCP between two pods on one host. Measure both
// rates before trusting the rule.
var DefaultConfig = Config{
	MinTokens:          8192,
	SharedPrefixTokens: 3899,
	KVBytesPerToken:    147456,
	TransferBytesPerS:  2e9,
	PrefillTokensPerS:  3800,
	Overhead:           50 * time.Millisecond,
	Timeout:            10 * time.Second,
	MaxInflight:        4,
}

// Validate reports every field that cannot drive the rule.
func (c Config) Validate() error {
	var errs []error
	if c.MinTokens < 0 {
		errs = append(errs, fmt.Errorf("min tokens %d is negative", c.MinTokens))
	}
	if c.SharedPrefixTokens < 0 {
		errs = append(errs, fmt.Errorf("shared prefix %d is negative", c.SharedPrefixTokens))
	}
	if c.KVBytesPerToken <= 0 {
		errs = append(errs, fmt.Errorf("KV bytes per token %d must be positive", c.KVBytesPerToken))
	}
	if c.TransferBytesPerS <= 0 {
		errs = append(errs, fmt.Errorf("transfer rate %g B/s must be positive", c.TransferBytesPerS))
	}
	if c.PrefillTokensPerS <= 0 {
		errs = append(errs, fmt.Errorf("prefill rate %g tokens/s must be positive", c.PrefillTokensPerS))
	}
	if c.Overhead < 0 {
		errs = append(errs, fmt.Errorf("overhead %s is negative", c.Overhead))
	}
	if c.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("timeout %s must be positive", c.Timeout))
	}
	if c.MaxInflight < 1 {
		errs = append(errs, fmt.Errorf("max in-flight hops %d must be at least 1", c.MaxInflight))
	}
	return errors.Join(errs...)
}

// Plan is the cost rule's answer for one move. Tokens is the history beyond
// the shared prefix, the part one of the two paths has to pay for. When Hop
// is false, Outcome says why.
type Plan struct {
	Hop           bool
	Outcome       Outcome
	Tokens        int
	HopCost       time.Duration
	RecomputeCost time.Duration
}

// Decide applies the cost rule to a run whose last step's prompt was history
// tokens long. It is pure.
func (c Config) Decide(history int) Plan {
	tokens := max(0, history-c.SharedPrefixTokens)
	p := Plan{
		Tokens:        tokens,
		HopCost:       c.Overhead + seconds(float64(tokens)*float64(c.KVBytesPerToken)/c.TransferBytesPerS),
		RecomputeCost: seconds(float64(tokens) / c.PrefillTokensPerS),
	}
	switch {
	case history < c.MinTokens:
		p.Outcome = BelowThreshold
	case p.HopCost >= p.RecomputeCost:
		p.Outcome = RecomputeCheaper
	default:
		p.Hop = true
	}
	return p
}

// Moved reports whether a placement moved a run off a worker that still
// holds its history. Those are the only moves worth a hop: a run with no
// binding has no history anywhere, a kept binding needs no copy, and a
// worker that restarted lost its cache.
func Moved(s decide.Sticky) bool {
	return s == decide.StickyBrokenLoad || s == decide.StickyBrokenShed
}

// seconds rounds to the nearest nanosecond, so a cost that is exact in
// decimal does not land a nanosecond short through floating point.
func seconds(s float64) time.Duration {
	return time.Duration(math.Round(s * float64(time.Second)))
}
