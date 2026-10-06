// Package serve is the gateway's HTTP shell. It reads each request, asks
// decide and fleet what to do, forwards the body to the chosen worker and
// relays the answer. It holds no policy of its own. The body goes unchanged
// unless the optional KV hop marks it for the worker to pull a moved run's
// cache.
package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cduggn/cluster-doctor/gateway/internal/decide"
	"github.com/cduggn/cluster-doctor/gateway/internal/fleet"
	"github.com/cduggn/cluster-doctor/gateway/internal/hop"
)

// DefaultUpstreamTimeout bounds one upstream call. It sits under the
// client's 120s timeout so the client always hears from the gateway.
const DefaultUpstreamTimeout = 115 * time.Second

// maxResponseBytes caps one upstream response body.
const maxResponseBytes = 32 << 20

// Options shape a Server. A zero field takes its default in New, which is
// decide.DefaultMaxBody, decide.DefaultBudgets, DefaultUpstreamTimeout, a
// plain http.Client, slog.Default or time.Now. OnRequest, when set, sees
// one Event per request, including refusals and 400s. Metrics, when set,
// serves GET /metrics. The metrics package feeds the one from the other.
// Hop, when set, copies a moved run's KV to its new worker; nil leaves every
// moved run to recompute its history.
type Options struct {
	MaxBody         int
	Budgets         decide.Budgets
	UpstreamTimeout time.Duration
	Client          *http.Client
	Log             *slog.Logger
	Now             func() time.Time
	OnRequest       func(Event)
	Metrics         http.Handler
	Hop             *hop.Hopper
}

// Event is one request as the gateway saw it. Pod, Policy, Sticky, Unknown
// and Est are zero when the guard rejected the request. Status is 0 when
// the client went away before anything was written. Gateway is the time
// spent in this process outside the queue, the hop and the upstream call.
// The token fields are set only for a 200 that carried usage. Overflow is set
// only for a 503 the gateway refused, and names what the overflow decision
// did. Hop is set only when the KV hop is configured and the request moved a
// run off a worker that still held its history, and HopTime is the time the
// hop spent with the old worker.
type Event struct {
	RequestID        string
	Run              decide.RunID
	Step             int
	Tenant           string
	Priority         decide.Priority
	Class            decide.DataClass
	Pod              string
	Policy           decide.PickPolicy
	Sticky           decide.Sticky
	Unknown          bool
	Est              int
	Status           int
	Reason           string
	Gateway          time.Duration
	Queue            time.Duration
	Upstream         time.Duration
	Total            time.Duration
	PromptTokens     int
	CachedTokens     int
	CompletionTokens int
	FinishReason     string
	Overflow         string
	Hop              string
	HopTime          time.Duration
}

// Server is the gateway's handler set over one Gate and one Fleet. urls
// maps each pod to its base URL.
type Server struct {
	gate  *fleet.Gate
	fleet *fleet.Fleet
	urls  map[string]string
	opt   Options
}

// New builds a Server and fills in the defaults for every zero Option.
func New(g *fleet.Gate, f *fleet.Fleet, urls map[string]string, opt Options) *Server {
	if opt.MaxBody == 0 {
		opt.MaxBody = decide.DefaultMaxBody
	}
	if opt.Budgets == (decide.Budgets{}) {
		opt.Budgets = decide.DefaultBudgets
	}
	if opt.UpstreamTimeout == 0 {
		opt.UpstreamTimeout = DefaultUpstreamTimeout
	}
	if opt.Client == nil {
		opt.Client = &http.Client{}
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	trimmed := make(map[string]string, len(urls))
	for pod, base := range urls {
		trimmed[pod] = strings.TrimSuffix(base, "/")
	}
	return &Server{gate: g, fleet: f, urls: trimmed, opt: opt}
}

// Handler routes the gateway's endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /debug/workers", s.workers)
	mux.HandleFunc("GET /debug/workers/{pod}/metrics", s.workerMetrics)
	if s.opt.Metrics != nil {
		mux.Handle("GET /metrics", s.opt.Metrics)
	}
	return mux
}

// chat is the proxy path. It guards, admits, forwards and relays, settles
// the ticket once a ticket exists, and finishes the Event exactly once.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	start := s.opt.Now()
	h := headersOf(r.Header)
	ev := eventFor(h)
	defer func() {
		ev.Total = s.opt.Now().Sub(start)
		s.finish(ev)
	}()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(s.opt.MaxBody)+1))
	var tooBig *http.MaxBytesError
	if err != nil && !errors.As(err, &tooBig) {
		ev.Reason = "client_gone"
		return
	}

	g := decide.Inspect(body, h, start, s.opt.Budgets, s.opt.MaxBody)
	if !g.OK() {
		ev.Status, ev.Reason = g.Reject.Code, g.Reject.Reason
		writeError(w, g.Reject.Code, g.Reject.Detail, "invalid_request_error", g.Reject.Reason)
		return
	}
	req := g.Req
	ev.Tenant = req.Tenant

	d, t, err := s.gate.Admit(r.Context(), req)
	p := d.Placement
	ev.Pod, ev.Policy, ev.Sticky, ev.Unknown, ev.Est, ev.Queue = p.Pod, p.Policy, p.Sticky, p.Unknown, d.Est, d.Queued
	if err != nil {
		ev.Reason = "client_gone"
		return
	}
	if t == nil {
		ev.Status, ev.Reason = p.Verdict.Code, string(p.Verdict.Reason)
		if !p.Verdict.Stays() {
			ev.Overflow = decide.OverflowResult(req, p.Verdict)
		}
		writeRefusal(w, p.Verdict)
		return
	}

	outcome := fleet.Outcome{}
	defer func() { s.gate.Settle(t, outcome, len(body)) }()

	ctx, cancel := context.WithTimeout(r.Context(), s.opt.UpstreamTimeout)
	defer cancel()
	fwd := s.moveKV(ctx, d, req, t.Pod(), body, &ev)
	up, err := s.forward(ctx, s.urls[t.Pod()], fwd, r.Header)
	ev.Upstream = up.took
	if err != nil && r.Context().Err() != nil {
		// The client went away and its cancellation stopped the forward. That is not a worker failure, so it is not
		// counted as one, and there is nobody to answer.
		ev.Reason = "client_gone"
		return
	}
	if err != nil {
		ev.Status, ev.Reason = http.StatusBadGateway, "upstream_error"
		writeError(w, http.StatusBadGateway, "upstream request failed: "+err.Error(), "upstream_error", "upstream_error")
		return
	}

	ev.Status = up.status
	if up.status == http.StatusOK {
		u, ok := usageOf(up.body)
		ev.PromptTokens, ev.CachedTokens, ev.CompletionTokens, ev.FinishReason = u.prompt, u.cached, u.completion, u.finish
		outcome = fleet.Outcome{OK: ok, PromptTokens: u.prompt, CompletionTokens: u.completion, Latency: up.took}
	}
	ev.Gateway = s.opt.Now().Sub(start) - ev.Queue - ev.HopTime - ev.Upstream
	relay(w, up, ev)
}

// moveKV asks the KV hop, when configured, to carry the run's history from
// the worker it was bound to onto pod. It returns the body to forward, which
// is body itself unless a hop succeeded. A failed hop is logged and the
// worker recomputes, so it never fails the request.
func (s *Server) moveKV(ctx context.Context, d fleet.Decision, req decide.Request, pod string, body []byte, ev *Event) []byte {
	if s.opt.Hop == nil {
		return body
	}
	out, res := s.opt.Hop.Move(ctx, hop.Move{
		Sticky:  d.Placement.Sticky,
		From:    d.From,
		To:      pod,
		History: d.History,
	}, body)
	ev.Hop, ev.HopTime = string(res.Outcome), res.Took
	if res.Err != nil {
		s.opt.Log.Warn("kv hop failed, the worker recomputes", "request_id", req.ID, "from", d.From, "to", pod, "err", res.Err)
	}
	return out
}

// forwarded are the request headers copied to the worker. Everything else,
// including hop-by-hop headers, stops at the gateway.
var forwarded = []string{
	"Content-Type", "Authorization", "Accept",
	"X-Request-Id", "X-Tenant", "X-App", "X-Priority", "X-Data-Class",
}

// upstream is one worker's answer, read in full.
type upstream struct {
	status      int
	contentType string
	body        []byte
	took        time.Duration
}

// forward posts body unchanged to base's chat endpoint and reads the whole
// answer. A transport error, a timeout or a body read error is returned
// with the time spent, so the caller can still report it.
func (s *Server) forward(ctx context.Context, base string, body []byte, in http.Header) (upstream, error) {
	start := s.opt.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return upstream{}, err
	}
	for _, k := range forwarded {
		if v := in.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := s.opt.Client.Do(req)
	if err != nil {
		return upstream{took: s.opt.Now().Sub(start)}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	took := s.opt.Now().Sub(start)
	if err != nil {
		return upstream{took: took}, err
	}
	return upstream{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: b, took: took}, nil
}

// relay writes the worker's status and body unchanged, plus the gateway's
// own placement and timing headers.
func relay(w http.ResponseWriter, up upstream, ev Event) {
	h := w.Header()
	if up.contentType != "" {
		h.Set("Content-Type", up.contentType)
	}
	h.Set("X-Pod", ev.Pod)
	h.Set("X-Policy", string(ev.Policy))
	h.Set("X-Sticky", string(ev.Sticky))
	if ev.Hop != "" {
		h.Set("X-Hop", ev.Hop)
	}
	h.Set("X-Gateway-Queue-Ms", millisText(ev.Queue))
	h.Set("Server-Timing", "gateway;dur="+millisText(ev.Gateway)+
		", queue;dur="+millisText(ev.Queue)+
		", upstream;dur="+millisText(ev.Upstream))
	w.WriteHeader(up.status)
	w.Write(up.body)
}

// usage is what the gateway reads from a worker's answer. Every field is
// optional in the wire shape, and a missing one reads as zero.
type usage struct {
	prompt, completion, cached int
	finish                     string
}

// usageOf decodes the usage and the first finish reason from a chat
// completion. ok reports whether the body carried a usage object at all. A
// body that is not JSON reads as no usage.
func usageOf(body []byte) (u usage, ok bool) {
	var wire struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			Details          struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &wire) != nil || wire.Usage == nil {
		return usage{}, false
	}
	u = usage{prompt: wire.Usage.PromptTokens, completion: wire.Usage.CompletionTokens, cached: wire.Usage.Details.CachedTokens}
	if len(wire.Choices) > 0 {
		u.finish = wire.Choices[0].FinishReason
	}
	return u, true
}

// finish writes the request's log line and hands the Event to OnRequest.
func (s *Server) finish(ev Event) {
	s.opt.Log.Info("request",
		"request_id", ev.RequestID,
		"run", string(ev.Run),
		"step", ev.Step,
		"tenant", ev.Tenant,
		"priority", ev.Priority.String(),
		"class", ev.Class.String(),
		"pod", ev.Pod,
		"policy", string(ev.Policy),
		"sticky", string(ev.Sticky),
		"unknown", ev.Unknown,
		"est", ev.Est,
		"status", ev.Status,
		"reason", ev.Reason,
		"queue_ms", millis(ev.Queue),
		"upstream_ms", millis(ev.Upstream),
		"total_ms", millis(ev.Total),
		"prompt_tokens", ev.PromptTokens,
		"cached_tokens", ev.CachedTokens,
		"completion_tokens", ev.CompletionTokens,
		"finish_reason", ev.FinishReason,
		"overflow", ev.Overflow,
		"hop", ev.Hop,
		"hop_ms", millis(ev.HopTime),
	)
	if s.opt.OnRequest != nil {
		s.opt.OnRequest(ev)
	}
}

// readyz answers 200 while any pod is Ready, else 503.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	for _, ws := range s.gate.View() {
		if ws.Ready {
			io.WriteString(w, "ready")
			return
		}
	}
	http.Error(w, "no worker is ready", http.StatusServiceUnavailable)
}

// debugWorker is one pod on /debug/workers, the fleet's status next to the
// gate's view. Phase is the status phase as text, because the wire form of
// fleet.Phase is a number.
type debugWorker struct {
	Pod    string             `json:"pod"`
	Phase  string             `json:"phase"`
	Status fleet.WorkerStatus `json:"status"`
	View   decide.WorkerState `json:"view"`
}

func (s *Server) workers(w http.ResponseWriter, r *http.Request) {
	views := make(map[string]decide.WorkerState)
	for _, ws := range s.gate.View() {
		views[ws.View.Pod] = ws
	}
	out := []debugWorker{}
	for _, st := range s.fleet.Status() {
		out = append(out, debugWorker{Pod: st.Pod, Phase: st.Phase.String(), Status: st, View: views[st.Pod]})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *Server) workerMetrics(w http.ResponseWriter, r *http.Request) {
	pod := r.PathValue("pod")
	if _, ok := s.urls[pod]; !ok {
		http.Error(w, "unknown pod "+pod, http.StatusNotFound)
		return
	}
	body, err := s.fleet.RawMetrics(r.Context(), pod)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.Write(body)
}

// headersOf copies the headers decide reads out of an http.Header.
func headersOf(h http.Header) decide.Headers {
	return decide.Headers{
		RequestID: h.Get("X-Request-Id"),
		Tenant:    h.Get("X-Tenant"),
		App:       h.Get("X-App"),
		Priority:  h.Get("X-Priority"),
		DataClass: h.Get("X-Data-Class"),
	}
}

// eventFor starts an Event from the headers alone, so a request the guard
// rejects is still logged with what the headers said.
func eventFor(h decide.Headers) Event {
	run, step := decide.ParseRequestID(h.RequestID)
	return Event{
		RequestID: h.RequestID,
		Run:       run,
		Step:      step,
		Tenant:    h.Tenant,
		Priority:  decide.ParsePriority(h.Priority),
		Class:     decide.ParseDataClass(h.DataClass),
	}
}

// apiError is the OpenAI error shape, the only error body the client
// parses.
type apiError struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func writeError(w http.ResponseWriter, status int, message, typ, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(apiError{Error: apiErrorBody{Message: message, Type: typ, Code: code}})
}

// writeRefusal answers a shed verdict. The client reads only the status
// code, so the reason and the retry hint ride in headers and in the body.
func writeRefusal(w http.ResponseWriter, v decide.Verdict) {
	typ := "overloaded"
	if v.Code == http.StatusTooManyRequests {
		typ = "rate_limit_exceeded"
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(v.RetryAfter.Seconds()))))
	w.Header().Set("X-Gateway-Reason", string(v.Reason))
	writeError(w, v.Code, v.Detail, typ, string(v.Reason))
}

// millis is d in milliseconds rounded to one decimal, for the log line.
func millis(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Millisecond)*10) / 10
}

// millisText is millis as text, for the headers.
func millisText(d time.Duration) string {
	return strconv.FormatFloat(millis(d), 'f', 1, 64)
}
