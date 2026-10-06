package hop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// doctorStep is a request shaped like a doctor step that streams nothing and
// sets max_completion_tokens, so the rewrite has both caps to handle.
const doctorStep = `{"model":"Qwen/Qwen3-8B-AWQ","messages":[{"role":"user","content":"why does checkout-api restart?"}],"tools":[{"type":"function","function":{"name":"submit_diagnosis","parameters":{"type":"object"}}}],"max_completion_tokens":768,"stream_options":{"include_usage":true},"temperature":0.7}`

// source is a fake vLLM worker running MooncakeConnector: an OpenAI server
// and a bootstrap registry on one listener. finish is the finish_reason its
// chat answer carries.
type source struct {
	t        *testing.T
	srv      *httptest.Server
	engineID string
	finish   string
	status   int

	mu      sync.Mutex
	queries int
	bodies  [][]byte
	ids     []string
}

func newSource(t *testing.T) *source {
	t.Helper()
	s := &source{t: t, engineID: "engine-vllm-0", finish: "length", status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /query", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.queries++
		engine := s.engineID
		s.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"0": map[string]any{"engine_id": engine, "dp_rank": 0}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.ids = append(s.ids, r.Header.Get("X-Request-Id"))
		status, finish := s.status, s.finish
		s.mu.Unlock()
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"finish_reason": finish}}})
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// set changes the source's behaviour under its lock.
func (s *source) set(fn func(*source)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

// seen returns how many registry queries and chat requests the source got,
// and the request ids of the chat requests.
func (s *source) seen() (queries, chats int, ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queries, len(s.bodies), append([]string(nil), s.ids...)
}

func (s *source) endpoint() Endpoint {
	return Endpoint{BaseURL: s.srv.URL, BootstrapURL: s.srv.URL}
}

func (s *source) lastBody() map[string]any {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		s.t.Fatal("source received no chat request")
	}
	var out map[string]any
	if err := json.Unmarshal(s.bodies[len(s.bodies)-1], &out); err != nil {
		s.t.Fatalf("source body is not JSON: %v", err)
	}
	return out
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	return out
}

func TestSendCapsTheSourceRequestAndMarksItToHold(t *testing.T) {
	src := newSource(t)
	p, err := NewMooncake(http.DefaultClient).Send(context.Background(), src.endpoint(), []byte(doctorStep), "run-s2-hop", "xfer-run-s2")
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if want := (Params{BootstrapURL: src.srv.URL, EngineID: "engine-vllm-0", TransferID: "xfer-run-s2"}); p != want {
		t.Errorf("Send() = %+v, want %+v", p, want)
	}

	got := src.lastBody()
	wantParams := map[string]any{"do_remote_decode": true, "do_remote_prefill": false, "transfer_id": "xfer-run-s2"}
	if b, _ := json.Marshal(got["kv_transfer_params"]); string(b) != string(mustJSON(wantParams)) {
		t.Errorf("kv_transfer_params = %s, want %s", b, mustJSON(wantParams))
	}
	if got["max_tokens"] != 1.0 || got["max_completion_tokens"] != 1.0 || got["stream"] != false {
		t.Errorf("caps = max_tokens %v, max_completion_tokens %v, stream %v; want 1, 1, false",
			got["max_tokens"], got["max_completion_tokens"], got["stream"])
	}
	if _, ok := got["stream_options"]; ok {
		t.Error("stream_options was kept on a non-streamed request")
	}
	original := decode(t, []byte(doctorStep))
	for _, k := range []string{"model", "messages", "tools", "temperature"} {
		if a, b := mustJSON(got[k]), mustJSON(original[k]); string(a) != string(b) {
			t.Errorf("%s changed: %s, want %s", k, a, b)
		}
	}
	if _, _, ids := src.seen(); ids[0] != "run-s2-hop" {
		t.Errorf("X-Request-Id = %q, want run-s2-hop", ids[0])
	}
}

func TestSendAsksTheRegistryOnceAndAgainAfterAFailure(t *testing.T) {
	src := newSource(t)
	m := NewMooncake(http.DefaultClient)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := m.Send(ctx, src.endpoint(), []byte(doctorStep), "id", "x"); err != nil {
			t.Fatalf("Send() #%d error = %v", i+1, err)
		}
	}
	if q, _, _ := src.seen(); q != 1 {
		t.Fatalf("registry queried %d times for two sends, want 1", q)
	}

	src.set(func(s *source) { s.status = http.StatusServiceUnavailable })
	if _, err := m.Send(ctx, src.endpoint(), []byte(doctorStep), "id", "x"); err == nil {
		t.Fatal("Send() to a 503 source = nil error")
	}
	src.set(func(s *source) { s.status, s.engineID = http.StatusOK, "engine-vllm-0-restarted" })
	p, err := m.Send(ctx, src.endpoint(), []byte(doctorStep), "id", "x")
	if err != nil {
		t.Fatalf("Send() after recovery error = %v", err)
	}
	if q, _, _ := src.seen(); p.EngineID != "engine-vllm-0-restarted" || q != 2 {
		t.Errorf("after a failure: engine %q, %d queries; want the new engine and a second query", p.EngineID, q)
	}
}

func TestSendFailsWhenTheSourceDidNotStopAtItsCap(t *testing.T) {
	src := newSource(t)
	src.set(func(s *source) { s.finish = "stop" })
	_, err := NewMooncake(http.DefaultClient).Send(context.Background(), src.endpoint(), []byte(doctorStep), "id", "x")
	if !errors.Is(err, ErrNotHeld) {
		t.Fatalf("Send() error = %v, want ErrNotHeld", err)
	}
}

func TestSendRejectsABodyThatIsNotAnObject(t *testing.T) {
	src := newSource(t)
	if _, err := NewMooncake(http.DefaultClient).Send(context.Background(), src.endpoint(), []byte(`[1,2]`), "id", "x"); err == nil {
		t.Fatal("Send() of a JSON array = nil error")
	}
	if _, chats, _ := src.seen(); chats != 0 {
		t.Error("a body that is not an object still reached the source")
	}
}

func TestReceiveMarksThePullAndKeepsEveryOtherField(t *testing.T) {
	out, err := Receive([]byte(doctorStep), Params{BootstrapURL: "http://vllm-0:8998", EngineID: "e0", TransferID: "xfer-1"})
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}
	got, original := decode(t, out), decode(t, []byte(doctorStep))
	want := map[string]any{
		"do_remote_decode": false, "do_remote_prefill": true,
		"remote_bootstrap_addr": "http://vllm-0:8998", "remote_engine_id": "e0", "transfer_id": "xfer-1",
	}
	if a, b := mustJSON(got["kv_transfer_params"]), mustJSON(want); string(a) != string(b) {
		t.Errorf("kv_transfer_params = %s, want %s", a, b)
	}
	delete(got, "kv_transfer_params")
	if a, b := mustJSON(got), mustJSON(original); string(a) != string(b) {
		t.Errorf("Receive changed other fields:\n got  %s\n want %s", a, b)
	}
}

func TestBootstrapURL(t *testing.T) {
	tests := []struct {
		base, want string
		ok         bool
	}{
		{"http://vllm-0.vllm.default.svc.cluster.local:8000", "http://vllm-0.vllm.default.svc.cluster.local:8998", true},
		{"http://127.0.0.1:18001/", "http://127.0.0.1:8998", true},
		{"vllm-0:8000", "", false},
	}
	for _, tt := range tests {
		got, err := BootstrapURL(tt.base, DefaultBootstrapPort)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("BootstrapURL(%q) = %q, %v; want %q, ok=%v", tt.base, got, err, tt.want, tt.ok)
		}
	}
}
