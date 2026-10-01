package fleet

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cduggn/cluster-doctor/gateway/internal/decide"
)

// fakeVLLM serves /metrics and /v1/chat/completions the way the fleet
// expects, with every knob settable mid-test.
type fakeVLLM struct {
	srv *httptest.Server

	mu          sync.Mutex
	metrics     string
	metricsFail bool
	probeDelay  time.Duration
	probeStatus int
	probeBodies [][]byte
	probeIDs    []string
}

func newFakeVLLM(t *testing.T) *fakeVLLM {
	t.Helper()
	f := &fakeVLLM{metrics: vllmMetrics, probeStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		fail, body := f.metricsFail, f.metrics
		f.mu.Unlock()
		if fail {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, body)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.probeBodies = append(f.probeBodies, body)
		f.probeIDs = append(f.probeIDs, r.Header.Get("X-Request-Id"))
		delay, status := f.probeDelay, f.probeStatus
		f.mu.Unlock()
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(status)
		io.WriteString(w, `{"choices":[]}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVLLM) set(fn func(f *fakeVLLM)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeVLLM) probes() (bodies [][]byte, ids []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.probeBodies...), append([]string(nil), f.probeIDs...)
}

var warmBody = []byte(`{"model":"Qwen/Qwen3-8B-AWQ","messages":[{"role":"user","content":"warm"}],"max_completion_tokens":1}`)

func fastWorkerConfig() WorkerConfig {
	return WorkerConfig{
		Interval:      5 * time.Millisecond,
		ScrapeTimeout: 200 * time.Millisecond,
		DownAfter:     60 * time.Millisecond,
		Window:        time.Second,
		RisingOver:    20 * time.Millisecond,
		WarmBody:      warmBody,
		WarmUnder:     50 * time.Millisecond,
		WarmPasses:    2,
		ProbeTimeout:  time.Second,
	}
}

// startFleet runs a one-pod fleet against the fake until the test ends.
func startFleet(t *testing.T, fake *fakeVLLM, cfg WorkerConfig) (*Gate, *Fleet) {
	t.Helper()
	g := NewGate(DefaultConfig([]string{"vllm-0"}), time.Now, zeroRnd)
	f := NewFleet(g, map[string]string{"vllm-0": fake.srv.URL}, cfg, fake.srv.Client(), time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return g, f
}

// eventually polls cond until it holds or the deadline passes.
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

func status(f *Fleet) WorkerStatus {
	return f.Status()[0]
}

func readyOf(t *testing.T, g *Gate, pod string) bool {
	t.Helper()
	for _, w := range g.View() {
		if w.View.Pod == pod {
			return w.Ready
		}
	}
	t.Fatalf("View has no pod %q", pod)
	return false
}

func TestWorkerWarmsUpThenIsReady(t *testing.T) {
	fake := newFakeVLLM(t)
	g, f := startFleet(t, fake, fastWorkerConfig())

	if readyOf(t, g, "vllm-0") {
		t.Fatal("pod Ready before any scrape")
	}

	eventually(t, "phase ready", func() bool { return status(f).Phase == Ready })
	st := status(f)
	if st.Pod != "vllm-0" || st.Probes < 2 || st.Restarts != 1 || st.LastScrape.IsZero() {
		t.Fatalf("Status = %+v, want pod vllm-0, Probes >= 2, Restarts 1, a LastScrape", st)
	}
	if st.LastProbeS <= 0 || st.LastProbeS >= 0.05 {
		t.Fatalf("LastProbeS = %v, want a fast probe under 0.05", st.LastProbeS)
	}
	if !readyOf(t, g, "vllm-0") {
		t.Fatal("Gate.View Ready = false after warm-up")
	}
	if v := viewOf(t, g, "vllm-0"); v.KVPoolTokens != 79056 || v.KVUsage != 0.42 {
		t.Fatalf("View = pool %d usage %v, want 79056 0.42", v.KVPoolTokens, v.KVUsage)
	}

	bodies, ids := fake.probes()
	if len(bodies) < 2 {
		t.Fatalf("fake saw %d probes, want at least 2", len(bodies))
	}
	for i, b := range bodies {
		if !bytes.Equal(b, warmBody) {
			t.Fatalf("probe %d body = %q, want %q", i, b, warmBody)
		}
	}
	if ids[0] != "warmup-vllm-0-1-s1" || ids[1] != "warmup-vllm-0-2-s1" {
		t.Fatalf("probe ids = %v, want warmup-vllm-0-1-s1, warmup-vllm-0-2-s1", ids[:2])
	}

	raw, err := f.RawMetrics(context.Background(), "vllm-0")
	if err != nil || string(raw) != vllmMetrics {
		t.Fatalf("RawMetrics = %q, %v, want the fixture body", raw, err)
	}
	if _, err := f.RawMetrics(context.Background(), "nope"); err == nil {
		t.Fatal("RawMetrics(nope) error = nil, want unknown pod")
	}
}

func TestSlowOrFailingProbeKeepsWorkerWarming(t *testing.T) {
	fake := newFakeVLLM(t)
	fake.set(func(f *fakeVLLM) { f.probeDelay = 80 * time.Millisecond })
	g, f := startFleet(t, fake, fastWorkerConfig())

	eventually(t, "two slow probes", func() bool { return status(f).Probes >= 2 })
	st := status(f)
	if st.Phase != Warming {
		t.Fatalf("Phase = %s after slow probes, want warming", st.Phase)
	}
	if st.LastProbeS < 0.08 {
		t.Fatalf("LastProbeS = %v, want at least 0.08", st.LastProbeS)
	}
	if readyOf(t, g, "vllm-0") {
		t.Fatal("Gate.View Ready = true after slow probes")
	}

	fake.set(func(f *fakeVLLM) { f.probeDelay = 0; f.probeStatus = http.StatusInternalServerError })
	seen := status(f).Probes
	eventually(t, "two failing probes", func() bool { return status(f).Probes >= seen+2 })
	if st := status(f); st.Phase != Warming {
		t.Fatalf("Phase = %s after 500 probes, want warming", st.Phase)
	}

	fake.set(func(f *fakeVLLM) { f.probeStatus = http.StatusOK })
	eventually(t, "phase ready once probes pass", func() bool { return status(f).Phase == Ready })
	if !readyOf(t, g, "vllm-0") {
		t.Fatal("Gate.View Ready = false after probes pass")
	}
}

func TestOutageMarksDownAndRecoveryRestarts(t *testing.T) {
	fake := newFakeVLLM(t)
	g, f := startFleet(t, fake, fastWorkerConfig())
	eventually(t, "phase ready", func() bool { return status(f).Phase == Ready })

	now := time.Now()
	_, tk := mustAdmit(t, g, decide.Request{
		ID:        "run1-s1",
		Run:       "run1",
		Step:      1,
		Tenant:    "platform",
		Priority:  decide.Interactive,
		BodyBytes: 7000,
		MaxOut:    768,
		Arrived:   now,
		Deadline:  now.Add(10 * time.Second),
	})
	g.Settle(tk, Outcome{OK: true, PromptTokens: 9000, CompletionTokens: 60, Latency: 300 * time.Millisecond}, 7000)
	if got := g.Bound("run1"); got != "vllm-0" {
		t.Fatalf("Bound(run1) = %q, want vllm-0", got)
	}

	fake.set(func(f *fakeVLLM) { f.metricsFail = true })
	eventually(t, "phase down", func() bool { return status(f).Phase == Down })
	if readyOf(t, g, "vllm-0") {
		t.Fatal("Gate.View Ready = true while down")
	}
	if got := g.Bound("run1"); got != "vllm-0" {
		t.Fatalf("Bound(run1) = %q during outage, want vllm-0 until a restart", got)
	}

	fake.set(func(f *fakeVLLM) { f.metricsFail = false })
	eventually(t, "restart counted", func() bool { return status(f).Restarts == 2 })
	if st := status(f); st.Phase == Down {
		t.Fatalf("Phase = %s after recovery, want warming or ready", st.Phase)
	}
	if got := g.Bound("run1"); got != "" {
		t.Fatalf("Bound(run1) = %q after restart, want \"\"", got)
	}
}

func TestWaitingRisingComparesAgainstRisingOverAgo(t *testing.T) {
	fake := newFakeVLLM(t)
	g, f := startFleet(t, fake, fastWorkerConfig())
	eventually(t, "phase ready", func() bool { return status(f).Phase == Ready })
	if viewOf(t, g, "vllm-0").WaitingRising {
		t.Fatal("WaitingRising = true on a flat waiting count")
	}

	fake.set(func(f *fakeVLLM) {
		f.metrics = strings.Replace(vllmMetrics,
			`vllm:num_requests_waiting{engine="0",model_name="Qwen/Qwen3-8B-AWQ"} 3.0`,
			`vllm:num_requests_waiting{engine="0",model_name="Qwen/Qwen3-8B-AWQ"} 9.0`, 1)
	})
	eventually(t, "WaitingRising", func() bool { return viewOf(t, g, "vllm-0").WaitingRising })
	eventually(t, "WaitingRising to settle", func() bool { return !viewOf(t, g, "vllm-0").WaitingRising })
}

func TestStepTransitionTable(t *testing.T) {
	cases := []struct {
		from Phase
		e    event
		want Phase
	}{
		{Down, scraped, Warming},
		{Down, warmed, Down},
		{Down, stale, Down},
		{Warming, scraped, Warming},
		{Warming, warmed, Ready},
		{Warming, stale, Down},
		{Ready, scraped, Ready},
		{Ready, warmed, Ready},
		{Ready, stale, Down},
	}
	for _, c := range cases {
		if got := step(c.from, c.e); got != c.want {
			t.Errorf("step(%s, %d) = %s, want %s", c.from, c.e, got, c.want)
		}
	}
}

func TestPhaseString(t *testing.T) {
	if Down.String() != "down" || Warming.String() != "warming" || Ready.String() != "ready" {
		t.Fatalf("Phase strings = %s %s %s, want down warming ready", Down, Warming, Ready)
	}
}
