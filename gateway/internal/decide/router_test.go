package decide

import (
	"maps"
	"testing"
	"time"
)

func TestRouterDrop(t *testing.T) {
	cases := []struct {
		status int
		header string
		reason Reason
	}{
		{429, "rejected-saturated", "router_saturated"},
		{429, "rejected-ttl-expired", "router_ttl_expired"},
		{429, "evicted", "router_evicted"},
		{429, "evicted-queue-pressure", "router_evicted"},
		{429, "evicted-priority", "router_evicted"},
		{503, "rejected-no-endpoints", "router_no_endpoints"},
		{503, "rejected-shutting-down", "router_shutting_down"},
		{503, "rejected-context-cancelled", "router_unknown"},
		{429, "rejected-something-new", "router_unknown"},
	}
	for _, c := range cases {
		t.Run(c.header, func(t *testing.T) {
			v, ok := RouterDrop(c.status, c.header)
			want := Verdict{
				Shed:       true,
				Code:       503,
				Reason:     c.reason,
				RetryAfter: time.Second,
				Detail:     "llm-d router dropped the request: " + c.header,
			}
			if !ok || v != want {
				t.Errorf("RouterDrop(%d, %q) = %+v, %v, want %+v, true", c.status, c.header, v, ok, want)
			}
			if _, leaves := MayLeave(Request{Class: Public}, v); !leaves {
				t.Errorf("a public %s refusal may not leave the box", c.reason)
			}
		})
	}
}

func TestRouterDropIgnoresAnswersThatAreNotCapacityRefusals(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header string
	}{
		{"internal error", 500, "rejected-internal"},
		{"429 without the header", 429, ""},
		{"503 without the header", 503, ""},
		{"200", 200, ""},
		{"a drop reason on a 200", 200, "rejected-saturated"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if v, ok := RouterDrop(c.status, c.header); ok || v != (Verdict{}) {
				t.Errorf("RouterDrop(%d, %q) = %+v, %v, want the zero Verdict, false", c.status, c.header, v, ok)
			}
		})
	}
}

func TestRouterHeaders(t *testing.T) {
	cases := []struct {
		priority  Priority
		objective string
	}{
		{Interactive, "doctor-interactive"},
		{Batch, "doctor-batch"},
	}
	for _, c := range cases {
		got := RouterHeaders(Request{Tenant: "acme", Priority: c.priority})
		want := map[string]string{
			"x-llm-d-inference-fairness-id": "acme",
			"x-llm-d-inference-objective":   c.objective,
		}
		if !maps.Equal(got, want) {
			t.Errorf("RouterHeaders(%s) = %v, want %v", c.priority, got, want)
		}
	}
}
