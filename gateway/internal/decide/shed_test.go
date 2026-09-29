package decide

import (
	"testing"
	"time"
)

var (
	fixedNow    = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	ampleTenant = TenantState{AvailableTokens: 1_000_000, RatePerS: 1000}
)

// assertVerdict checks a ShouldShed result against the literal fields the
// gate contract promises: Shed, Code, Reason and RetryAfter. Detail is a
// human-readable string, not part of that contract; it is only checked for
// presence on a refusal. An admit must equal the zero Verdict exactly, per
// "Admit is Verdict{}".
func assertVerdict(t *testing.T, got Verdict, wantShed bool, wantCode int, wantReason Reason, wantRetry time.Duration) {
	t.Helper()
	if !wantShed {
		if got != (Verdict{}) {
			t.Errorf("ShouldShed() = %+v, want Verdict{} (admit)", got)
		}
		return
	}
	if got.Shed != wantShed || got.Code != wantCode || got.Reason != wantReason || got.RetryAfter != wantRetry {
		t.Errorf("ShouldShed() = %+v, want {Shed:%v Code:%d Reason:%q RetryAfter:%v}",
			got, wantShed, wantCode, wantReason, wantRetry)
	}
	if got.Detail == "" {
		t.Error("Detail is empty on a shed verdict, want a human-readable message")
	}
}

func TestShouldShedAdmitOnHealthyWorker(t *testing.T) {
	r := Request{Priority: Interactive, Deadline: fixedNow.Add(10 * time.Second)}
	s := Snap{
		Now:    fixedNow,
		Tenant: TenantState{AvailableTokens: 100000, RatePerS: 1000},
		Worker: WorkerView{
			KVPoolTokens: 10000, KVUsage: 0.5,
			InFlight: 5, MaxInflight: 16,
			MeanServiceS: 1.0, TTFTp50S: 0.1, TTFTp99S: 0.2,
		},
		EstTokens: 500,
		Policy:    DefaultPolicy,
	}
	assertVerdict(t, ShouldShed(r, s), false, 0, "", 0)
}

func TestShouldShedTenantTokens(t *testing.T) {
	// Every case gives the worker a fully saturated KV pool, so a request
	// that clears the tenant gate would still be shed by kv_free. A tenant
	// refusal in every case therefore also proves gate order: tenant_tokens
	// outranks a simultaneously full worker.
	saturated := WorkerView{KVPoolTokens: 10000, KVUsage: 1.0, InFlight: 0, MaxInflight: 16}
	deadline := fixedNow.Add(30 * time.Second)

	cases := []struct {
		name      string
		tenant    TenantState
		estTokens int
		wantRetry time.Duration
	}{
		{"retry-after arithmetic", TenantState{AvailableTokens: 4000, RatePerS: 1000}, 10000, 6 * time.Second},
		{"retry-after clamps to a 1s minimum", TenantState{AvailableTokens: 1000.5, RatePerS: 1000000}, 1001, time.Second},
		{"retry-after clamps to a 60s maximum", TenantState{AvailableTokens: 0, RatePerS: 1}, 100000, 60 * time.Second},
		{"zero rate defaults to 60s", TenantState{AvailableTokens: 100, RatePerS: 0}, 5000, 60 * time.Second},
		{"negative rate defaults to 60s", TenantState{AvailableTokens: 100, RatePerS: -5}, 5000, 60 * time.Second},
		{"wins over a simultaneously full kv pool", TenantState{AvailableTokens: 100, RatePerS: 1000}, 5000, 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Request{Priority: Batch, Deadline: deadline}
			s := Snap{Now: fixedNow, Tenant: tc.tenant, Worker: saturated, EstTokens: tc.estTokens, Policy: DefaultPolicy}
			assertVerdict(t, ShouldShed(r, s), true, 429, ReasonTenantTokens, tc.wantRetry)
		})
	}
}

func TestShouldShedKVFree(t *testing.T) {
	deadline := fixedNow.Add(30 * time.Second)
	r := Request{Priority: Batch, Deadline: deadline}

	cases := []struct {
		name      string
		worker    WorkerView
		resident  bool
		estTokens int
		wantShed  bool
	}{
		{
			name:      "new run sheds at 0.19 free, below the line",
			worker:    WorkerView{KVPoolTokens: 10000, KVUsage: 0.81, InFlight: 0, MaxInflight: 16},
			resident:  false,
			estTokens: 100,
			wantShed:  true,
		},
		{
			name:      "resident continuation admitted at 0.19 free",
			worker:    WorkerView{KVPoolTokens: 10000, KVUsage: 0.81, InFlight: 0, MaxInflight: 16},
			resident:  true,
			estTokens: 100,
			wantShed:  false,
		},
		{
			name:      "resident continuation sheds at 0.04 free, below the floor",
			worker:    WorkerView{KVPoolTokens: 10000, KVUsage: 0.96, InFlight: 0, MaxInflight: 16},
			resident:  true,
			estTokens: 100,
			wantShed:  true,
		},
		{
			name:      "estimate above free tokens sheds even above the line",
			worker:    WorkerView{KVPoolTokens: 1000, KVUsage: 0.5, InFlight: 0, MaxInflight: 16},
			resident:  false,
			estTokens: 600,
			wantShed:  true,
		},
		{
			name:      "reserved tokens shrink free enough to shed",
			worker:    WorkerView{KVPoolTokens: 1000, KVUsage: 0.5, ReservedTokens: 350, InFlight: 0, MaxInflight: 16},
			resident:  false,
			estTokens: 100,
			wantShed:  true,
		},
		{
			name:      "same pool with no reservation admits",
			worker:    WorkerView{KVPoolTokens: 1000, KVUsage: 0.5, ReservedTokens: 0, InFlight: 0, MaxInflight: 16},
			resident:  false,
			estTokens: 100,
			wantShed:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Snap{Now: fixedNow, Tenant: ampleTenant, Worker: tc.worker, Resident: tc.resident, EstTokens: tc.estTokens, Policy: DefaultPolicy}
			got := ShouldShed(r, s)
			if tc.wantShed {
				assertVerdict(t, got, true, 503, ReasonKVFree, 2*time.Second)
			} else {
				assertVerdict(t, got, false, 0, "", 0)
			}
		})
	}
}

func TestShouldShedTimeoutQueue(t *testing.T) {
	// A generous KV pool on every case, so only the timeout_queue gate can
	// refuse.
	roomyKV := func(w WorkerView) WorkerView {
		w.KVPoolTokens = 10000
		w.KVUsage = 0.1
		return w
	}

	cases := []struct {
		name     string
		worker   WorkerView
		deadline time.Time
		wantShed bool
	}{
		{
			name:     "not full, deadline already passed, sheds",
			worker:   roomyKV(WorkerView{InFlight: 1, MaxInflight: 16, MeanServiceS: 5.0}),
			deadline: fixedNow.Add(-1 * time.Second),
			wantShed: true,
		},
		{
			name:     "not full, deadline ahead, admits",
			worker:   roomyKV(WorkerView{InFlight: 1, MaxInflight: 16, MeanServiceS: 5.0}),
			deadline: fixedNow.Add(10 * time.Second),
			wantShed: false,
		},
		{
			name:     "full, projected wait exceeds deadline, sheds",
			worker:   roomyKV(WorkerView{InFlight: 16, MaxInflight: 16, Queued: 5, MeanServiceS: 2.0}),
			deadline: fixedNow.Add(5 * time.Second),
			wantShed: true,
		},
		{
			name:     "full, projected wait within deadline, admits",
			worker:   roomyKV(WorkerView{InFlight: 16, MaxInflight: 16, Queued: 5, MeanServiceS: 2.0}),
			deadline: fixedNow.Add(20 * time.Second),
			wantShed: false,
		},
		{
			name:     "full with unknown service time never sheds",
			worker:   roomyKV(WorkerView{InFlight: 16, MaxInflight: 16, Queued: 5, MeanServiceS: 0}),
			deadline: fixedNow.Add(1 * time.Second),
			wantShed: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Request{Priority: Batch, Deadline: tc.deadline}
			s := Snap{Now: fixedNow, Tenant: ampleTenant, Worker: tc.worker, Policy: DefaultPolicy}
			got := ShouldShed(r, s)
			if tc.wantShed {
				assertVerdict(t, got, true, 503, ReasonTimeoutQueue, time.Second)
			} else {
				assertVerdict(t, got, false, 0, "", 0)
			}
		})
	}
}

func TestShouldShedP99Spread(t *testing.T) {
	deadline := fixedNow.Add(30 * time.Second)
	roomyWorker := WorkerView{KVPoolTokens: 10000, KVUsage: 0.1, InFlight: 0, MaxInflight: 16}

	cases := []struct {
		name          string
		priority      Priority
		ttftP50       float64
		ttftP99       float64
		waitingRising bool
		wantShed      bool
	}{
		{"sheds batch with spread and a rising queue", Batch, 0.1, 0.5, true, true},
		{"never sheds interactive with the same spread", Interactive, 0.1, 0.5, true, false},
		{"batch without a rising queue admits despite the spread", Batch, 0.1, 0.5, false, false},
		{"batch with unknown p50 admits despite a large p99", Batch, 0, 5.0, true, false},
		{"batch exactly at the spread ratio does not shed", Batch, 0.1, 0.4, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := roomyWorker
			w.TTFTp50S = tc.ttftP50
			w.TTFTp99S = tc.ttftP99
			w.WaitingRising = tc.waitingRising

			r := Request{Priority: tc.priority, Deadline: deadline}
			s := Snap{Now: fixedNow, Tenant: ampleTenant, Worker: w, Policy: DefaultPolicy}
			got := ShouldShed(r, s)
			if tc.wantShed {
				assertVerdict(t, got, true, 503, ReasonP99Spread, 5*time.Second)
			} else {
				assertVerdict(t, got, false, 0, "", 0)
			}
		})
	}
}

func TestVerdictStays(t *testing.T) {
	cases := []struct {
		name string
		v    Verdict
		want bool
	}{
		{"429 stays local", Verdict{Shed: true, Code: 429}, true},
		{"503 may leave", Verdict{Shed: true, Code: 503}, false},
		{"admit does not stay", Verdict{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.Stays(); got != tc.want {
				t.Errorf("%+v.Stays() = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

func TestWorkerViewFreeTokensAndRatio(t *testing.T) {
	cases := []struct {
		name      string
		worker    WorkerView
		wantFree  int
		wantRatio float64
	}{
		{"no reservation", WorkerView{KVPoolTokens: 1000, KVUsage: 0.5}, 500, 0.5},
		{"reservation reduces free", WorkerView{KVPoolTokens: 1000, KVUsage: 0.5, ReservedTokens: 200}, 300, 0.3},
		{"reservation past scraped free clamps to zero", WorkerView{KVPoolTokens: 1000, KVUsage: 0.5, ReservedTokens: 900}, 0, 0},
		{"empty pool reports zero ratio", WorkerView{KVPoolTokens: 0, KVUsage: 0.5}, 0, 0},
		{"usage rounds to the nearest token", WorkerView{KVPoolTokens: 3, KVUsage: 0.5}, 2, float64(2) / float64(3)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.worker.FreeTokens(); got != tc.wantFree {
				t.Errorf("FreeTokens() = %d, want %d", got, tc.wantFree)
			}
			if got := tc.worker.FreeRatio(); got != tc.wantRatio {
				t.Errorf("FreeRatio() = %v, want %v", got, tc.wantRatio)
			}
		})
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		name  string
		req   Request
		prior *RunUsage
		want  int
	}{
		{
			name: "no prior estimates from body bytes alone",
			req:  Request{BodyBytes: 35000, MaxOut: 768},
			want: 10768,
		},
		{
			name:  "with prior anchors to real usage plus growth",
			req:   Request{BodyBytes: 35000, MaxOut: 768},
			prior: &RunUsage{PromptTokens: 9000, CompletionTokens: 60, BodyBytes: 30000},
			want:  11257,
		},
		{
			name:  "shrinking body clamps growth to zero",
			req:   Request{BodyBytes: 35000, MaxOut: 768},
			prior: &RunUsage{PromptTokens: 9000, CompletionTokens: 60, BodyBytes: 40000},
			want:  9828,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EstimateTokens(tc.req, tc.prior); got != tc.want {
				t.Errorf("EstimateTokens() = %d, want %d", got, tc.want)
			}
		})
	}
}
