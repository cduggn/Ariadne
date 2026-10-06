package hop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	Sticky   decide.Sticky
	From, To string
	History  int
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
	slots     chan struct{} // one token per hop in flight, MaxInflight deep
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
	return &Hopper{
		cfg:       cfg,
		endpoints: endpoints,
		mooncake:  NewMooncake(client),
		now:       now,
		slots:     make(chan struct{}, cfg.MaxInflight),
	}, nil
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
	if !Moved(m.Sticky) || !known || m.From == m.To {
		return body, Result{}
	}
	plan := h.cfg.Decide(m.History)
	if !plan.Hop {
		return body, Result{Outcome: plan.Outcome, Plan: plan}
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		return body, Result{Outcome: Busy, Plan: plan}
	}

	start := h.now()
	// The transfer id names the held blocks on the source, and any request
	// that presents it pulls them. It is random and never derived from the
	// client's request id, so one tenant cannot name, or collide with, a
	// transfer made for another.
	transferID, err := newTransferID()
	if err != nil {
		return body, Result{Outcome: Failed, Plan: plan, Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, h.cfg.Timeout)
	defer cancel()
	params, err := h.mooncake.Send(ctx, src, body, transferID, transferID)
	if err == nil {
		var marked []byte
		if marked, err = Receive(body, params); err == nil {
			return marked, Result{Outcome: Hopped, Plan: plan, Took: h.now().Sub(start)}
		}
	}
	return body, Result{Outcome: Failed, Plan: plan, Took: h.now().Sub(start), Err: err}
}

// newTransferID is 128 random bits, unguessable and collision-free in
// practice.
func newTransferID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("transfer id: %w", err)
	}
	return "xfer-" + hex.EncodeToString(b[:]), nil
}
