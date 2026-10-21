package serve

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cduggn/ariadne/gateway/internal/decide"
	"github.com/cduggn/ariadne/gateway/internal/fleet"
	"github.com/cduggn/ariadne/gateway/internal/overflow"
)

// routerAnswer is a 200 chat completion with 100 prompt and 20 completion
// tokens.
const routerAnswer = `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":64}}}`

// fakeRouter stands in for llm-d's Envoy. It answers every chat request with
// the configured status, headers and body, and records the request headers.
type fakeRouter struct {
	mu      sync.Mutex
	status  int
	header  map[string]string
	body    string
	seen    []http.Header
	srv     *httptest.Server
	baseURL string
}

func newFakeRouter(t *testing.T) *fakeRouter {
	t.Helper()
	f := &fakeRouter{status: http.StatusOK, body: routerAnswer}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.seen = append(f.seen, r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		for k, v := range f.header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
	}))
	t.Cleanup(f.srv.Close)
	f.baseURL = f.srv.URL
	return f
}

// answer sets what the router answers from now on.
func (f *fakeRouter) answer(status int, header map[string]string, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.header, f.body = status, header, body
}

func (f *fakeRouter) requests() []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]http.Header(nil), f.seen...)
}

// routerBurst is the platform tenant's bucket in the router tests, with no
// refill so every charge and refund shows exactly.
const routerBurst = 50000.0

// startRouter builds a router-mode Server in front of base, with a Gate that
// has no pods and a Fleet that has no members, as main wires router mode.
func startRouter(t *testing.T, base string, opt Options) *harness {
	t.Helper()
	cfg := fleet.DefaultConfig(nil)
	cfg.Tenants["platform"] = fleet.TenantLimit{RatePerS: 0, Burst: routerBurst}
	h := &harness{t: t}
	h.gate = fleet.NewGate(cfg, time.Now, func(int) int { return 0 })
	h.fleet = fleet.NewFleet(h.gate, nil, fleet.WorkerConfig{}, nil, time.Now)
	opt.Router = base
	opt.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	opt.OnRequest = func(ev Event) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.events = append(h.events, ev)
	}
	srv := httptest.NewServer(New(h.gate, h.fleet, nil, opt).Handler())
	t.Cleanup(srv.Close)
	h.url = srv.URL
	return h
}

func TestRouterAnswerIsRelayedAndChargedAtItsUsage(t *testing.T) {
	fake := newFakeRouter(t)
	fake.answer(http.StatusOK, map[string]string{"x-gateway-destination-endpoint-served": "10.0.0.7:8000"}, routerAnswer)
	h := startRouter(t, fake.baseURL, Options{})

	if got := h.get("/readyz"); got.status != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200 with no workers of its own", got.status)
	}
	resp := h.post("rt-1-s1", doctorBody)
	if resp.status != http.StatusOK || string(resp.body) != routerAnswer {
		t.Fatalf("got %d %s, want 200 with the router's body", resp.status, resp.body)
	}
	if got := resp.header.Get("X-Pod"); got != "10.0.0.7:8000" {
		t.Errorf("X-Pod = %q, want the endpoint llm-d served from", got)
	}
	if got := resp.header.Get("X-Policy"); got != "router" {
		t.Errorf("X-Policy = %q, want router", got)
	}
	if got := h.gate.TenantTokens("platform"); got != routerBurst-120 {
		t.Errorf("bucket = %v, want %v: the charge settles at the 120 tokens used", got, routerBurst-120)
	}
	ev := h.event("rt-1-s1")
	if ev.Status != 200 || ev.Reason != "" || ev.Pod != "10.0.0.7:8000" || ev.Policy != decide.PolicyRouter ||
		ev.Sticky != "" || ev.PromptTokens != 100 || ev.CachedTokens != 64 || ev.CompletionTokens != 20 {
		t.Errorf("event = %+v, want a 200 on 10.0.0.7:8000 by router with its usage", ev)
	}

	fake.answer(http.StatusOK, nil, routerAnswer)
	if resp := h.post("rt-2-s1", doctorBody); resp.header.Get("X-Pod") != "router" {
		t.Errorf("X-Pod without the served header = %q, want router", resp.header.Get("X-Pod"))
	}
}

func TestRouterSaturationIsA503TheClientRetries(t *testing.T) {
	fake := newFakeRouter(t)
	fake.answer(http.StatusTooManyRequests, map[string]string{"x-llm-d-request-dropped-reason": "rejected-saturated"}, "saturated")
	h := startRouter(t, fake.baseURL, Options{})

	resp := h.post("rt-3-s1", doctorBody)
	typ, code := errorCode(t, resp.body)
	if resp.status != 503 || typ != "overloaded" || code != "router_saturated" {
		t.Fatalf("got %d %s/%s, want 503 overloaded/router_saturated", resp.status, typ, code)
	}
	if got := resp.header.Get("X-Gateway-Reason"); got != "router_saturated" {
		t.Errorf("X-Gateway-Reason = %q, want router_saturated", got)
	}
	if got := resp.header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if got := h.gate.TenantTokens("platform"); got != routerBurst {
		t.Errorf("bucket = %v, want %v: a refused request is refunded", got, routerBurst)
	}
	ev := h.event("rt-3-s1")
	if ev.Status != 503 || ev.Reason != "router_saturated" || ev.Overflow != decide.OverflowBlocked {
		t.Errorf("event = %+v, want 503 router_saturated, blocked as restricted", ev)
	}
}

func TestRouterSaturationGoesThroughStayOrLeave(t *testing.T) {
	fake := newFakeRouter(t)
	fake.answer(http.StatusTooManyRequests, map[string]string{"x-llm-d-request-dropped-reason": "rejected-saturated"}, "saturated")
	ov, backend := withOverflow(t, nil)
	h := startRouter(t, fake.baseURL, Options{Overflow: ov})

	resp := h.postAs("rt-4-s1", "public", doctorBody)
	if resp.status != http.StatusOK || resp.header.Get("X-Overflow") != overflow.Sent {
		t.Fatalf("public: got %d, X-Overflow %q, want 200 sent", resp.status, resp.header.Get("X-Overflow"))
	}
	if got := resp.header.Get("X-Gateway-Reason"); got != "router_saturated" {
		t.Errorf("public: X-Gateway-Reason = %q, want router_saturated", got)
	}
	reqs := backend.Requests()
	if len(reqs) != 1 || !bytes.Equal(resp.body, reqs[0].Response) {
		t.Fatalf("public: backend saw %d requests, want 1 whose answer the client got", len(reqs))
	}
	u, _ := usageOf(resp.body)
	if got, want := h.gate.TenantTokens("platform"), routerBurst-float64(u.prompt+u.completion); got != want {
		t.Errorf("public: bucket = %v, want %v: charged once, for the overflowed answer", got, want)
	}

	resp = h.postAs("rt-5-s1", "restricted", doctorBody)
	_, code := errorCode(t, resp.body)
	if resp.status != 503 || code != "router_saturated" {
		t.Fatalf("restricted: got %d %s, want 503 router_saturated", resp.status, code)
	}
	if ev := h.event("rt-5-s1"); ev.Overflow != decide.OverflowBlocked {
		t.Errorf("restricted: overflow = %q, want blocked_invariant", ev.Overflow)
	}
	if n := len(backend.Requests()); n != 1 {
		t.Errorf("backend saw %d requests, want only the public one", n)
	}
	if len(fake.requests()) != 2 {
		t.Errorf("router saw %d requests, want 2", len(fake.requests()))
	}
}

func TestRouterGetsTheGatewaysFlowHeadersAndNeverTheClients(t *testing.T) {
	fake := newFakeRouter(t)
	h := startRouter(t, fake.baseURL, Options{})

	for _, priority := range []string{"interactive", "batch"} {
		req, _ := http.NewRequest(http.MethodPost, h.url+"/v1/chat/completions", strings.NewReader(doctorBody))
		for k, v := range doctorHeaders("rt-6-" + priority + "-s1") {
			req.Header.Set(k, v)
		}
		req.Header.Set("X-Tenant", " Acme ")
		req.Header.Set("X-Priority", priority)
		req.Header.Set("x-llm-d-inference-fairness-id", "someone-else")
		req.Header.Set("x-llm-d-inference-objective", "vip")
		req.Header.Set("x-gateway-inference-fairness-id", "someone-else")
		req.Header.Set("x-gateway-inference-objective", "vip")
		req.Header.Set("x-gateway-destination-endpoint", "10.0.0.99:8000")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	seen := fake.requests()
	if len(seen) != 2 {
		t.Fatalf("router saw %d requests, want 2", len(seen))
	}
	for i, objective := range []string{"doctor-interactive", "doctor-batch"} {
		if got := seen[i].Get("x-llm-d-inference-fairness-id"); got != "acme" {
			t.Errorf("request %d: fairness id = %q, want the parsed tenant acme", i, got)
		}
		if got := seen[i].Values("x-llm-d-inference-objective"); len(got) != 1 || got[0] != objective {
			t.Errorf("request %d: objective = %q, want only %q", i, got, objective)
		}
		for _, k := range []string{"x-gateway-destination-endpoint", "x-gateway-inference-fairness-id", "x-gateway-inference-objective"} {
			if got := seen[i].Get(k); got != "" {
				t.Errorf("request %d: the client's %s = %q reached the router", i, k, got)
			}
		}
		if got := seen[i].Get("X-Request-Id"); !strings.HasPrefix(got, "rt-6-") {
			t.Errorf("request %d: X-Request-Id = %q, want the client's id forwarded", i, got)
		}
	}
}

func TestRouterNeverSeesATenantOverQuota(t *testing.T) {
	fake := newFakeRouter(t)
	h := startRouter(t, fake.baseURL, Options{})
	h.gate.SetTenantTokensForTest("platform", 10)

	resp := h.post("rt-7-s1", doctorBody)
	typ, code := errorCode(t, resp.body)
	if resp.status != 429 || typ != "rate_limit_exceeded" || code != "tenant_tokens" {
		t.Fatalf("got %d %s/%s, want 429 rate_limit_exceeded/tenant_tokens", resp.status, typ, code)
	}
	if got := resp.header.Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60 for a bucket with no refill", got)
	}
	if n := len(fake.requests()); n != 0 {
		t.Errorf("router saw %d requests, want 0", n)
	}
	if got := h.gate.TenantTokens("platform"); got != 10 {
		t.Errorf("bucket = %v, want 10: a refusal charges nothing", got)
	}
}

func TestRouterInternalErrorIsRelayedNotOverflowed(t *testing.T) {
	fake := newFakeRouter(t)
	fake.answer(http.StatusInternalServerError, map[string]string{"x-llm-d-request-dropped-reason": "rejected-internal"}, "internal flow control error")
	ov, backend := withOverflow(t, nil)
	h := startRouter(t, fake.baseURL, Options{Overflow: ov})

	resp := h.postAs("rt-8-s1", "public", doctorBody)
	if resp.status != 500 || string(resp.body) != "internal flow control error" {
		t.Fatalf("got %d %q, want the router's 500 relayed", resp.status, resp.body)
	}
	if n := len(backend.Requests()); n != 0 {
		t.Errorf("backend saw %d requests, want 0: a 500 is not a capacity refusal", n)
	}
	ev := h.event("rt-8-s1")
	if ev.Status != 500 || ev.Reason != "" || ev.Overflow != "" || ev.Pod != "router" {
		t.Errorf("event = %+v, want a relayed 500 from router with no overflow decision", ev)
	}
	if got := h.gate.TenantTokens("platform"); got != routerBurst {
		t.Errorf("bucket = %v, want %v: an unserved request is refunded", got, routerBurst)
	}
}

func TestRouterTransportErrorIs502AndRefunded(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	h := startRouter(t, gone.URL, Options{})

	resp := h.post("rt-9-s1", doctorBody)
	typ, code := errorCode(t, resp.body)
	if resp.status != 502 || typ != "upstream_error" || code != "upstream_error" {
		t.Fatalf("got %d %s/%s, want 502 upstream_error/upstream_error", resp.status, typ, code)
	}
	if got := h.gate.TenantTokens("platform"); got != routerBurst {
		t.Errorf("bucket = %v, want %v: an unserved request is refunded", got, routerBurst)
	}
	if ev := h.event("rt-9-s1"); ev.Status != 502 || ev.Reason != "upstream_error" || ev.Policy != decide.PolicyRouter {
		t.Errorf("event = %+v, want 502 upstream_error by router", ev)
	}
}
