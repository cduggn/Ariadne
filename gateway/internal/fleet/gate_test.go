package fleet

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cduggn/cluster-doctor/gateway/internal/decide"
)

// fakeClock is a settable clock. It starts at the real time so the queue,
// which reads time.Now for deadlines, agrees with the Gate about the
// present, and tests only ever move it forward.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Now()}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func zeroRnd(int) int { return 0 }

func newTestGate(cfg Config) (*Gate, *fakeClock) {
	clk := newFakeClock()
	return NewGate(cfg, clk.now, zeroRnd), clk
}

// scrapeReady marks a pod ready with a fresh scrape of the given pool and
// usage.
func scrapeReady(g *Gate, clk *fakeClock, pod string, pool int, usage float64) {
	g.Observe(pod, Scrape{At: clk.now(), KVPoolTokens: pool, KVUsage: usage})
	g.SetReady(pod, true)
}

// request builds an interactive request. The run id comes from id's -s<n>
// suffix, as Inspect would parse it.
func request(clk *fakeClock, id, tenant string, bodyBytes, maxOut int) decide.Request {
	run, step := decide.ParseRequestID(id)
	now := clk.now()
	return decide.Request{
		ID:        id,
		Run:       run,
		Step:      step,
		Tenant:    tenant,
		Priority:  decide.Interactive,
		BodyBytes: bodyBytes,
		MaxOut:    maxOut,
		Arrived:   now,
		Deadline:  now.Add(10 * time.Second),
	}
}

func mustAdmit(t *testing.T, g *Gate, r decide.Request) (Decision, *Ticket) {
	t.Helper()
	d, tk, err := g.Admit(context.Background(), r)
	if err != nil {
		t.Fatalf("Admit(%s) error = %v", r.ID, err)
	}
	if tk == nil {
		t.Fatalf("Admit(%s) refused: %+v", r.ID, d.Placement.Verdict)
	}
	return d, tk
}

func mustRefuse(t *testing.T, g *Gate, r decide.Request, code int, reason decide.Reason) Decision {
	t.Helper()
	d, tk, err := g.Admit(context.Background(), r)
	if err != nil {
		t.Fatalf("Admit(%s) error = %v", r.ID, err)
	}
	if tk != nil {
		t.Fatalf("Admit(%s) placed on %s, want %d %s", r.ID, tk.Pod(), code, reason)
	}
	v := d.Placement.Verdict
	if !v.Shed || v.Code != code || v.Reason != reason || d.Placement.Pod != "" {
		t.Fatalf("Admit(%s) verdict = %+v pod %q, want %d %s", r.ID, v, d.Placement.Pod, code, reason)
	}
	return d
}

func viewOf(t *testing.T, g *Gate, pod string) decide.WorkerView {
	t.Helper()
	for _, w := range g.View() {
		if w.View.Pod == pod {
			return w.View
		}
	}
	t.Fatalf("View has no pod %q", pod)
	return decide.WorkerView{}
}

func assertReserved(t *testing.T, g *Gate, pod string, want int) {
	t.Helper()
	if got := viewOf(t, g, pod).ReservedTokens; got != want {
		t.Fatalf("%s ReservedTokens = %d, want %d", pod, got, want)
	}
}

func assertPlaced(t *testing.T, d Decision, tk *Ticket, pod string, sticky decide.Sticky, est int) {
	t.Helper()
	if d.Placement.Pod != pod || tk.Pod() != pod {
		t.Fatalf("placed on %q (ticket %q), want %q", d.Placement.Pod, tk.Pod(), pod)
	}
	if d.Placement.Sticky != sticky {
		t.Fatalf("Sticky = %q, want %q", d.Placement.Sticky, sticky)
	}
	if d.Est != est {
		t.Fatalf("Est = %d, want %d", d.Est, est)
	}
}

func TestNewRunLandsOnLessLoadedPodAndSticks(t *testing.T) {
	g, clk := newTestGate(DefaultConfig([]string{"a", "b"}))
	scrapeReady(g, clk, "a", 100000, 0.5)
	scrapeReady(g, clk, "b", 100000, 0.2)

	d1, tk1 := mustAdmit(t, g, request(clk, "run1-s1", "platform", 7000, 768))
	assertPlaced(t, d1, tk1, "b", decide.StickyNew, 2768)
	if got := g.Bound("run1"); got != "b" {
		t.Fatalf("Bound(run1) = %q, want b", got)
	}
	g.Settle(tk1, Outcome{OK: true, PromptTokens: 9000, CompletionTokens: 60, Latency: 300 * time.Millisecond}, 7000)

	clk.advance(time.Second)
	scrapeReady(g, clk, "a", 100000, 0.5)
	scrapeReady(g, clk, "b", 100000, 0.6)

	d2, tk2 := mustAdmit(t, g, request(clk, "run1-s2", "platform", 10500, 768))
	assertPlaced(t, d2, tk2, "b", decide.StickyHit, 10828)
	if got := viewOf(t, g, "b").MeanServiceS; got != 0.3 {
		t.Fatalf("b MeanServiceS = %v, want 0.3", got)
	}
	g.Settle(tk2, Outcome{OK: true, PromptTokens: 10000, CompletionTokens: 50, Latency: 100 * time.Millisecond}, 10500)
	if got := viewOf(t, g, "b").MeanServiceS; got != 0.2*0.1+0.8*0.3 {
		t.Fatalf("b MeanServiceS = %v, want 0.26", got)
	}
}

func TestDecisionNamesWhereAMovedRunLeftItsHistory(t *testing.T) {
	g, clk := newTestGate(DefaultConfig([]string{"a", "b"}))
	scrapeReady(g, clk, "a", 100000, 0.5)
	scrapeReady(g, clk, "b", 100000, 0.2)

	d1, tk1 := mustAdmit(t, g, request(clk, "run1-s1", "platform", 7000, 768))
	if d1.From != "" || d1.History != 0 {
		t.Fatalf("step 1 From %q History %d, want no binding and no history", d1.From, d1.History)
	}
	g.Settle(tk1, Outcome{OK: true, PromptTokens: 9000, CompletionTokens: 60, Latency: time.Second}, 7000)

	// b, the bound worker, is now 0.5 load units busier than a, over the
	// 0.25 slack, but still above the KV line, so the run moves for load.
	clk.advance(time.Second)
	scrapeReady(g, clk, "a", 100000, 0.2)
	scrapeReady(g, clk, "b", 100000, 0.7)
	d2, tk2 := mustAdmit(t, g, request(clk, "run1-s2", "platform", 10500, 768))
	defer g.Settle(tk2, Outcome{}, 10500)
	if d2.Placement.Pod != "a" || d2.Placement.Sticky != decide.StickyBrokenLoad {
		t.Fatalf("step 2 placed on %q with sticky %q, want a moved run on a", d2.Placement.Pod, d2.Placement.Sticky)
	}
	if d2.From != "b" || d2.History != 9000 {
		t.Fatalf("step 2 From %q History %d, want b and 9000", d2.From, d2.History)
	}
	if got := g.Bound("run1"); got != "a" {
		t.Fatalf("Bound(run1) = %q after the move, want a", got)
	}
}

func TestUnusablePodsAreNeverPicked(t *testing.T) {
	g, clk := newTestGate(DefaultConfig([]string{"a", "b", "c"}))
	g.SetReady("a", true)
	g.Observe("b", Scrape{At: clk.now(), KVPoolTokens: 100000})

	d := mustRefuse(t, g, request(clk, "req-1", "platform", 7000, 768), 503, decide.ReasonNoEligiblePod)
	if d.Est != 2768 {
		t.Fatalf("Est = %d, want 2768", d.Est)
	}

	scrapeReady(g, clk, "c", 100000, 0.7)
	d, tk := mustAdmit(t, g, request(clk, "req-2", "platform", 7000, 768))
	assertPlaced(t, d, tk, "c", decide.StickyNone, 2768)
	g.Settle(tk, Outcome{}, 7000)
}

func TestReservationsCountUntilSettledOrSuperseded(t *testing.T) {
	g, clk := newTestGate(DefaultConfig([]string{"a", "b"}))
	scrapeReady(g, clk, "a", 10000, 0)
	scrapeReady(g, clk, "b", 10000, 0)
	scrapedAt := clk.now()

	d1, tk1 := mustAdmit(t, g, request(clk, "req-1", "platform", 3500, 7000))
	assertPlaced(t, d1, tk1, "a", decide.StickyNone, 8000)
	assertReserved(t, g, "a", 8000)
	assertReserved(t, g, "b", 0)

	d2, tk2 := mustAdmit(t, g, request(clk, "req-2", "platform", 3500, 7000))
	assertPlaced(t, d2, tk2, "b", decide.StickyNone, 8000)
	assertReserved(t, g, "a", 8000)
	assertReserved(t, g, "b", 8000)

	mustRefuse(t, g, request(clk, "req-3", "platform", 3500, 7000), 503, decide.ReasonKVFree)

	g.Settle(tk1, Outcome{OK: true, PromptTokens: 1000, CompletionTokens: 10}, 3500)
	assertReserved(t, g, "a", 0)
	assertReserved(t, g, "b", 8000)

	g.Observe("b", Scrape{At: scrapedAt.Add(500 * time.Millisecond), KVPoolTokens: 10000})
	assertReserved(t, g, "b", 8000)

	g.Observe("b", Scrape{At: scrapedAt.Add(5 * time.Second), KVPoolTokens: 10000, KVUsage: 0.8})
	assertReserved(t, g, "b", 0)
	g.Settle(tk2, Outcome{OK: true, PromptTokens: 1000, CompletionTokens: 10}, 3500)
	assertReserved(t, g, "b", 0)
}

func TestTenantBucketChargesAndRefunds(t *testing.T) {
	cfg := DefaultConfig([]string{"a"})
	cfg.Tenants = map[string]TenantLimit{
		"platform": {RatePerS: 100, Burst: 5000},
		"*":        {RatePerS: 10, Burst: 3000},
	}
	g, clk := newTestGate(cfg)
	scrapeReady(g, clk, "a", 1000000, 0)

	d := mustRefuse(t, g, request(clk, "big", "platform", 3500, 7000), 429, decide.ReasonTenantTokens)
	if want := "need 8000 tokens, tenant has 5000 available at 100/s"; d.Placement.Verdict.Detail != want {
		t.Fatalf("Detail = %q, want %q", d.Placement.Verdict.Detail, want)
	}
	if d.Placement.Verdict.RetryAfter != 30*time.Second {
		t.Fatalf("RetryAfter = %v, want 30s", d.Placement.Verdict.RetryAfter)
	}

	dA, tkA := mustAdmit(t, g, request(clk, "A", "platform", 7000, 2000))
	assertPlaced(t, dA, tkA, "a", decide.StickyNone, 4000)

	d = mustRefuse(t, g, request(clk, "B", "platform", 3500, 1500), 429, decide.ReasonTenantTokens)
	if want := "need 2500 tokens, tenant has 1000 available at 100/s"; d.Placement.Verdict.Detail != want {
		t.Fatalf("Detail = %q, want %q", d.Placement.Verdict.Detail, want)
	}

	g.Settle(tkA, Outcome{OK: true, PromptTokens: 2000, CompletionTokens: 500}, 7000)
	dB, tkB := mustAdmit(t, g, request(clk, "B", "platform", 3500, 1500))
	assertPlaced(t, dB, tkB, "a", decide.StickyNone, 2500)
	g.Settle(tkB, Outcome{OK: true, PromptTokens: 1000, CompletionTokens: 10}, 3500)

	dX, tkX := mustAdmit(t, g, request(clk, "X", "tenant-x", 3500, 1500))
	assertPlaced(t, dX, tkX, "a", decide.StickyNone, 2500)
	d = mustRefuse(t, g, request(clk, "Y", "tenant-y", 3500, 1500), 429, decide.ReasonTenantTokens)
	if want := "need 2500 tokens, tenant has 500 available at 10/s"; d.Placement.Verdict.Detail != want {
		t.Fatalf("Detail = %q, want %q", d.Placement.Verdict.Detail, want)
	}
	g.Settle(tkX, Outcome{}, 3500)
}

func TestRestartBreaksBindingAndClearsReservations(t *testing.T) {
	g, clk := newTestGate(DefaultConfig([]string{"a", "b"}))
	scrapeReady(g, clk, "a", 100000, 0.2)
	scrapeReady(g, clk, "b", 100000, 0.5)

	d1, tk1 := mustAdmit(t, g, request(clk, "run1-s1", "platform", 7000, 768))
	assertPlaced(t, d1, tk1, "a", decide.StickyNew, 2768)
	assertReserved(t, g, "a", 2768)
	if got := g.Bound("run1"); got != "a" {
		t.Fatalf("Bound(run1) = %q, want a", got)
	}

	g.Restarted("a")
	if got := g.Bound("run1"); got != "" {
		t.Fatalf("Bound(run1) after restart = %q, want empty", got)
	}
	assertReserved(t, g, "a", 0)

	d2, tk2 := mustAdmit(t, g, request(clk, "run1-s2", "platform", 7000, 768))
	assertPlaced(t, d2, tk2, "b", decide.StickyNew, 2768)
	if got := g.Bound("run1"); got != "b" {
		t.Fatalf("Bound(run1) after rebind = %q, want b", got)
	}

	g.Settle(tk1, Outcome{}, 7000)
	g.Settle(tk2, Outcome{}, 7000)
	assertReserved(t, g, "a", 0)
	assertReserved(t, g, "b", 0)
}

func TestSecondAdmitWaitsForSettle(t *testing.T) {
	cfg := DefaultConfig([]string{"a"})
	cfg.MaxInflight = 1
	g, clk := newTestGate(cfg)
	scrapeReady(g, clk, "a", 1000000, 0)

	d1, tk1 := mustAdmit(t, g, request(clk, "req-1", "platform", 7000, 768))
	assertPlaced(t, d1, tk1, "a", decide.StickyNone, 2768)

	type result struct {
		d   Decision
		tk  *Ticket
		err error
	}
	done := make(chan result, 1)
	go func() {
		d, tk, err := g.Admit(context.Background(), request(clk, "req-2", "platform", 7000, 768))
		done <- result{d, tk, err}
	}()

	waitUntil := time.Now().Add(2 * time.Second)
	for viewOf(t, g, "a").Queued != 1 {
		if time.Now().After(waitUntil) {
			t.Fatal("second Admit never reached the queue")
		}
		time.Sleep(200 * time.Microsecond)
	}
	select {
	case res := <-done:
		t.Fatalf("second Admit returned before Settle: %+v", res)
	default:
	}
	if v := viewOf(t, g, "a"); v.InFlight != 1 || v.Queued != 1 {
		t.Fatalf("a InFlight/Queued = %d/%d, want 1/1", v.InFlight, v.Queued)
	}

	g.Settle(tk1, Outcome{OK: true, PromptTokens: 2000, CompletionTokens: 100}, 7000)
	var res result
	select {
	case res = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second Admit did not return after Settle")
	}
	if res.err != nil || res.tk == nil {
		t.Fatalf("second Admit = (%+v, %v, %v), want a ticket", res.d, res.tk, res.err)
	}
	assertPlaced(t, res.d, res.tk, "a", decide.StickyNone, 2768)
	if v := viewOf(t, g, "a"); v.InFlight != 1 || v.Queued != 0 {
		t.Fatalf("a InFlight/Queued = %d/%d, want 1/0", v.InFlight, v.Queued)
	}

	g.Settle(res.tk, Outcome{}, 7000)
	if v := viewOf(t, g, "a"); v.InFlight != 0 || v.ReservedTokens != 0 {
		t.Fatalf("a InFlight/ReservedTokens = %d/%d, want 0/0", v.InFlight, v.ReservedTokens)
	}
}

func TestSettleTwiceIsNoOp(t *testing.T) {
	cfg := DefaultConfig([]string{"a"})
	cfg.Tenants = map[string]TenantLimit{"platform": {RatePerS: 100, Burst: 5000}}
	g, clk := newTestGate(cfg)
	scrapeReady(g, clk, "a", 1000000, 0)

	dA, tkA := mustAdmit(t, g, request(clk, "A", "platform", 7000, 2000))
	assertPlaced(t, dA, tkA, "a", decide.StickyNone, 4000)

	g.Settle(tkA, Outcome{OK: true, PromptTokens: 2000, CompletionTokens: 1000}, 7000)
	g.Settle(tkA, Outcome{OK: true, PromptTokens: 2000, CompletionTokens: 1000}, 7000)

	if v := viewOf(t, g, "a"); v.InFlight != 0 || v.ReservedTokens != 0 {
		t.Fatalf("a InFlight/ReservedTokens = %d/%d, want 0/0", v.InFlight, v.ReservedTokens)
	}
	d := mustRefuse(t, g, request(clk, "B", "platform", 3500, 1500), 429, decide.ReasonTenantTokens)
	if want := "need 2500 tokens, tenant has 2000 available at 100/s"; d.Placement.Verdict.Detail != want {
		t.Fatalf("Detail = %q, want %q", d.Placement.Verdict.Detail, want)
	}
}

func TestConcurrentAdmitSettleLeavesNothingHeld(t *testing.T) {
	cfg := DefaultConfig([]string{"a", "b"})
	cfg.Tenants = map[string]TenantLimit{"platform": {RatePerS: 1000000, Burst: 1000000}}
	g, clk := newTestGate(cfg)
	scrapeReady(g, clk, "a", 1000000, 0)
	scrapeReady(g, clk, "b", 1000000, 0)

	var placed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := request(clk, "run"+strconv.Itoa(i)+"-s1", "platform", 3500, 768)
			_, tk, err := g.Admit(context.Background(), r)
			if err != nil || tk == nil {
				return
			}
			placed.Add(1)
			g.Settle(tk, Outcome{OK: true, PromptTokens: 1000, CompletionTokens: 100, Latency: 50 * time.Millisecond}, 3500)
		}(i)
	}
	wg.Wait()

	if got := placed.Load(); got != 50 {
		t.Fatalf("placed = %d, want 50", got)
	}
	for _, pod := range []string{"a", "b"} {
		v := viewOf(t, g, pod)
		if v.ReservedTokens != 0 || v.InFlight != 0 || v.Queued != 0 {
			t.Fatalf("%s ReservedTokens/InFlight/Queued = %d/%d/%d, want 0/0/0", pod, v.ReservedTokens, v.InFlight, v.Queued)
		}
	}
}
