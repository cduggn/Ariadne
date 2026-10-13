package metrics

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cduggn/ariadne/gateway/internal/decide"
	"github.com/cduggn/ariadne/gateway/internal/overflow"
	"github.com/cduggn/ariadne/gateway/internal/serve"
)

func TestOverflowSeriesArePreregisteredAndTheBackendReadsUnavailable(t *testing.T) {
	m, _, _ := fixture(t)
	wantLines(t, scrape(t, m),
		`orch_overflow_total{result="blocked_invariant"} 0`,
		`orch_overflow_total{result="no_backend"} 0`,
		`orch_overflow_total{result="sent"} 0`,
		`orch_overflow_total{result="failed"} 0`,
		`orch_overflow_total{result="unavailable"} 0`,
		`orch_overflow_total{result="capped"} 0`,
		`orch_overflow_backend_available 0`,
		`orch_restricted_offbox_total 0`,
	)
}

func TestObserveCountsOverflowOutcomesAndTheirTime(t *testing.T) {
	m, _, _ := fixture(t)
	shed := serve.Event{Status: 503, Reason: "kv_free", Class: decide.Internal, Sticky: decide.StickyNew, Gateway: dur(1), Total: dur(1)}
	sent := serve.Event{Status: 200, Class: decide.Internal, Sticky: decide.StickyNew, Overflow: overflow.Sent,
		OverflowTime: 800 * time.Millisecond, Gateway: dur(1), Total: dur(801), CompletionTokens: 30, FinishReason: "tool_calls"}
	failed, unavailable, capped := shed, shed, shed
	failed.Overflow, failed.OverflowTime = overflow.Failed, 200*time.Millisecond
	unavailable.Overflow = overflow.Unavailable
	capped.Overflow = overflow.Capped
	for _, ev := range []serve.Event{sent, failed, unavailable, capped} {
		m.Observe(ev)
	}
	body := scrape(t, m)
	wantLines(t, body,
		`orch_overflow_total{result="sent"} 1`,
		`orch_overflow_total{result="failed"} 1`,
		`orch_overflow_total{result="unavailable"} 1`,
		`orch_overflow_total{result="capped"} 1`,
		`orch_overflow_total{result="blocked_invariant"} 0`,
		`orch_shed_total{code="503",reason="kv_free"} 3`,
		`orch_completed_total{finish_reason="tool_calls",pod="overflow",status="200"} 1`,
		`orch_request_duration_seconds_count{stage="overflow"} 2`,
		`orch_request_duration_seconds_sum{stage="overflow"} 1`,
		`orch_restricted_offbox_total 0`,
	)
	for _, absent := range []string{`pod=""`, `orch_pick_total{pod="overflow"`, `orch_prompt_tokens_total{kind="miss",pod="overflow"`} {
		if strings.Contains(body, absent) {
			t.Errorf("found %q, which an overflowed answer must not produce", absent)
		}
	}
}

func TestARestrictedAnswerFromOffBoxWouldCount(t *testing.T) {
	// Unreachable through the gateway (decide.MayLeave, the permit, the
	// backend's own check), but if it ever happened the alert must fire.
	m, _, _ := fixture(t)
	m.Observe(serve.Event{Status: 200, Class: decide.Restricted, Overflow: overflow.Sent, OverflowTime: dur(5)})
	wantLines(t, scrape(t, m), `orch_restricted_offbox_total 1`)
}

func TestWatchOverflowFollowsTheCircuit(t *testing.T) {
	m, _, _ := fixture(t)
	ov := overflow.New(overflow.Null{}, 0, 1, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.WatchOverflow(ov)
	ov.Probe(context.Background())
	wantLines(t, scrape(t, m), `orch_overflow_backend_available 0`)
}
