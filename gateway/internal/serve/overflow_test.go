package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cduggn/ariadne/gateway/internal/decide"
	"github.com/cduggn/ariadne/gateway/internal/fakeoverflow"
	"github.com/cduggn/ariadne/gateway/internal/fakevllm"
	"github.com/cduggn/ariadne/gateway/internal/fleet"
	"github.com/cduggn/ariadne/gateway/internal/overflow"
)

// overflowKey is the bearer key the fake backend demands.
const overflowKey = "test-key"

// withOverflow builds a fake backend and the circuit over it, probed once
// so it starts available, and returns both for tuning.options.Overflow.
func withOverflow(t *testing.T, edit func(*overflow.Config)) (*overflow.Overflow, *fakeoverflow.Backend) {
	t.Helper()
	fake := fakeoverflow.New()
	fake.Set(func(s *fakeoverflow.Settings) { s.Key = overflowKey })
	srv := httptest.NewServer(fake.Handler())
	t.Cleanup(srv.Close)
	cfg := overflow.DefaultConfig
	cfg.BaseURL = srv.URL
	cfg.Timeout = time.Second
	if edit != nil {
		edit(&cfg)
	}
	backend, err := overflow.NewSuperlinked(cfg, overflowKey, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	ov := overflow.New(backend, 0, cfg.MaxInflight, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ov.Probe(context.Background())
	return ov, fake
}

// saturate makes both fake workers report a full KV cache and waits for
// the gate to see it, so every new request is a kv_free 503.
func (h *harness) saturate() {
	h.t.Helper()
	for _, w := range h.fakes {
		w.Set(func(s *fakevllm.Settings) { s.KVUsage = 0.95 })
	}
	eventually(h.t, "both scrapes at 0.95", func() bool {
		for _, ws := range h.gate.View() {
			if ws.View.KVUsage < 0.95 {
				return false
			}
		}
		return true
	})
}

// postAs is post with a different data class.
func (h *harness) postAs(id, class, body string) response {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.url+"/v1/chat/completions", strings.NewReader(body))
	for k, v := range doctorHeaders(id) {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Data-Class", class)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("POST %s error = %v", id, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: out}
}

func TestSaturated503OfANonRestrictedRequestIsServedByTheOverflowBackend(t *testing.T) {
	ov, fake := withOverflow(t, func(c *overflow.Config) { c.Model = "Qwen/Qwen3.8-27B-FP8" })
	h := start(t, tuning{options: Options{Overflow: ov}})
	h.ready()
	h.saturate()
	before := h.recordedCount()

	resp := h.postAs("ovf-1-s1", "internal", doctorBody)
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d, body %s, want 200 from the overflow backend", resp.status, resp.body)
	}
	reqs := fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("backend saw %d requests, want 1", len(reqs))
	}
	if !bytes.Equal(resp.body, reqs[0].Response) {
		t.Errorf("client received %q, want the backend's answer %q", resp.body, reqs[0].Response)
	}
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(reqs[0].Body, &sent); err != nil || sent.Model != "Qwen/Qwen3.8-27B-FP8" {
		t.Errorf("backend got model %q (err %v), want the configured one", sent.Model, err)
	}
	if got := reqs[0].Header.Get("Authorization"); got != "Bearer "+overflowKey {
		t.Errorf("backend saw Authorization %q, want the configured key, not the client's", got)
	}
	for _, k := range []string{"X-Request-Id", "X-Tenant", "X-Data-Class"} {
		if reqs[0].Header.Get(k) != "" {
			t.Errorf("%s left the box", k)
		}
	}
	if h.recordedCount() != before {
		t.Errorf("a worker saw the overflowed request")
	}
	if got := resp.header.Get("X-Overflow"); got != overflow.Sent {
		t.Errorf("X-Overflow = %q, want sent", got)
	}
	if got := resp.header.Get("X-Overflow-Backend"); got != "superlinked" {
		t.Errorf("X-Overflow-Backend = %q, want superlinked", got)
	}
	if got := resp.header.Get("X-Gateway-Reason"); got != "kv_free" {
		t.Errorf("X-Gateway-Reason = %q, want the shed reason the request left for", got)
	}
	if !strings.HasPrefix(resp.header.Get("Server-Timing"), "overflow;dur=") {
		t.Errorf("Server-Timing = %q, want an overflow entry", resp.header.Get("Server-Timing"))
	}
	if resp.header.Get("Retry-After") != "" || resp.header.Get("X-Pod") != "" {
		t.Errorf("a served request carries Retry-After %q or X-Pod %q", resp.header.Get("Retry-After"), resp.header.Get("X-Pod"))
	}
	ev := h.event("ovf-1-s1")
	if ev.Status != 200 || ev.Reason != "" || ev.Pod != "" || ev.Overflow != overflow.Sent || ev.OverflowTime <= 0 ||
		ev.CompletionTokens != 30 || ev.FinishReason != "tool_calls" || ev.Class != decide.Internal {
		t.Errorf("event = %+v, want 200 sent off-box with the backend's usage and no pod", ev)
	}
	if h.inFlight() != 0 {
		t.Errorf("in-flight = %d after an overflowed request, want 0: nothing was reserved", h.inFlight())
	}
}

func TestRestricted503NeverReachesTheOverflowBackend(t *testing.T) {
	ov, fake := withOverflow(t, nil)
	h := start(t, tuning{options: Options{Overflow: ov}})
	h.ready()
	h.saturate()

	for i, class := range []string{"restricted", "", "RESTRICTED", "nonsense"} {
		id := "ovf-r-s" + string(rune('1'+i))
		resp := h.postAs(id, class, doctorBody)
		typ, code := errorCode(t, resp.body)
		if resp.status != 503 || typ != "overloaded" || code != "kv_free" {
			t.Fatalf("class %q: got %d %s/%s, want 503 overloaded/kv_free", class, resp.status, typ, code)
		}
		if resp.header.Get("X-Overflow") != "" || resp.header.Get("Retry-After") != "2" {
			t.Errorf("class %q: headers %v, want the plain 503", class, resp.header)
		}
		ev := h.event(id)
		if ev.Overflow != decide.OverflowBlocked || ev.Class != decide.Restricted || ev.OverflowTime != 0 {
			t.Errorf("class %q: event = %+v, want blocked_invariant on a restricted request", class, ev)
		}
	}
	if n := len(fake.Requests()); n != 0 {
		t.Fatalf("the overflow backend saw %d restricted requests", n)
	}
	if !ov.Available() {
		t.Error("blocked requests opened the circuit")
	}
}

func TestUnavailableBackendYieldsTheOriginal503Unchanged(t *testing.T) {
	// The control: no backend at all, the shipped gateway.
	control := start(t, tuning{})
	control.ready()
	control.saturate()
	want := control.postAs("ovf-c-s1", "internal", doctorBody)
	if want.status != 503 {
		t.Fatalf("control status = %d, want 503", want.status)
	}
	if ev := control.event("ovf-c-s1"); ev.Overflow != decide.OverflowNoBackend {
		t.Fatalf("control overflow = %q, want no_backend", ev.Overflow)
	}

	ov, fake := withOverflow(t, nil)
	fake.Set(func(s *fakeoverflow.Settings) { s.Down = true })
	ov.Probe(context.Background()) // the backend went away: the circuit opens
	h := start(t, tuning{options: Options{Overflow: ov}})
	h.ready()
	h.saturate()

	got := h.postAs("ovf-u-s1", "internal", doctorBody)
	if got.status != want.status || !bytes.Equal(got.body, want.body) {
		t.Fatalf("got %d %s, want the control's %d %s", got.status, got.body, want.status, want.body)
	}
	for _, k := range []string{"Retry-After", "X-Gateway-Reason", "Content-Type"} {
		if got.header.Get(k) != want.header.Get(k) {
			t.Errorf("%s = %q, control %q", k, got.header.Get(k), want.header.Get(k))
		}
	}
	for _, k := range []string{"X-Overflow", "X-Overflow-Backend", "Server-Timing"} {
		if got.header.Get(k) != "" {
			t.Errorf("%s = %q on the 503, want it absent", k, got.header.Get(k))
		}
	}
	if n := len(fake.Requests()); n != 0 {
		t.Errorf("an unavailable backend saw %d requests", n)
	}
	if ev := h.event("ovf-u-s1"); ev.Status != 503 || ev.Reason != "kv_free" || ev.Overflow != overflow.Unavailable {
		t.Errorf("event = %+v, want 503 kv_free marked unavailable", ev)
	}
}

func TestBackendErrorYieldsTheOriginal503AndOpensTheCircuit(t *testing.T) {
	ov, fake := withOverflow(t, nil)
	fake.Set(func(s *fakeoverflow.Settings) { s.Status = http.StatusPaymentRequired })
	h := start(t, tuning{options: Options{Overflow: ov}})
	h.ready()
	h.saturate()

	first := h.postAs("ovf-e-s1", "internal", doctorBody)
	_, code := errorCode(t, first.body)
	if first.status != 503 || code != "kv_free" || first.header.Get("X-Overflow") != "" {
		t.Fatalf("got %d %s with X-Overflow %q, want the plain 503 kv_free", first.status, code, first.header.Get("X-Overflow"))
	}
	if ev := h.event("ovf-e-s1"); ev.Overflow != overflow.Failed || ev.OverflowTime <= 0 || ev.Status != 503 {
		t.Errorf("event = %+v, want 503 marked failed with the time spent", ev)
	}
	second := h.postAs("ovf-e-s2", "internal", doctorBody)
	if second.status != 503 {
		t.Fatalf("second status = %d, want 503", second.status)
	}
	if ev := h.event("ovf-e-s2"); ev.Overflow != overflow.Unavailable {
		t.Errorf("second event overflow = %q, want unavailable: the circuit should be open", ev.Overflow)
	}
	if n := len(fake.Requests()); n != 1 {
		t.Errorf("backend saw %d requests, want 1", n)
	}
}

func TestA429NeverReachesTheOverflowBackend(t *testing.T) {
	ov, fake := withOverflow(t, nil)
	h := start(t, tuning{
		config:  func(c *fleet.Config) { c.Tenants["platform"] = fleet.TenantLimit{RatePerS: 1, Burst: 10} },
		options: Options{Overflow: ov},
	})
	h.ready()
	resp := h.postAs("ovf-q-s1", "internal", doctorBody)
	if resp.status != 429 || resp.header.Get("X-Overflow") != "" {
		t.Fatalf("got %d with X-Overflow %q, want a plain 429", resp.status, resp.header.Get("X-Overflow"))
	}
	if ev := h.event("ovf-q-s1"); ev.Overflow != "" {
		t.Errorf("event overflow = %q, want none for a 429", ev.Overflow)
	}
	if n := len(fake.Requests()); n != 0 {
		t.Errorf("backend saw %d requests for a tenant refusal", n)
	}
}

func TestAdmittedRequestNeverReachesTheOverflowBackend(t *testing.T) {
	ov, fake := withOverflow(t, nil)
	h := start(t, tuning{options: Options{Overflow: ov}})
	h.ready()
	resp := h.postAs("ovf-a-s1", "internal", doctorBody)
	if resp.status != 200 || resp.header.Get("X-Pod") == "" || resp.header.Get("X-Overflow") != "" {
		t.Fatalf("got %d pod %q X-Overflow %q, want 200 from a worker", resp.status, resp.header.Get("X-Pod"), resp.header.Get("X-Overflow"))
	}
	if n := len(fake.Requests()); n != 0 {
		t.Errorf("backend saw %d requests while a worker had room", n)
	}
}
