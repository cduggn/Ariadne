package decide

import (
	"testing"
	"time"
)

// freshWorker returns a Ready, unstale, lightly loaded worker: 0.9 KV free,
// no in-flight or queued requests, MaxInflight 16. Tests override only the
// fields that matter to them.
func freshWorker(pod string) WorkerState {
	return WorkerState{
		Ready: true,
		Age:   0,
		View: WorkerView{
			Pod:          pod,
			KVPoolTokens: 10000,
			KVUsage:      0.1,
			MaxInflight:  16,
			MeanServiceS: 1.0,
		},
	}
}

func withInFlight(w WorkerState, n int) WorkerState {
	w.View.InFlight = n
	return w
}

func withKVUsage(w WorkerState, usage float64) WorkerState {
	w.View.KVUsage = usage
	return w
}

func withAge(w WorkerState, age time.Duration) WorkerState {
	w.Age = age
	return w
}

func notReady(w WorkerState) WorkerState {
	w.Ready = false
	return w
}

// reqRun and reqNoRun are requests with a generous deadline, so only the
// gate under test can shed. reqRun carries a run with no binding of its own;
// stickiness comes entirely from Fleet.Bound.
func reqRun(run string) Request {
	return Request{Run: RunID(run), Priority: Batch, Deadline: fixedNow.Add(30 * time.Second)}
}

func reqNoRun() Request {
	return Request{Priority: Batch, Deadline: fixedNow.Add(30 * time.Second)}
}

func fleetOf(workers ...WorkerState) Fleet {
	return Fleet{
		Now:        fixedNow,
		Workers:    workers,
		Tenant:     ampleTenant,
		EstTokens:  100,
		Shed:       DefaultPolicy,
		Staleness:  DefaultStaleness,
		StickSlack: DefaultStickSlack,
	}
}

// assertPlacement checks Pick's result against the literal fields the
// routing contract promises: which pod (if any), the shed code and reason,
// the stickiness label and whether the chosen worker was stale. It also
// checks Placed() agrees with whether a pod was chosen, since that method
// exists only to name that same fact.
func assertPlacement(t *testing.T, got Placement, wantPod string, wantCode int, wantReason Reason, wantSticky Sticky, wantUnknown bool) {
	t.Helper()
	if got.Pod != wantPod {
		t.Errorf("Pick() Pod = %q, want %q", got.Pod, wantPod)
	}
	if got.Verdict.Code != wantCode {
		t.Errorf("Pick() Verdict.Code = %d, want %d", got.Verdict.Code, wantCode)
	}
	if got.Verdict.Reason != wantReason {
		t.Errorf("Pick() Verdict.Reason = %q, want %q", got.Verdict.Reason, wantReason)
	}
	if got.Sticky != wantSticky {
		t.Errorf("Pick() Sticky = %q, want %q", got.Sticky, wantSticky)
	}
	if got.Unknown != wantUnknown {
		t.Errorf("Pick() Unknown = %v, want %v", got.Unknown, wantUnknown)
	}
	if got.Placed() != (wantPod != "") {
		t.Errorf("Pick() Placed() = %v, want %v", got.Placed(), wantPod != "")
	}
}

func noRnd(t *testing.T) func(int) int {
	return func(n int) int {
		t.Fatalf("rnd should not be called, got n=%d", n)
		return 0
	}
}

func TestPickNewRunToLeastLoaded(t *testing.T) {
	f := fleetOf(
		freshWorker("pod-a"),
		withInFlight(freshWorker("pod-b"), 10),
	)
	got := Pick(reqRun("run1"), f, PolicyPrefixThenLoad, noRnd(t))
	assertPlacement(t, got, "pod-a", 0, "", StickyNew, false)
}

func TestPickStickyHit(t *testing.T) {
	f := fleetOf(
		withInFlight(freshWorker("pod-a"), 3), // Load 0.2875
		freshWorker("pod-b"),                  // Load 0.1
	)
	f.Bound = "pod-a"
	got := Pick(reqRun("run1"), f, PolicyPrefixThenLoad, noRnd(t))
	assertPlacement(t, got, "pod-a", 0, "", StickyHit, false)
}

func TestPickStickyBrokenLoadOverSlack(t *testing.T) {
	f := fleetOf(
		withInFlight(freshWorker("pod-a"), 8), // Load 0.6, 0.5 over pod-b
		freshWorker("pod-b"),                  // Load 0.1
	)
	f.Bound = "pod-a"
	got := Pick(reqRun("run1"), f, PolicyPrefixThenLoad, noRnd(t))
	assertPlacement(t, got, "pod-b", 0, "", StickyBrokenLoad, false)
}

func TestPickStickyKeptWithinSlack(t *testing.T) {
	// pod-a is exactly StickSlack (0.25 = 4/16) busier than pod-b, so Pick keeps it.
	f := fleetOf(
		withInFlight(freshWorker("pod-a"), 4),
		freshWorker("pod-b"),
	)
	f.Bound = "pod-a"
	got := Pick(reqRun("run1"), f, PolicyPrefixThenLoad, noRnd(t))
	assertPlacement(t, got, "pod-a", 0, "", StickyHit, false)
}

func TestPickBrokenShedWhenBoundIsKVFull(t *testing.T) {
	// 0.03 free is below the 0.05 resident floor, so the bound worker itself
	// refuses even though it is exempt from the ordinary 0.20 line.
	f := fleetOf(
		withKVUsage(freshWorker("pod-a"), 0.97),
		freshWorker("pod-b"),
	)
	f.Bound = "pod-a"
	got := Pick(reqRun("run1"), f, PolicyPrefixThenLoad, noRnd(t))
	assertPlacement(t, got, "pod-b", 0, "", StickyBrokenShed, false)
}

func TestPickBrokenGoneWhenBoundNotReady(t *testing.T) {
	f := fleetOf(
		notReady(freshWorker("pod-a")),
		freshWorker("pod-b"),
	)
	f.Bound = "pod-a"
	got := Pick(reqRun("run1"), f, PolicyPrefixThenLoad, noRnd(t))
	assertPlacement(t, got, "pod-b", 0, "", StickyBrokenGone, false)
}

func TestPickExcludesNotReadyAndDownWorkers(t *testing.T) {
	f := fleetOf(
		notReady(freshWorker("pod-a")),
		withAge(freshWorker("pod-b"), 10*time.Second), // Down: Age >= DownAfter
		freshWorker("pod-c"),
	)
	got := Pick(reqNoRun(), f, PolicyPrefixThenLoad, noRnd(t))
	assertPlacement(t, got, "pod-c", 0, "", StickyNone, false)
}

func TestPickStaleFloorTwoWorkersBothStay(t *testing.T) {
	// fresh/total = 16/32 = 0.5, below the 0.75 floor, so the stale worker
	// stays and Pick chooses it as the less loaded one. Unknown reports that
	// Pick placed on stale telemetry.
	f := fleetOf(
		withAge(freshWorker("pod-a"), 3*time.Second), // stale: 2s <= Age < 10s
		withInFlight(freshWorker("pod-b"), 8),
	)
	got := Pick(reqNoRun(), f, PolicyPrefixThenLoad, noRnd(t))
	assertPlacement(t, got, "pod-a", 0, "", StickyNone, true)
}

func TestPickStaleFloorEightWorkersTwoDropped(t *testing.T) {
	// fresh/total = 96/128 = 0.75, at the floor, so Pick drops both stale
	// workers even though they are the least loaded candidates.
	workers := []WorkerState{
		withAge(freshWorker("stale-0"), 3*time.Second),
		withAge(freshWorker("stale-1"), 3*time.Second),
		withInFlight(freshWorker("fresh-0"), 0), // least loaded fresh worker
		withInFlight(freshWorker("fresh-1"), 4),
		withInFlight(freshWorker("fresh-2"), 4),
		withInFlight(freshWorker("fresh-3"), 4),
		withInFlight(freshWorker("fresh-4"), 4),
		withInFlight(freshWorker("fresh-5"), 4),
	}
	f := fleetOf(workers...)
	got := Pick(reqNoRun(), f, PolicyPrefixThenLoad, noRnd(t))
	assertPlacement(t, got, "fresh-0", 0, "", StickyNone, false)
}

func TestPickAllDownIsNoEligiblePod(t *testing.T) {
	f := fleetOf(
		withAge(freshWorker("pod-a"), 10*time.Second),
		withAge(freshWorker("pod-b"), 20*time.Second),
	)
	got := Pick(reqRun("run1"), f, PolicyPrefixThenLoad, noRnd(t))
	assertPlacement(t, got, "", 503, ReasonNoEligiblePod, StickyNew, false)
	if got.Verdict.RetryAfter != 2*time.Second {
		t.Errorf("RetryAfter = %v, want 2s", got.Verdict.RetryAfter)
	}
}

func TestPickTenantExhaustedOutranksKVFull(t *testing.T) {
	f := fleetOf(
		withKVUsage(freshWorker("pod-a"), 0.97), // also kv_free-refused
		freshWorker("pod-b"),
	)
	f.Tenant = TenantState{AvailableTokens: 1, RatePerS: 1000}
	got := Pick(reqNoRun(), f, PolicyPrefixThenLoad, noRnd(t))
	assertPlacement(t, got, "", 429, ReasonTenantTokens, StickyNone, false)
}

func TestPickAllKVFullIsKVFree(t *testing.T) {
	f := fleetOf(
		withKVUsage(freshWorker("pod-a"), 0.9),
		withKVUsage(freshWorker("pod-b"), 0.85),
	)
	got := Pick(reqNoRun(), f, PolicyPrefixThenLoad, noRnd(t))
	assertPlacement(t, got, "", 503, ReasonKVFree, StickyNone, false)
}

func TestPickLeastLoadedTieBrokenByPodName(t *testing.T) {
	f := fleetOf(
		freshWorker("pod-b"),
		freshWorker("pod-a"),
	)
	got := Pick(reqNoRun(), f, PolicyLeastLoaded, noRnd(t))
	assertPlacement(t, got, "pod-a", 0, "", StickyNone, false)
}

func TestPickLeastLoadedReportsStickyForMetricsOnly(t *testing.T) {
	// Bound is admitted but busier than pod-b; least_loaded still takes
	// pod-b, but reports broken_load rather than hit, purely for metrics.
	f := fleetOf(
		withInFlight(freshWorker("pod-a"), 8),
		freshWorker("pod-b"),
	)
	f.Bound = "pod-a"
	got := Pick(reqRun("run1"), f, PolicyLeastLoaded, noRnd(t))
	assertPlacement(t, got, "pod-b", 0, "", StickyBrokenLoad, false)
}

func TestPickP2CSamplesTwoAndTakesTheLessLoaded(t *testing.T) {
	// pod-c is the global minimum, but a rigged rnd samples only pod-a and
	// pod-b, so p2c must return the lower of that pair, not the fleet min.
	f := fleetOf(
		freshWorker("pod-a"),                  // Load 0.1
		withInFlight(freshWorker("pod-b"), 8), // Load 0.6
		withInFlight(freshWorker("pod-c"), 0), // Load 0.1, tied lowest overall
	)

	var calls []int
	rets := []int{0, 0} // i := rnd(3) -> 0 (pod-a); j := rnd(2) -> 0, bumped to 1 (pod-b)
	call := 0
	rnd := func(n int) int {
		calls = append(calls, n)
		v := rets[call]
		call++
		return v
	}

	got := Pick(reqNoRun(), f, PolicyP2C, rnd)
	assertPlacement(t, got, "pod-a", 0, "", StickyNone, false)

	wantCalls := []int{3, 2}
	if len(calls) != len(wantCalls) || calls[0] != wantCalls[0] || calls[1] != wantCalls[1] {
		t.Errorf("rnd calls = %v, want %v", calls, wantCalls)
	}
}

func TestPickP2CNeverCallsRndForOneCandidate(t *testing.T) {
	f := fleetOf(freshWorker("pod-a"))
	got := Pick(reqNoRun(), f, PolicyP2C, noRnd(t))
	assertPlacement(t, got, "pod-a", 0, "", StickyNone, false)
}

func TestParsePickPolicy(t *testing.T) {
	cases := []struct {
		in     string
		want   PickPolicy
		wantOK bool
	}{
		{"prefix_then_load", PolicyPrefixThenLoad, true},
		{"least_loaded", PolicyLeastLoaded, true},
		{"p2c", PolicyP2C, true},
		{" P2C ", PolicyP2C, true},
		{"Least_Loaded", PolicyLeastLoaded, true},
		{"", "", false},
		{"random", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := ParsePickPolicy(tc.in)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("ParsePickPolicy(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
