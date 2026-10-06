package hop

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/cduggn/cluster-doctor/gateway/internal/decide"
)

// Move is one placement as the hop sees it. From is the worker the run was
// bound to before this request and To the worker it was placed on. History
// is the prompt length the run's last settled step reported, which is what
// From holds in its cache.
type Move struct {
	RequestID string
	Sticky    decide.Sticky
	From, To  string
	History   int
}

// Result is what Hopper.Move did. Outcome is empty when the move was not one
// a hop applies to, and then nothing should be counted. Took is the time the
// hop spent talking to the source, zero unless a hop was attempted. Err is
// the reason a hop failed.
type Result struct {
	Outcome Outcome
	Plan    Plan
	Took    time.Duration
	Err     error
}

// Hopper is the gateway's hop capability: the cost rule and the transport,
// over the fixed set of workers.
type Hopper struct {
	cfg       Config
	endpoints map[string]Endpoint
	mooncake  *Mooncake
	now       func() time.Time
}

// New checks cfg and returns a Hopper over endpoints, which must name every
// worker the gateway places on.
func New(cfg Config, endpoints map[string]Endpoint, client *http.Client, now func() time.Time) (*Hopper, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("hop config: %w", err)
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("hop needs at least one worker endpoint")
	}
	return &Hopper{cfg: cfg, endpoints: endpoints, mooncake: NewMooncake(client), now: now}, nil
}

// Move returns the body to forward to m.To. When the move qualifies and the
// cost rule says hop, it makes m.From hold the run's KV and returns body
// marked for m.To to pull it. In every other case, including a failed hop, it
// returns body unchanged and m.To recomputes the history.
//
// A hop that succeeds here pins the blocks on m.From until m.To pulls them.
// If the destination request never reaches m.To (the client goes away, or the
// forward fails), vLLM frees them after VLLM_MOONCAKE_ABORT_REQUEST_TIMEOUT,
// 480 s by default, so a burst of abandoned hops costs the source KV for that
// long.
func (h *Hopper) Move(ctx context.Context, m Move, body []byte) ([]byte, Result) {
	src, known := h.endpoints[m.From]
	// The transfer id is built from the request id, so a request without one
	// could share a transfer with another and pull the wrong KV.
	if !Moved(m.Sticky) || !known || m.From == m.To || m.RequestID == "" {
		return body, Result{}
	}
	plan := h.cfg.Decide(m.History)
	if !plan.Hop {
		return body, Result{Outcome: plan.Outcome, Plan: plan}
	}

	start := h.now()
	ctx, cancel := context.WithTimeout(ctx, h.cfg.Timeout)
	defer cancel()
	params, err := h.mooncake.Send(ctx, src, body, m.RequestID+"-hop", "xfer-"+m.RequestID)
	if err == nil {
		var marked []byte
		if marked, err = Receive(body, params); err == nil {
			return marked, Result{Outcome: Hopped, Plan: plan, Took: h.now().Sub(start)}
		}
	}
	return body, Result{Outcome: Failed, Plan: plan, Took: h.now().Sub(start), Err: err}
}
