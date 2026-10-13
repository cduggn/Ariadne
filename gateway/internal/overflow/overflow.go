// Package overflow sends a 503 the gateway cannot serve to a backend off the
// box, when the stay-or-leave rule allows it. It is off unless configured,
// and every failure falls back to the 503 the client would have had anyway,
// so overflow can turn a refusal into an answer but never makes one worse.
//
// The package has three parts. Backend is the wire client: Superlinked
// speaks the OpenAI-compatible chat-completions shape over HTTPS with a
// bearer key, and Null is the backend of an unconfigured gateway. Overflow
// wraps a Backend with the circuit the serve package calls: a startup probe
// and periodic re-probes mark the backend available or not, a failed send
// opens the circuit until the next probe passes, and an in-flight cap keeps
// a burst of 503s from becoming a burst of paid calls. The decision whether a
// request may leave at all is not here: it is decide.MayLeave, and Send
// refuses any permit that did not come from it.
package overflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/cduggn/ariadne/gateway/internal/decide"
)

// Overflow results, the label values of orch_overflow_total. The first two
// are decide's, repeated here so one list names every result.
const (
	// Blocked means a 503 for a restricted request was kept on the box.
	Blocked = decide.OverflowBlocked
	// NoBackend means the request could leave but no backend is configured.
	NoBackend = decide.OverflowNoBackend
	// Sent means the backend answered 200 and its answer was relayed.
	Sent = "sent"
	// Failed means the backend was tried and gave an error, a non-200 or
	// no answer in time, so the client got the original 503.
	Failed = "failed"
	// Unavailable means the circuit was open, because the last probe or the
	// last send failed, so the backend was not tried.
	Unavailable = "unavailable"
	// Capped means MaxInflight overflow calls were already running.
	Capped = "capped"
)

// Outcomes lists every result, for pre-registering the metric's labels.
var Outcomes = []string{Blocked, NoBackend, Sent, Failed, Unavailable, Capped}

// ErrNoPermit is returned when Send is called with an Offboxable that did
// not come from decide.MayLeave.
var ErrNoPermit = errors.New("overflow: no valid permit to leave the box")

// ErrNoBackend is what the Null backend answers.
var ErrNoBackend = errors.New("overflow: no backend configured")

// Answer is a backend's reply, read in full.
type Answer struct {
	Status      int
	ContentType string
	Body        []byte
}

// Backend is one place a 503 may be sent. Send posts the request body and
// returns the backend's answer; it must refuse an invalid permit. Probe
// checks that the backend is reachable and the credentials work, and
// returns nil when it is. Name labels the backend in logs and headers.
type Backend interface {
	Send(ctx context.Context, permit decide.Offboxable, body []byte) (Answer, error)
	Probe(ctx context.Context) error
	Name() string
}

// Null is the backend of an unconfigured gateway. Every call fails with
// ErrNoBackend, so the client always gets the 503.
type Null struct{}

func (Null) Send(context.Context, decide.Offboxable, []byte) (Answer, error) {
	return Answer{}, ErrNoBackend
}
func (Null) Probe(context.Context) error { return ErrNoBackend }
func (Null) Name() string                { return "null" }

// Result is what Overflow.Send did. Outcome is always set. Status, ContentType
// and Body are set only for Sent. Took is the time spent with the backend,
// zero unless it was called. Err says why the request did not leave.
type Result struct {
	Outcome     string
	Status      int
	ContentType string
	Body        []byte
	Took        time.Duration
	Err         error
}

// Overflow is the circuit over one Backend.
type Overflow struct {
	backend  Backend
	interval time.Duration
	now      func() time.Time
	log      *slog.Logger
	up       atomic.Bool
	probes   atomic.Int64 // completed probes, for tests and /debug
	slots    chan struct{}
}

// New wraps backend. interval is how often Run re-probes; maxInflight caps
// concurrent sends. The backend starts unavailable until a probe passes.
func New(backend Backend, interval time.Duration, maxInflight int, now func() time.Time, log *slog.Logger) *Overflow {
	if maxInflight < 1 {
		maxInflight = 1
	}
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Overflow{backend: backend, interval: interval, now: now, log: log, slots: make(chan struct{}, maxInflight)}
}

// Name is the backend's.
func (o *Overflow) Name() string { return o.backend.Name() }

// Available reports whether the circuit is closed: the last probe passed
// and no send has failed since.
func (o *Overflow) Available() bool { return o.up.Load() }

// Probes is how many probes have completed, passed or not.
func (o *Overflow) Probes() int64 { return o.probes.Load() }

// Probe runs one probe and sets the availability from it. It returns the
// new availability. A probe never fails the gateway; a backend that is
// down, unreachable or refusing the key just stays unavailable.
func (o *Overflow) Probe(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	err := o.backend.Probe(ctx)
	was := o.up.Swap(err == nil)
	o.probes.Add(1)
	switch {
	case err != nil && was:
		o.log.Warn("overflow backend unavailable", "backend", o.Name(), "err", err)
	case err != nil:
		o.log.Debug("overflow backend still unavailable", "backend", o.Name(), "err", err)
	case !was:
		o.log.Info("overflow backend available", "backend", o.Name())
	}
	return err == nil
}

// probeTimeout bounds one probe. It is short because a probe runs on the
// gateway's clock, not a request's.
const probeTimeout = 10 * time.Second

// Run probes once, then every interval until ctx ends.
func (o *Overflow) Run(ctx context.Context) {
	o.Probe(ctx)
	if o.interval <= 0 {
		<-ctx.Done()
		return
	}
	t := time.NewTicker(o.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.Probe(ctx)
		}
	}
}

// Send forwards body to the backend when the permit is valid, the circuit
// is closed and a slot is free. Anything else is a Result without a Body,
// and the caller answers the original 503. A failed send opens the circuit
// until the next probe passes, so a backend that is down or out of credit
// costs at most one call per probe interval.
func (o *Overflow) Send(ctx context.Context, permit decide.Offboxable, body []byte) Result {
	if !permit.Valid() {
		return Result{Outcome: Blocked, Err: ErrNoPermit}
	}
	if !o.up.Load() {
		return Result{Outcome: Unavailable, Err: errors.New("overflow: backend unavailable")}
	}
	select {
	case o.slots <- struct{}{}:
		defer func() { <-o.slots }()
	default:
		return Result{Outcome: Capped, Err: errors.New("overflow: in-flight cap reached")}
	}
	start := o.now()
	ans, err := o.backend.Send(ctx, permit, body)
	took := o.now().Sub(start)
	if err == nil && ans.Status != http.StatusOK {
		err = fmt.Errorf("overflow: backend answered %d", ans.Status)
	}
	if err != nil {
		if ctx.Err() == nil { // the client leaving is not the backend's fault
			o.up.Store(false)
		}
		return Result{Outcome: Failed, Took: took, Err: err}
	}
	return Result{Outcome: Sent, Status: ans.Status, ContentType: ans.ContentType, Body: ans.Body, Took: took}
}
