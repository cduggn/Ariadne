package hop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/cduggn/cluster-doctor/gateway/internal/decide"
)

// fastRule hops any history over 10 tokens beyond a 0-token shared prefix.
var fastRule = Config{
	MinTokens:         10,
	KVBytesPerToken:   1,
	TransferBytesPerS: 1e12,
	PrefillTokensPerS: 1,
	Timeout:           time.Second,
	MaxInflight:       4,
}

func newHopper(t *testing.T, cfg Config, src *source) *Hopper {
	t.Helper()
	h, err := New(cfg, map[string]Endpoint{
		"vllm-0": src.endpoint(),
		"vllm-1": {BaseURL: "http://unused", BootstrapURL: "http://unused"},
	}, http.DefaultClient, time.Now)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return h
}

func move(sticky decide.Sticky, history int) Move {
	return Move{Sticky: sticky, From: "vllm-0", To: "vllm-1", History: history}
}

func TestMoveHopsALongHistoryOffTheOldWorker(t *testing.T) {
	src := newSource(t)
	out, res := newHopper(t, fastRule, src).Move(context.Background(), move(decide.StickyBrokenLoad, 5000), []byte(doctorStep))
	if res.Outcome != Hopped || res.Err != nil {
		t.Fatalf("Move() = %+v, want hopped", res)
	}
	var got struct {
		Params map[string]any `json:"kv_transfer_params"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	xfer, _ := got.Params["transfer_id"].(string)
	if got.Params["do_remote_prefill"] != true || got.Params["remote_engine_id"] != "engine-vllm-0" || !validTransferID(xfer) {
		t.Errorf("destination kv_transfer_params = %v", got.Params)
	}
	if _, chats, ids := src.seen(); chats != 1 || ids[0] != xfer {
		t.Errorf("source saw %d requests %v, want one under the transfer id %s", chats, ids, xfer)
	}
}

func TestMoveLeavesTheBodyAloneWhenNoHopApplies(t *testing.T) {
	tests := []struct {
		name string
		m    Move
		want Outcome
	}{
		{"a kept binding", move(decide.StickyHit, 5000), ""},
		{"a first step", move(decide.StickyNew, 5000), ""},
		{"a restarted worker", move(decide.StickyBrokenGone, 5000), ""},
		{"an unknown source", Move{Sticky: decide.StickyBrokenLoad, From: "vllm-9", To: "vllm-1", History: 5000}, ""},
		{"a short history", move(decide.StickyBrokenShed, 5), BelowThreshold},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newSource(t)
			out, res := newHopper(t, fastRule, src).Move(context.Background(), tt.m, []byte(doctorStep))
			if res.Outcome != tt.want || string(out) != doctorStep {
				t.Errorf("Move() = %+v with body changed=%v, want outcome %q and the body unchanged", res, string(out) != doctorStep, tt.want)
			}
			if _, chats, _ := src.seen(); chats != 0 {
				t.Errorf("source got %d requests, want none", chats)
			}
		})
	}
}

func TestMoveFallsBackToRecomputeWhenTheSourceFails(t *testing.T) {
	src := newSource(t)
	src.set(func(s *source) { s.status = http.StatusServiceUnavailable })
	out, res := newHopper(t, fastRule, src).Move(context.Background(), move(decide.StickyBrokenLoad, 5000), []byte(doctorStep))
	if res.Outcome != Failed || res.Err == nil || string(out) != doctorStep {
		t.Fatalf("Move() = %+v, want failed with an error and the body unchanged", res)
	}
}

func TestMoveGivesUpOnASourceSlowerThanTheTimeout(t *testing.T) {
	stall := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /query", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"0":{"engine_id":"e0"}}`))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	})
	slow := httptest.NewServer(mux)
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(stall) })

	cfg := fastRule
	cfg.Timeout = 20 * time.Millisecond
	h, err := New(cfg, map[string]Endpoint{"vllm-0": {BaseURL: slow.URL, BootstrapURL: slow.URL}}, http.DefaultClient, time.Now)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	out, res := h.Move(context.Background(), move(decide.StickyBrokenLoad, 5000), []byte(doctorStep))
	if res.Outcome != Failed || res.Took > time.Second || string(out) != doctorStep {
		t.Fatalf("Move() = %+v, want failed within the timeout and the body unchanged", res)
	}
}

func TestNewRejectsABadConfigAndNoEndpoints(t *testing.T) {
	if _, err := New(Config{}, map[string]Endpoint{"vllm-0": {}}, http.DefaultClient, time.Now); err == nil {
		t.Error("New() with a zero config = nil error")
	}
	if _, err := New(fastRule, nil, http.DefaultClient, time.Now); err == nil {
		t.Error("New() with no endpoints = nil error")
	}
}

var transferIDPattern = regexp.MustCompile(`^xfer-[0-9a-f]{32}$`)

func validTransferID(s string) bool { return transferIDPattern.MatchString(s) }

func TestEveryHopGetsItsOwnUnguessableTransferID(t *testing.T) {
	src := newSource(t)
	h := newHopper(t, fastRule, src)
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		out, res := h.Move(context.Background(), move(decide.StickyBrokenLoad, 5000), []byte(doctorStep))
		if res.Outcome != Hopped {
			t.Fatalf("Move() #%d = %+v, want hopped", i+1, res)
		}
		var got struct {
			Params struct {
				TransferID string `json:"transfer_id"`
			} `json:"kv_transfer_params"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		id := got.Params.TransferID
		if !validTransferID(id) || seen[id] {
			t.Fatalf("transfer id %q is malformed or repeated across hops %v", id, seen)
		}
		seen[id] = true
	}
}

func TestMoveIsBusyPastMaxInflightAndFreesItsSlot(t *testing.T) {
	stall, entered := make(chan struct{}), make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /query", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"0":{"engine_id":"e0"}}`))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-stall:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"length"}]}`))
	})
	slow := httptest.NewServer(mux)
	t.Cleanup(slow.Close)

	cfg := fastRule
	cfg.MaxInflight = 1
	h, err := New(cfg, map[string]Endpoint{"vllm-0": {BaseURL: slow.URL, BootstrapURL: slow.URL}}, http.DefaultClient, time.Now)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	first := make(chan Result, 1)
	go func() {
		_, res := h.Move(context.Background(), move(decide.StickyBrokenLoad, 5000), []byte(doctorStep))
		first <- res
	}()
	<-entered

	if out, res := h.Move(context.Background(), move(decide.StickyBrokenLoad, 5000), []byte(doctorStep)); res.Outcome != Busy || string(out) != doctorStep {
		t.Fatalf("second Move() = %+v, want busy and the body unchanged", res)
	}
	close(stall)
	if res := <-first; res.Outcome != Hopped {
		t.Fatalf("first Move() = %+v, want hopped", res)
	}
	go func() { <-entered }()
	if _, res := h.Move(context.Background(), move(decide.StickyBrokenLoad, 5000), []byte(doctorStep)); res.Outcome != Hopped {
		t.Fatalf("Move() after the slot freed = %+v, want hopped", res)
	}
}
