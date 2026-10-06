package hop

import (
	"strings"
	"testing"
	"time"

	"github.com/cduggn/cluster-doctor/gateway/internal/decide"
)

// rule is a Config with round numbers: 1 MB per token over 1 GB/s is 1 ms
// per token to copy, and 1,000 tokens/s is 1 ms per token to recompute, so
// the overhead alone decides a tie.
var rule = Config{
	MinTokens:          1000,
	SharedPrefixTokens: 200,
	KVBytesPerToken:    1_000_000,
	TransferBytesPerS:  1e9,
	PrefillTokensPerS:  1000,
	Overhead:           10 * time.Millisecond,
	Timeout:            time.Second,
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name    string
		cfg     func(*Config)
		history int
		want    Plan
	}{
		{
			name:    "history below the threshold recomputes",
			history: 999,
			want:    Plan{Outcome: BelowThreshold, Tokens: 799, HopCost: 809 * time.Millisecond, RecomputeCost: 799 * time.Millisecond},
		},
		{
			name:    "equal per-token costs lose to the overhead",
			history: 1200,
			want:    Plan{Outcome: RecomputeCheaper, Tokens: 1000, HopCost: 1010 * time.Millisecond, RecomputeCost: time.Second},
		},
		{
			name:    "a faster link hops",
			cfg:     func(c *Config) { c.TransferBytesPerS = 2e9 },
			history: 1200,
			want:    Plan{Hop: true, Tokens: 1000, HopCost: 510 * time.Millisecond, RecomputeCost: time.Second},
		},
		{
			name:    "a history inside the shared prefix costs nothing either way",
			cfg:     func(c *Config) { c.MinTokens = 0 },
			history: 150,
			want:    Plan{Outcome: RecomputeCheaper, HopCost: 10 * time.Millisecond},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := rule
			if tt.cfg != nil {
				tt.cfg(&c)
			}
			if got := c.Decide(tt.history); got != tt.want {
				t.Errorf("Decide(%d) = %+v, want %+v", tt.history, got, tt.want)
			}
		})
	}
}

func TestDefaultConfigHopsALongHistoryAndSkipsAShortOne(t *testing.T) {
	if err := DefaultConfig.Validate(); err != nil {
		t.Fatalf("DefaultConfig.Validate() = %v", err)
	}
	if p := DefaultConfig.Decide(4000); p.Outcome != BelowThreshold {
		t.Errorf("Decide(4000) = %+v, want below_threshold", p)
	}
	if p := DefaultConfig.Decide(16000); !p.Hop {
		t.Errorf("Decide(16000) = %+v, want a hop", p)
	}
}

func TestValidateNamesEveryBadField(t *testing.T) {
	err := Config{MinTokens: -1, SharedPrefixTokens: -1, Overhead: -1}.Validate()
	if err == nil {
		t.Fatal("Validate() = nil for a zero config")
	}
	for _, want := range []string{"min tokens", "shared prefix", "KV bytes", "transfer rate", "prefill rate", "overhead", "timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() = %q, missing %q", err, want)
		}
	}
}

func TestMovedOnlyWhenTheOldWorkerStillHoldsTheHistory(t *testing.T) {
	want := map[decide.Sticky]bool{
		decide.StickyNone:       false,
		decide.StickyNew:        false,
		decide.StickyHit:        false,
		decide.StickyBrokenLoad: true,
		decide.StickyBrokenShed: true,
		decide.StickyBrokenGone: false,
	}
	for s, w := range want {
		if got := Moved(s); got != w {
			t.Errorf("Moved(%q) = %v, want %v", s, got, w)
		}
	}
}
