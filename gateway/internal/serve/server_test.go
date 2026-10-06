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
	"sync"
	"testing"
	"time"

	"github.com/cduggn/cluster-doctor/gateway/internal/decide"
	"github.com/cduggn/cluster-doctor/gateway/internal/fakevllm"
	"github.com/cduggn/cluster-doctor/gateway/internal/fleet"
	"github.com/cduggn/cluster-doctor/gateway/internal/hop"
)

// doctorBody is a step-1 request shaped like the doctor's, and the body the
// warm-up probes replay.
const doctorBody = `{"model":"Qwen/Qwen3-8B-AWQ","messages":[{"role":"system","content":"You are cluster-doctor, an SRE agent. Diagnose the cluster from the card and the tool results, then call submit_diagnosis."},{"role":"user","content":"Cluster card: 3 nodes, 2 GPUs, 41 pods, kube-prometheus installed. Task: investigate why checkout-api restarts every few minutes."}],"tools":[{"type":"function","function":{"name":"submit_diagnosis","description":"Submit the final diagnosis","parameters":{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}}}],"tool_choice":"auto","max_completion_tokens":768,"temperature":0}`

// doctorStep2 is doctorBody with the step-1 exchange appended to messages,
// so the two share a long byte prefix.
var doctorStep2 = strings.Replace(doctorBody, `],"tools"`,
	`,{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"kubectl_get","arguments":"{\"kind\":\"pod\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"checkout-api-7d9f OOMKilled, restarts 14"}],"tools"`, 1)

var pods = []string{"vllm-0", "vllm-1"}

// tuning is what one test changes from the shipped shape. hop, when set,
// turns on the KV hop over the two fakes.
type tuning struct {
	config  func(*fleet.Config)
	options Options
	fakes   func(*fakevllm.Settings)
	hop     *hop.Config
}

// harness is two fake workers, a Gate, a Fleet and a Server, all live
// until the test ends.
type harness struct {
	t     *testing.T
	fakes map[string]*fakevllm.Worker
	fake  map[string]*httptest.Server
	gate  *fleet.Gate
	fleet *fleet.Fleet
	url   string

	mu     sync.Mutex
	events []Event
}

func start(t *testing.T, tn tuning) *harness {
	t.Helper()
	h := &harness{t: t, fakes: map[string]*fakevllm.Worker{}, fake: map[string]*httptest.Server{}}
	urls := map[string]string{}
	for _, pod := range pods {
		w := fakevllm.New()
		if tn.fakes != nil {
			w.Set(tn.fakes)
		}
		srv := httptest.NewServer(w.Handler())
		t.Cleanup(srv.Close)
		h.fakes[pod], h.fake[pod], urls[pod] = w, srv, srv.URL
	}

	cfg := fleet.DefaultConfig(pods)
	if tn.config != nil {
		tn.config(&cfg)
	}
	h.gate = fleet.NewGate(cfg, time.Now, func(int) int { return 0 })
	h.fleet = fleet.NewFleet(h.gate, urls, fleet.WorkerConfig{
		Interval:      5 * time.Millisecond,
		ScrapeTimeout: 200 * time.Millisecond,
		DownAfter:     200 * time.Millisecond,
		Window:        time.Second,
		RisingOver:    20 * time.Millisecond,
		WarmBody:      []byte(doctorBody),
		WarmUnder:     50 * time.Millisecond,
		WarmPasses:    2,
		ProbeTimeout:  time.Second,
	}, nil, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.fleet.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	opt := tn.options
	if tn.hop != nil {
		endpoints := map[string]hop.Endpoint{}
		for pod, u := range urls {
			endpoints[pod] = hop.Endpoint{BaseURL: u, BootstrapURL: u}
		}
		hopper, err := hop.New(*tn.hop, endpoints, http.DefaultClient, time.Now)
		if err != nil {
			t.Fatalf("hop.New() error = %v", err)
		}
		opt.Hop = hopper
	}
	opt.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	opt.OnRequest = func(ev Event) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.events = append(h.events, ev)
	}
	srv := httptest.NewServer(New(h.gate, h.fleet, urls, opt).Handler())
	t.Cleanup(srv.Close)
	h.url = srv.URL
	return h
}

// ready blocks until /readyz answers 200.
func (h *harness) ready() {
	h.t.Helper()
	eventually(h.t, "/readyz 200", func() bool { return h.get("/readyz").status == http.StatusOK })
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (h *harness) get(path string) response {
	h.t.Helper()
	resp, err := http.Get(h.url + path)
	if err != nil {
		h.t.Fatalf("GET %s error = %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: body}
}

// post sends body with the doctor's headers under the given request id.
func (h *harness) post(id, body string) response {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.url+"/v1/chat/completions", strings.NewReader(body))
	for k, v := range doctorHeaders(id) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("POST %s error = %v", id, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: out}
}

func doctorHeaders(id string) map[string]string {
	return map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer EMPTY",
		"X-Request-Id":  id,
		"X-Tenant":      "platform",
		"X-App":         "cluster-doctor",
		"X-Priority":    "interactive",
		"X-Data-Class":  "restricted",
	}
}

// recordedCount sums the chat requests both fakes have seen.
func (h *harness) recordedCount() int {
	n := 0
	for _, w := range h.fakes {
		n += len(w.Requests())
	}
	return n
}

// lastRecorded returns the newest chat request across both fakes with the
// given request id.
func (h *harness) lastRecorded(id string) fakevllm.Recorded {
	h.t.Helper()
	for _, w := range h.fakes {
		reqs := w.Requests()
		for i := len(reqs) - 1; i >= 0; i-- {
			if reqs[i].Header.Get("X-Request-Id") == id {
				return reqs[i]
			}
		}
	}
	h.t.Fatalf("no fake recorded request %s", id)
	return fakevllm.Recorded{}
}

func (h *harness) eventsSeen() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Event(nil), h.events...)
}

// event returns the Event for a request id, waiting for the handler's
// deferred finish, which may run after the client has its response.
func (h *harness) event(id string) Event {
	h.t.Helper()
	var found Event
	eventually(h.t, "event for "+id, func() bool {
		for _, ev := range h.eventsSeen() {
			if ev.RequestID == id {
				found = ev
				return true
			}
		}
		return false
	})
	return found
}

// inFlight sums the gate's in-flight count over both pods.
func (h *harness) inFlight() int {
	n := 0
	for _, ws := range h.gate.View() {
		n += ws.View.InFlight
	}
	return n
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func errorCode(t *testing.T, body []byte) (typ, code string) {
	t.Helper()
	var e apiError
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body is not JSON: %v\n%s", err, body)
	}
	return e.Error.Type, e.Error.Code
}

func TestChatForwardsBodyUnchangedAndAnnotates(t *testing.T) {
	h := start(t, tuning{})
	h.ready()

	resp := h.post("audit-42-s1", doctorBody)
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.status, resp.body)
	}
	rec := h.lastRecorded("audit-42-s1")
	if !bytes.Equal(rec.Body, []byte(doctorBody)) {
		t.Fatalf("fake received %q, want the client's body", rec.Body)
	}
	if !bytes.Equal(resp.body, rec.Response) {
		t.Fatalf("client received %q, want the fake's response %q", resp.body, rec.Response)
	}
	for _, k := range []string{"X-Request-Id", "X-Tenant", "X-App", "X-Priority", "X-Data-Class", "Authorization"} {
		if got, want := rec.Header.Get(k), doctorHeaders("audit-42-s1")[k]; got != want {
			t.Errorf("fake saw %s = %q, want %q", k, got, want)
		}
	}
	if pod := resp.header.Get("X-Pod"); pod != "vllm-0" && pod != "vllm-1" {
		t.Errorf("X-Pod = %q, want a pod", pod)
	}
	if got := resp.header.Get("X-Policy"); got != "prefix_then_load" {
		t.Errorf("X-Policy = %q, want prefix_then_load", got)
	}
	if got := resp.header.Get("X-Sticky"); got != "new" {
		t.Errorf("X-Sticky = %q, want new", got)
	}
	timing := strings.Split(resp.header.Get("Server-Timing"), ", ")
	if len(timing) != 3 || !strings.HasPrefix(timing[0], "gateway;dur=") ||
		!strings.HasPrefix(timing[1], "queue;dur=") || !strings.HasPrefix(timing[2], "upstream;dur=") {
		t.Errorf("Server-Timing = %q, want gateway, queue and upstream entries", resp.header.Get("Server-Timing"))
	}
	if resp.header.Get("X-Gateway-Queue-Ms") == "" {
		t.Errorf("X-Gateway-Queue-Ms missing")
	}
	ev := h.event("audit-42-s1")
	if ev.Status != 200 || ev.Reason != "" || ev.Pod != resp.header.Get("X-Pod") ||
		ev.CompletionTokens != 35 || ev.FinishReason != "tool_calls" {
		t.Errorf("event = %+v, want 200 on %s with the fake's usage", ev, resp.header.Get("X-Pod"))
	}
}

func TestSecondStepSticksToSamePodWithCachedPrefix(t *testing.T) {
	h := start(t, tuning{})
	h.ready()

	first := h.post("audit-7-s1", doctorBody)
	second := h.post("audit-7-s2", doctorStep2)
	if first.status != 200 || second.status != 200 {
		t.Fatalf("statuses = %d, %d, want 200, 200", first.status, second.status)
	}
	if first.header.Get("X-Pod") != second.header.Get("X-Pod") {
		t.Errorf("step 2 went to %s, step 1 to %s", second.header.Get("X-Pod"), first.header.Get("X-Pod"))
	}
	if got := second.header.Get("X-Sticky"); got != "hit" {
		t.Errorf("step 2 X-Sticky = %q, want hit", got)
	}
	ev := h.event("audit-7-s2")
	if ev.Sticky != decide.StickyHit || ev.CachedTokens <= 0 || ev.Step != 2 {
		t.Errorf("step 2 event = %+v, want sticky hit with cached tokens", ev)
	}
}

func TestMalformedBodyIs400AndNeverReachesAWorker(t *testing.T) {
	h := start(t, tuning{})
	h.ready()
	before := h.recordedCount()

	resp := h.post("audit-1-s1", `{"model":"m","messages":[`)
	typ, code := errorCode(t, resp.body)
	if resp.status != 400 || typ != "invalid_request_error" || code != "bad_json" {
		t.Fatalf("got %d %s/%s, want 400 invalid_request_error/bad_json", resp.status, typ, code)
	}
	if h.recordedCount() != before {
		t.Errorf("a worker saw the malformed request")
	}
	ev := h.event("audit-1-s1")
	if ev.Status != 400 || ev.Reason != "bad_json" || ev.Pod != "" {
		t.Errorf("event = %+v, want 400 bad_json with no pod", ev)
	}
}

func TestStreamIs400(t *testing.T) {
	h := start(t, tuning{})
	h.ready()

	resp := h.post("audit-2-s1", strings.Replace(doctorBody, `"temperature":0`, `"temperature":0,"stream":true`, 1))
	_, code := errorCode(t, resp.body)
	if resp.status != 400 || code != "stream_unsupported" {
		t.Fatalf("got %d %s, want 400 stream_unsupported", resp.status, code)
	}
}

func TestTenantExhaustedIs429(t *testing.T) {
	h := start(t, tuning{config: func(c *fleet.Config) {
		c.Tenants["platform"] = fleet.TenantLimit{RatePerS: 1, Burst: 10}
	}})
	h.ready()

	resp := h.post("audit-3-s1", doctorBody)
	typ, code := errorCode(t, resp.body)
	if resp.status != 429 || typ != "rate_limit_exceeded" || code != "tenant_tokens" {
		t.Fatalf("got %d %s/%s, want 429 rate_limit_exceeded/tenant_tokens", resp.status, typ, code)
	}
	if got := resp.header.Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}
	if got := resp.header.Get("X-Gateway-Reason"); got != "tenant_tokens" {
		t.Errorf("X-Gateway-Reason = %q, want tenant_tokens", got)
	}
	ev := h.event("audit-3-s1")
	if ev.Status != 429 || ev.Reason != "tenant_tokens" || ev.Pod != "" || ev.Overflow != "" {
		t.Errorf("event = %+v, want 429 tenant_tokens with no pod and no overflow decision", ev)
	}
}

func TestKVFullOnEveryWorkerIs503(t *testing.T) {
	h := start(t, tuning{})
	h.ready()
	for _, w := range h.fakes {
		w.Set(func(s *fakevllm.Settings) { s.KVUsage = 0.95 })
	}
	eventually(t, "both scrapes at 0.95", func() bool {
		for _, ws := range h.gate.View() {
			if ws.View.KVUsage < 0.95 {
				return false
			}
		}
		return true
	})

	resp := h.post("audit-4-s1", doctorBody)
	typ, code := errorCode(t, resp.body)
	if resp.status != 503 || typ != "overloaded" || code != "kv_free" {
		t.Fatalf("got %d %s/%s, want 503 overloaded/kv_free", resp.status, typ, code)
	}
	if got := resp.header.Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want 2", got)
	}
	if got := resp.header.Get("X-Gateway-Reason"); got != "kv_free" {
		t.Errorf("X-Gateway-Reason = %q, want kv_free", got)
	}
	if ev := h.event("audit-4-s1"); ev.Overflow != decide.OverflowBlocked {
		t.Errorf("restricted 503 overflow = %q, want %q", ev.Overflow, decide.OverflowBlocked)
	}
}

func TestUpstream500IsRelayedAndSettled(t *testing.T) {
	h := start(t, tuning{})
	h.ready()
	for _, w := range h.fakes {
		w.Set(func(s *fakevllm.Settings) { s.Status = http.StatusInternalServerError })
	}

	resp := h.post("audit-5-s1", doctorBody)
	if resp.status != 500 {
		t.Fatalf("status = %d, want 500", resp.status)
	}
	if rec := h.lastRecorded("audit-5-s1"); !bytes.Equal(resp.body, rec.Response) {
		t.Errorf("client received %q, want the fake's error body %q", resp.body, rec.Response)
	}
	ev := h.event("audit-5-s1")
	if ev.Status != 500 || ev.Reason != "" || ev.Pod == "" {
		t.Errorf("event = %+v, want 500 with a pod", ev)
	}
	eventually(t, "in-flight back to 0", func() bool { return h.inFlight() == 0 })
}

func TestUpstreamTimeoutIs502AndSettled(t *testing.T) {
	h := start(t, tuning{options: Options{UpstreamTimeout: 50 * time.Millisecond}})
	h.ready()
	for _, w := range h.fakes {
		w.Set(func(s *fakevllm.Settings) { s.Latency = 300 * time.Millisecond })
	}

	resp := h.post("audit-6-s1", doctorBody)
	typ, code := errorCode(t, resp.body)
	if resp.status != 502 || typ != "upstream_error" || code != "upstream_error" {
		t.Fatalf("got %d %s/%s, want 502 upstream_error/upstream_error", resp.status, typ, code)
	}
	ev := h.event("audit-6-s1")
	if ev.Status != 502 || ev.Reason != "upstream_error" || ev.Pod == "" || ev.Upstream < 50*time.Millisecond {
		t.Errorf("event = %+v, want 502 upstream_error with a pod and the time spent", ev)
	}
	eventually(t, "in-flight back to 0", func() bool { return h.inFlight() == 0 })
}

func TestOnRequestSeesEveryRequestOnce(t *testing.T) {
	h := start(t, tuning{config: func(c *fleet.Config) {
		c.Tenants["tiny"] = fleet.TenantLimit{RatePerS: 1, Burst: 10}
	}})
	h.ready()

	ok := h.post("audit-8-s1", doctorBody)
	h.post("audit-9-s1", `not json`)
	req, _ := http.NewRequest(http.MethodPost, h.url+"/v1/chat/completions", strings.NewReader(doctorBody))
	req.Header.Set("X-Request-Id", "audit-10-s1")
	req.Header.Set("X-Tenant", "tiny")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	h.event("audit-10-s1")
	got := h.eventsSeen()
	if len(got) != 3 {
		t.Fatalf("saw %d events, want 3: %+v", len(got), got)
	}
	want := []struct {
		id     string
		status int
		reason string
		pod    string
	}{
		{"audit-8-s1", 200, "", ok.header.Get("X-Pod")},
		{"audit-9-s1", 400, "bad_json", ""},
		{"audit-10-s1", 429, "tenant_tokens", ""},
	}
	for i, w := range want {
		ev := got[i]
		if ev.RequestID != w.id || ev.Status != w.status || ev.Reason != w.reason || ev.Pod != w.pod {
			t.Errorf("event %d = %+v, want %+v", i, ev, w)
		}
	}
	if got[2].Tenant != "tiny" || got[2].Priority != decide.Batch {
		t.Errorf("tenant event = %+v, want tenant tiny at batch priority", got[2])
	}
}

func TestReadyzAndDebugEndpoints(t *testing.T) {
	h := start(t, tuning{fakes: func(s *fakevllm.Settings) { s.Latency = 100 * time.Millisecond }})
	if got := h.get("/readyz").status; got != 503 {
		t.Fatalf("/readyz before warm-up = %d, want 503", got)
	}
	if got := h.get("/healthz"); got.status != 200 || string(got.body) != "ok" {
		t.Errorf("/healthz = %d %q, want 200 ok", got.status, got.body)
	}

	for _, w := range h.fakes {
		w.Set(func(s *fakevllm.Settings) { s.Latency = 0 })
	}
	h.ready()

	direct, err := http.Get(h.fake["vllm-0"].URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := io.ReadAll(direct.Body)
	direct.Body.Close()
	got := h.get("/debug/workers/vllm-0/metrics")
	if got.status != 200 || !bytes.Equal(got.body, want) || !strings.HasPrefix(got.header.Get("Content-Type"), "text/plain") {
		t.Errorf("/debug/workers/vllm-0/metrics = %d %s, want the fake's text", got.status, got.header.Get("Content-Type"))
	}
	if got := h.get("/debug/workers/nope/metrics").status; got != 404 {
		t.Errorf("unknown pod metrics = %d, want 404", got)
	}

	var workers []debugWorker
	if err := json.Unmarshal(h.get("/debug/workers").body, &workers); err != nil {
		t.Fatalf("/debug/workers is not JSON: %v", err)
	}
	if len(workers) != 2 || workers[0].Pod != "vllm-0" || workers[1].Pod != "vllm-1" {
		t.Fatalf("/debug/workers = %+v, want vllm-0 and vllm-1", workers)
	}
	for _, w := range workers {
		if w.Phase != "ready" || !w.View.Ready || w.View.View.KVPoolTokens != 79056 {
			t.Errorf("worker %s = %+v, want ready with the fake's pool", w.Pod, w)
		}
	}
}

func TestMetricsRouteServesTheHandlerWhenSet(t *testing.T) {
	exposition := "# TYPE orch_requests_total counter\norch_requests_total{class=\"restricted\",priority=\"batch\"} 0\n"
	h := start(t, tuning{options: Options{Metrics: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, exposition)
	})}})
	if got := h.get("/metrics"); got.status != 200 || string(got.body) != exposition {
		t.Errorf("/metrics = %d %q, want 200 with the handler's text", got.status, got.body)
	}
	if got := start(t, tuning{}).get("/metrics").status; got != 404 {
		t.Errorf("/metrics without Options.Metrics = %d, want 404", got)
	}
}

// eagerHop hops any moved history, so the test exercises the transport
// rather than the cost rule.
var eagerHop = hop.Config{
	MinTokens:         1,
	KVBytesPerToken:   1,
	TransferBytesPerS: 1e12,
	PrefillTokensPerS: 1,
	Timeout:           time.Second,
	MaxInflight:       4,
}

func TestMovedRunHopsItsKVAndTheNewWorkerPullsIt(t *testing.T) {
	h := start(t, tuning{hop: &eagerHop})
	h.ready()

	first := h.post("runA-s1", doctorBody)
	if first.status != http.StatusOK {
		t.Fatalf("step 1 status = %d", first.status)
	}
	from := first.header.Get("X-Pod")
	to := "vllm-1"
	if from == to {
		to = "vllm-0"
	}

	// The run's worker is now 0.5 load units busier than the other, over the
	// 0.25 slack but above the KV line, so step 2 moves for load.
	h.fakes[from].Set(func(s *fakevllm.Settings) { s.KVUsage = 0.7 })
	h.fakes[to].Set(func(s *fakevllm.Settings) { s.KVUsage = 0.2 })
	eventually(t, "the gate to see the new load", func() bool {
		v := map[string]float64{}
		for _, ws := range h.gate.View() {
			v[ws.View.Pod] = ws.View.FreeRatio()
		}
		return v[from] < 0.35 && v[to] > 0.75
	})

	second := h.post("runA-s2", doctorStep2)
	if second.status != http.StatusOK || second.header.Get("X-Pod") != to || second.header.Get("X-Hop") != string(hop.Hopped) {
		t.Fatalf("step 2 = %d on %q with X-Hop %q, want 200 on %s hopped",
			second.status, second.header.Get("X-Pod"), second.header.Get("X-Hop"), to)
	}
	ev := h.event("runA-s2")
	if ev.Sticky != decide.StickyBrokenLoad || ev.Hop != string(hop.Hopped) || ev.HopTime <= 0 {
		t.Errorf("event sticky %q hop %q took %v, want broken_load, hopped and a hop time", ev.Sticky, ev.Hop, ev.HopTime)
	}
	if ev.CachedTokens != ev.PromptTokens || ev.PromptTokens == 0 {
		t.Errorf("cached %d of %d prompt tokens, want the whole pulled history", ev.CachedTokens, ev.PromptTokens)
	}

	reqs := h.fakes[from].Requests()
	hold := reqs[len(reqs)-1]
	if strings.Contains(hold.Header.Get("X-Request-Id"), "runA") {
		t.Errorf("source request id %q carries the client's request id", hold.Header.Get("X-Request-Id"))
	}
	var held struct {
		Params    map[string]any `json:"kv_transfer_params"`
		MaxTokens int            `json:"max_tokens"`
	}
	json.Unmarshal(hold.Body, &held)
	if held.Params["do_remote_decode"] != true || held.MaxTokens != 1 {
		t.Errorf("source request params %v max_tokens %d, want do_remote_decode and a one-token cap", held.Params, held.MaxTokens)
	}
	var pulled struct {
		Params map[string]any `json:"kv_transfer_params"`
	}
	json.Unmarshal(h.lastRecorded("runA-s2").Body, &pulled)
	if pulled.Params["do_remote_prefill"] != true || pulled.Params["remote_engine_id"] != "fake-engine" ||
		pulled.Params["transfer_id"] != held.Params["transfer_id"] {
		t.Errorf("destination params %v, want do_remote_prefill from fake-engine under the source's transfer id", pulled.Params)
	}
	eventually(t, "both tickets settled", func() bool { return h.inFlight() == 0 })
}

func TestKeptRunDoesNotHop(t *testing.T) {
	h := start(t, tuning{hop: &eagerHop})
	h.ready()
	h.post("runB-s1", doctorBody)
	second := h.post("runB-s2", doctorStep2)
	if second.header.Get("X-Sticky") != string(decide.StickyHit) || second.header.Get("X-Hop") != "" {
		t.Fatalf("step 2 sticky %q hop %q, want hit and no hop", second.header.Get("X-Sticky"), second.header.Get("X-Hop"))
	}
	if got := string(h.lastRecorded("runB-s2").Body); got != doctorStep2 {
		t.Errorf("a kept run's body changed on the way to the worker")
	}
}

func TestClientKVTransferParamsAre400AndNeverReachAWorker(t *testing.T) {
	h := start(t, tuning{hop: &eagerHop})
	h.ready()
	before := h.recordedCount()

	spoofed := strings.Replace(doctorBody, `"temperature":0}`,
		`"temperature":0,"kv_transfer_params":{"do_remote_prefill":true,"remote_bootstrap_addr":"http://attacker.example:8998","remote_engine_id":"x","transfer_id":"xfer-someone-else"}}`, 1)
	resp := h.post("runC-s1", spoofed)
	typ, code := errorCode(t, resp.body)
	if resp.status != 400 || typ != "invalid_request_error" || code != "kv_transfer_params" {
		t.Fatalf("got %d %s/%s, want 400 invalid_request_error/kv_transfer_params", resp.status, typ, code)
	}
	if h.recordedCount() != before {
		t.Errorf("a worker saw a client's kv_transfer_params")
	}
}
