package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cduggn/cluster-doctor/gateway/internal/decide"
	"github.com/cduggn/cluster-doctor/gateway/internal/fleet"
	"github.com/cduggn/cluster-doctor/gateway/internal/serve"
)

var pods = []string{"vllm-0", "vllm-1"}

// fixture is a Metrics over a Gate and an idle Fleet for two pods at a
// fixed clock. The fleet never runs, so every phase stays Down and the test
// sets the gate's state directly.
func fixture(t *testing.T) (*Metrics, *fleet.Gate, time.Time) {
	t.Helper()
	now := time.Now()
	clock := func() time.Time { return now }
	gate := fleet.NewGate(fleet.DefaultConfig(pods), clock, func(int) int { return 0 })
	urls := map[string]string{"vllm-0": "http://127.0.0.1:1", "vllm-1": "http://127.0.0.1:2"}
	f := fleet.NewFleet(gate, urls, fleet.DefaultWorkerConfig(nil), nil, clock)
	return New(gate, f, "sliced", 3899), gate, now
}

// scrape serves one GET /metrics through Handler and returns the text.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d\n%s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// wantLines fails for every exposition line missing from body and prints
// the family's lines next to each miss.
func wantLines(t *testing.T, body string, lines ...string) {
	t.Helper()
	have := strings.Split(body, "\n")
	for _, want := range lines {
		if slices.Contains(have, want) {
			continue
		}
		family, _, _ := strings.Cut(want, "{")
		family, _, _ = strings.Cut(family, " ")
		var near []string
		for _, l := range have {
			if strings.HasPrefix(l, family) {
				near = append(near, l)
			}
		}
		t.Errorf("missing %q\nfamily has:\n%s", want, strings.Join(near, "\n"))
	}
}

func dur(ms int) time.Duration { return time.Duration(ms) * time.Millisecond }

// representative covers every outcome Observe distinguishes. A 200 with
// usage on vllm-0, three gateway refusals, a 502 with no worker answer and
// a 500 relayed from a stale vllm-1.
var representative = []serve.Event{
	{RequestID: "run-a-s2", Priority: decide.Interactive, Class: decide.Internal,
		Pod: "vllm-0", Policy: decide.PolicyPrefixThenLoad, Sticky: decide.StickyHit,
		Status: 200, Gateway: dur(3), Queue: dur(40), Upstream: dur(1200), Total: dur(1243),
		PromptTokens: 5000, CachedTokens: 4500, CompletionTokens: 120, FinishReason: "tool_calls"},
	{RequestID: "run-b-s1", Sticky: decide.StickyNew, Status: 429, Reason: "tenant_tokens",
		Gateway: dur(1), Total: dur(1)},
	{RequestID: "run-c-s3", Sticky: decide.StickyBrokenShed, Status: 503, Reason: "kv_free",
		Overflow: decide.OverflowBlocked, Gateway: dur(1), Total: dur(1)},
	{RequestID: "bad", Status: 400, Reason: "bad_json", Gateway: dur(1), Total: dur(1)},
	{RequestID: "run-d-s1", Pod: "vllm-1", Policy: decide.PolicyPrefixThenLoad, Sticky: decide.StickyNew,
		Status: 502, Reason: "upstream_error", Gateway: dur(2), Upstream: dur(115000), Total: dur(115002)},
	{RequestID: "", Pod: "vllm-1", Policy: decide.PolicyLeastLoaded, Sticky: decide.StickyNone, Unknown: true,
		Status: 500, Gateway: dur(2), Upstream: dur(500), Total: dur(502)},
}

func TestPreregisteredSeriesAppearBeforeAnyRequest(t *testing.T) {
	m, _, _ := fixture(t)
	body := scrape(t, m)
	wantLines(t, body,
		`orch_shed_total{code="429",reason="tenant_tokens"} 0`,
		`orch_shed_total{code="503",reason="kv_free"} 0`,
		`orch_shed_total{code="503",reason="timeout_queue"} 0`,
		`orch_shed_total{code="503",reason="p99_spread"} 0`,
		`orch_shed_total{code="503",reason="no_eligible_pod"} 0`,
		`orch_shed_total{code="503",reason="queue_full"} 0`,
		`orch_overflow_total{result="blocked_invariant"} 0`,
		`orch_overflow_total{result="no_backend"} 0`,
		`orch_restricted_offbox_total 0`,
		`# TYPE go_goroutines gauge`,
		`# TYPE process_cpu_seconds_total counter`,
	)
	if strings.Contains(body, "orch_pick_total{") {
		t.Errorf("orch_pick_total has a series before any request:\n%s", body)
	}
}

func TestObserveCountsEachOutcome(t *testing.T) {
	m, _, _ := fixture(t)
	for _, ev := range representative {
		m.Observe(ev)
	}
	body := scrape(t, m)
	wantLines(t, body,
		`orch_requests_total{class="internal",priority="interactive"} 1`,
		`orch_requests_total{class="restricted",priority="batch"} 5`,
		`orch_shed_total{code="429",reason="tenant_tokens"} 1`,
		`orch_shed_total{code="503",reason="kv_free"} 1`,
		`orch_shed_total{code="400",reason="bad_json"} 1`,
		`orch_shed_total{code="502",reason="upstream_error"} 1`,
		`orch_shed_total{code="503",reason="queue_full"} 0`,
		`orch_pick_total{pod="vllm-0",policy="prefix_then_load"} 1`,
		`orch_pick_total{pod="vllm-1",policy="prefix_then_load"} 1`,
		`orch_pick_total{pod="vllm-1",policy="least_loaded"} 1`,
		`orch_pick_unknown_snapshot_total{pod="vllm-1"} 1`,
		`orch_sticky_total{outcome="hit"} 1`,
		`orch_sticky_total{outcome="new"} 2`,
		`orch_sticky_total{outcome="broken_shed"} 1`,
		`orch_sticky_total{outcome="none"} 1`,
		`orch_completed_total{finish_reason="tool_calls",pod="vllm-0",status="200"} 1`,
		`orch_completed_total{finish_reason="",pod="vllm-1",status="500"} 1`,
		`orch_upstream_errors_total{kind="transport",pod="vllm-1"} 1`,
		`orch_upstream_errors_total{kind="status",pod="vllm-1"} 1`,
		`orch_prompt_tokens_total{kind="shared_hit",pod="vllm-0"} 3899`,
		`orch_prompt_tokens_total{kind="run_hit",pod="vllm-0"} 601`,
		`orch_prompt_tokens_total{kind="miss",pod="vllm-0"} 500`,
		`orch_completion_tokens_total{pod="vllm-0"} 120`,
		`orch_overflow_total{result="blocked_invariant"} 1`,
		`orch_overflow_total{result="no_backend"} 0`,
		`orch_restricted_offbox_total 0`,
		`orch_request_duration_seconds_count{stage="gateway"} 6`,
		`orch_request_duration_seconds_count{stage="e2e"} 6`,
		`orch_request_duration_seconds_count{stage="queue"} 1`,
		`orch_request_duration_seconds_sum{stage="queue"} 0.04`,
		`orch_request_duration_seconds_bucket{stage="queue",le="0.025"} 0`,
		`orch_request_duration_seconds_bucket{stage="queue",le="0.05"} 1`,
		`orch_request_duration_seconds_count{stage="local"} 3`,
		`orch_request_duration_seconds_bucket{stage="local",le="60"} 2`,
		`orch_request_duration_seconds_bucket{stage="local",le="120"} 3`,
	)
	for _, absent := range []string{`orch_shed_total{code="500"`, `orch_completed_total{finish_reason="",pod="",`, `pod=""`} {
		if strings.Contains(body, absent) {
			t.Errorf("found %q, which no outcome should produce", absent)
		}
	}
}

func TestCollectorDerivesReplicaGaugesFromGateAndFleet(t *testing.T) {
	m, gate, now := fixture(t)
	gate.Observe("vllm-0", fleet.Scrape{At: now.Add(-1500 * time.Millisecond), KVPoolTokens: 10000, KVUsage: 0.3})
	gate.Observe("vllm-1", fleet.Scrape{At: now.Add(-4 * time.Second), KVPoolTokens: 10000, KVUsage: 0.9})
	gate.SetReady("vllm-0", true)

	req := decide.Request{ID: "run-m-s1", Run: "run-m", Step: 1, Tenant: "platform", Priority: decide.Interactive,
		BodyBytes: 350, MaxOut: 100, Arrived: now, Deadline: now.Add(10 * time.Second)}
	d, first, err := gate.Admit(context.Background(), req)
	if err != nil || first == nil || d.Est != 200 {
		t.Fatalf("first Admit = %+v ticket %v err %v, want a ticket for 200 tokens", d, first, err)
	}
	gate.Settle(first, fleet.Outcome{OK: true, PromptTokens: 150, CompletionTokens: 50, Latency: 2 * time.Second}, 350)
	// Step 2 is estimated from step 1's settled usage, 150 + 50 plus the
	// 100 max out, so it reserves 300 tokens and leaves 6700 of 10000 free.
	req.ID, req.Step = "run-m-s2", 2
	if d, second, err := gate.Admit(context.Background(), req); err != nil || second == nil || d.Est != 300 {
		t.Fatalf("second Admit = %+v ticket %v err %v, want a ticket for 300 tokens", d, second, err)
	}

	wantLines(t, scrape(t, m),
		`orch_replica_healthy{pod="vllm-0",pool="sliced"} 1`,
		`orch_replica_healthy{pod="vllm-1",pool="sliced"} 0`,
		`orch_replica_warm{pod="vllm-0",pool="sliced"} 0`,
		`orch_replica_phase{phase="down",pod="vllm-0",pool="sliced"} 1`,
		`orch_replica_phase{phase="warming",pod="vllm-0",pool="sliced"} 0`,
		`orch_replica_phase{phase="ready",pod="vllm-0",pool="sliced"} 0`,
		`orch_replica_saturating{pod="vllm-0",pool="sliced"} 0`,
		`orch_replica_saturating{pod="vllm-1",pool="sliced"} 1`,
		`orch_replica_kv_free_ratio{pod="vllm-0",pool="sliced"} 0.67`,
		`orch_replica_kv_free_ratio{pod="vllm-1",pool="sliced"} 0.1`,
		`orch_replica_tokens_in_flight{pod="vllm-0",pool="sliced"} 300`,
		`orch_replica_tokens_in_flight{pod="vllm-1",pool="sliced"} 0`,
		`orch_replica_active_requests{pod="vllm-0",pool="sliced"} 1`,
		`orch_replica_queue_depth{pod="vllm-0",pool="sliced"} 0`,
		`orch_replica_snapshot_age_seconds{pod="vllm-0",pool="sliced"} 1.5`,
		`orch_replica_snapshot_age_seconds{pod="vllm-1",pool="sliced"} 4`,
		`orch_replica_mean_service_seconds{pod="vllm-0",pool="sliced"} 2`,
		`orch_replica_mean_service_seconds{pod="vllm-1",pool="sliced"} 0`,
		`orch_warmup_probe_seconds{pod="vllm-0",pool="sliced"} 0`,
		`orch_replica_restarts_total{pod="vllm-0",pool="sliced"} 0`,
		`orch_kv_free_ratio{pool="sliced"} 0.67`,
	)
}

func TestFleetKVFreeRatioIsZeroWithNoReadyPod(t *testing.T) {
	m, gate, now := fixture(t)
	gate.Observe("vllm-0", fleet.Scrape{At: now, KVPoolTokens: 10000, KVUsage: 0.3})
	wantLines(t, scrape(t, m), `orch_kv_free_ratio{pool="sliced"} 0`)
}

// exprsOf collects every "expr" and "query" string in a dashboard's JSON
// tree, through rows and nested panels.
func exprsOf(v any, out *[]string) {
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			if s, ok := child.(string); ok && (k == "expr" || k == "query") {
				*out = append(*out, s)
				continue
			}
			exprsOf(child, out)
		}
	case []any:
		for _, child := range node {
			exprsOf(child, out)
		}
	}
}

func TestDashboardReadsOnlyMetricsTheGatewayExports(t *testing.T) {
	raw, err := os.ReadFile("../../../deploy/observability/dashboards/gateway.json")
	if err != nil {
		t.Fatal(err)
	}
	var dash map[string]any
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("gateway.json is not JSON: %v", err)
	}
	if dash["uid"] != "cd-gateway" {
		t.Errorf("uid = %v, want cd-gateway", dash["uid"])
	}

	var exprs []string
	exprsOf(dash, &exprs)
	if len(exprs) < 14 {
		t.Fatalf("found %d exprs, want at least one per panel", len(exprs))
	}
	token := regexp.MustCompile(`orch_[a-z_]+`)
	used := map[string]bool{}
	for _, e := range exprs {
		for _, name := range token.FindAllString(e, -1) {
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				name = strings.TrimSuffix(name, suffix)
			}
			used[name] = true
			if !slices.Contains(MetricNames, name) {
				t.Errorf("dashboard reads %s, which the gateway does not export (expr %q)", name, e)
			}
		}
	}
	for _, name := range []string{"orch_requests_total", "orch_shed_total", "orch_replica_kv_free_ratio", "orch_restricted_offbox_total"} {
		if !used[name] {
			t.Errorf("dashboard never reads %s", name)
		}
	}

	m, gate, now := fixture(t)
	gate.Observe("vllm-0", fleet.Scrape{At: now, KVPoolTokens: 10000, KVUsage: 0.3})
	for _, ev := range representative {
		m.Observe(ev)
	}
	body := scrape(t, m)
	for _, name := range MetricNames {
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("MetricNames lists %s but /metrics does not export it", name)
		}
	}
}
