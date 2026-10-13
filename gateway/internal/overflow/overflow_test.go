package overflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cduggn/ariadne/gateway/internal/decide"
	"github.com/cduggn/ariadne/gateway/internal/fakeoverflow"
)

const body = `{"model":"Qwen/Qwen3-8B-AWQ","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":8}`

func permit(t *testing.T, class decide.DataClass) decide.Offboxable {
	t.Helper()
	o, _ := decide.MayLeave(decide.Request{ID: "run-s1", Class: class},
		decide.Verdict{Shed: true, Code: 503, Reason: decide.ReasonKVFree})
	return o
}

// fixture is a fake backend behind a Superlinked client and an Overflow
// with no periodic probing, at the given key.
func fixture(t *testing.T, cfg Config, key string) (*Overflow, *fakeoverflow.Backend) {
	t.Helper()
	fake := fakeoverflow.New()
	srv := httptest.NewServer(fake.Handler())
	t.Cleanup(srv.Close)
	cfg.BaseURL = srv.URL
	sl, err := NewSuperlinked(cfg, key, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return New(sl, 0, cfg.MaxInflight, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil))), fake
}

func TestConfigValidate(t *testing.T) {
	if err := DefaultConfig.Validate(); err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	bad := []struct {
		name string
		edit func(*Config)
	}{
		{"no scheme", func(c *Config) { c.BaseURL = "api.superlinked.com" }},
		{"ftp", func(c *Config) { c.BaseURL = "ftp://x" }},
		{"relative chat path", func(c *Config) { c.ChatPath = "v1/chat/completions" }},
		{"zero timeout", func(c *Config) { c.Timeout = 0 }},
		{"no slots", func(c *Config) { c.MaxInflight = 0 }},
	}
	for _, c := range bad {
		cfg := DefaultConfig
		c.edit(&cfg)
		if cfg.Validate() == nil {
			t.Errorf("%s: Validate() = nil, want an error", c.name)
		}
	}
	for _, in := range []string{"https://api.superlinked.com", "https://api.superlinked.com/", "https://api.superlinked.com/v1", "https://api.superlinked.com/v1/"} {
		if got := (Config{BaseURL: in}).origin(); got != "https://api.superlinked.com" {
			t.Errorf("origin(%q) = %q", in, got)
		}
	}
}

func TestBackendStartsUnavailableUntilAProbePasses(t *testing.T) {
	ov, fake := fixture(t, DefaultConfig, "k")
	if ov.Available() {
		t.Fatal("available before any probe")
	}
	res := ov.Send(context.Background(), permit(t, decide.Internal), []byte(body))
	if res.Outcome != Unavailable || res.Body != nil {
		t.Fatalf("Send before a probe = %+v, want unavailable", res)
	}
	if len(fake.Requests()) != 0 {
		t.Fatal("the backend saw a request while unavailable")
	}
	fake.Set(func(s *fakeoverflow.Settings) { s.Key = "k" })
	if !ov.Probe(context.Background()) || !ov.Available() {
		t.Fatal("probe with the right key did not make the backend available")
	}
	if fake.Probes() != 1 || ov.Probes() != 1 {
		t.Errorf("probes = %d/%d, want 1", fake.Probes(), ov.Probes())
	}
}

func TestProbeFailsOnWrongKeyAndOnDownBackend(t *testing.T) {
	ov, fake := fixture(t, DefaultConfig, "wrong")
	fake.Set(func(s *fakeoverflow.Settings) { s.Key = "k" })
	if ov.Probe(context.Background()) {
		t.Fatal("probe passed with the wrong key")
	}
	ov2, fake2 := fixture(t, DefaultConfig, "")
	fake2.Set(func(s *fakeoverflow.Settings) { s.Down = true })
	if ov2.Probe(context.Background()) {
		t.Fatal("probe passed against a down backend")
	}
	unreachable, err := NewSuperlinked(Config{BaseURL: "http://127.0.0.1:1", ChatPath: "/v1/chat/completions",
		ProbePath: "/v1/models", Timeout: time.Second, MaxInflight: 1}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ov3 := New(unreachable, 0, 1, time.Now, nil)
	if ov3.Probe(context.Background()) {
		t.Fatal("probe passed against nothing")
	}
}

func TestSentRelaysTheBackendAnswerWithTheModelRewritten(t *testing.T) {
	cfg := DefaultConfig
	cfg.Model = "Qwen/Qwen3.8-27B-FP8"
	ov, fake := fixture(t, cfg, "k")
	fake.Set(func(s *fakeoverflow.Settings) { s.Key = "k" })
	ov.Probe(context.Background())

	res := ov.Send(context.Background(), permit(t, decide.Public), []byte(body))
	if res.Outcome != Sent || res.Status != 200 || res.Err != nil || res.Took <= 0 {
		t.Fatalf("Send = %+v, want sent 200", res)
	}
	reqs := fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("backend saw %d requests, want 1", len(reqs))
	}
	if string(res.Body) != string(reqs[0].Response) || !strings.HasPrefix(res.ContentType, "application/json") {
		t.Errorf("relayed %q (%s), want the backend's answer", res.Body, res.ContentType)
	}
	var sent struct {
		Model    string          `json:"model"`
		Messages json.RawMessage `json:"messages"`
		MaxOut   int             `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(reqs[0].Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != cfg.Model || string(sent.Messages) != `[{"role":"user","content":"hi"}]` || sent.MaxOut != 8 {
		t.Errorf("backend got %s, want the body with only the model rewritten", reqs[0].Body)
	}
	if got := reqs[0].Header.Get("Authorization"); got != "Bearer k" {
		t.Errorf("Authorization = %q, want the configured key", got)
	}
	for _, k := range []string{"X-Request-Id", "X-Tenant", "X-Data-Class", "X-Priority", "X-App"} {
		if reqs[0].Header.Get(k) != "" {
			t.Errorf("%s left the box", k)
		}
	}
}

func TestWithoutAModelTheClientsModelIsForwardedUnchanged(t *testing.T) {
	ov, fake := fixture(t, DefaultConfig, "")
	ov.Probe(context.Background())
	if res := ov.Send(context.Background(), permit(t, decide.Internal), []byte(body)); res.Outcome != Sent {
		t.Fatalf("Send = %+v", res)
	}
	if got := string(fake.Requests()[0].Body); got != body {
		t.Errorf("backend got %s, want the body unchanged", got)
	}
}

func TestRestrictedPermitNeverReachesTheBackend(t *testing.T) {
	ov, fake := fixture(t, DefaultConfig, "")
	ov.Probe(context.Background())
	for name, p := range map[string]decide.Offboxable{"restricted": permit(t, decide.Restricted), "zero": {}} {
		res := ov.Send(context.Background(), p, []byte(body))
		if res.Outcome != Blocked || !errors.Is(res.Err, ErrNoPermit) {
			t.Errorf("%s permit: Send = %+v, want blocked_invariant", name, res)
		}
		sl := ov.backend.(*Superlinked)
		if _, err := sl.Send(context.Background(), p, []byte(body)); !errors.Is(err, ErrNoPermit) {
			t.Errorf("%s permit: the client itself sent, err %v", name, err)
		}
	}
	if len(fake.Requests()) != 0 {
		t.Fatalf("the backend saw %d requests from restricted or forged permits", len(fake.Requests()))
	}
	if !ov.Available() {
		t.Error("a blocked request opened the circuit")
	}
}

func TestBackendErrorFailsAndOpensTheCircuitUntilTheNextProbe(t *testing.T) {
	ov, fake := fixture(t, DefaultConfig, "")
	ov.Probe(context.Background())
	fake.Set(func(s *fakeoverflow.Settings) { s.Status = http.StatusPaymentRequired })

	res := ov.Send(context.Background(), permit(t, decide.Internal), []byte(body))
	if res.Outcome != Failed || res.Body != nil || res.Err == nil || !strings.Contains(res.Err.Error(), "402") {
		t.Fatalf("Send against a 402 backend = %+v, want failed", res)
	}
	if ov.Available() {
		t.Fatal("a failed send left the circuit closed")
	}
	if res := ov.Send(context.Background(), permit(t, decide.Internal), []byte(body)); res.Outcome != Unavailable {
		t.Fatalf("Send with the circuit open = %+v, want unavailable", res)
	}
	if n := len(fake.Requests()); n != 1 {
		t.Fatalf("backend saw %d requests, want 1: the open circuit must not send", n)
	}
	fake.Set(func(s *fakeoverflow.Settings) { s.Status = http.StatusOK })
	ov.Probe(context.Background())
	if res := ov.Send(context.Background(), permit(t, decide.Internal), []byte(body)); res.Outcome != Sent {
		t.Fatalf("Send after the probe closed the circuit = %+v, want sent", res)
	}
}

func TestTimeoutFailsWithinTheConfiguredBound(t *testing.T) {
	cfg := DefaultConfig
	cfg.Timeout = 50 * time.Millisecond
	ov, fake := fixture(t, cfg, "")
	ov.Probe(context.Background())
	fake.Set(func(s *fakeoverflow.Settings) { s.Latency = time.Second })
	res := ov.Send(context.Background(), permit(t, decide.Internal), []byte(body))
	if res.Outcome != Failed || res.Took < 50*time.Millisecond || res.Took > 500*time.Millisecond {
		t.Fatalf("Send against a slow backend = %+v, want failed after about 50 ms", res)
	}
}

func TestClientGoneDoesNotOpenTheCircuit(t *testing.T) {
	ov, fake := fixture(t, DefaultConfig, "")
	ov.Probe(context.Background())
	fake.Set(func(s *fakeoverflow.Settings) { s.Latency = time.Second })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if res := ov.Send(ctx, permit(t, decide.Internal), []byte(body)); res.Outcome != Failed {
		t.Fatalf("Send = %+v, want failed", res)
	}
	if !ov.Available() {
		t.Error("a client that left opened the circuit")
	}
}

func TestInflightCap(t *testing.T) {
	cfg := DefaultConfig
	cfg.MaxInflight = 1
	ov, fake := fixture(t, cfg, "")
	ov.Probe(context.Background())
	fake.Set(func(s *fakeoverflow.Settings) { s.Latency = 200 * time.Millisecond })
	first := make(chan Result, 1)
	go func() { first <- ov.Send(context.Background(), permit(t, decide.Internal), []byte(body)) }()
	deadline := time.Now().Add(time.Second)
	for len(fake.Requests()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if res := ov.Send(context.Background(), permit(t, decide.Internal), []byte(body)); res.Outcome != Capped {
		t.Fatalf("second Send = %+v, want capped", res)
	}
	if res := <-first; res.Outcome != Sent {
		t.Fatalf("first Send = %+v, want sent", res)
	}
	if ov.Available() != true {
		t.Error("a capped send opened the circuit")
	}
}

func TestRunProbesAtStartAndThenPeriodically(t *testing.T) {
	fake := fakeoverflow.New()
	srv := httptest.NewServer(fake.Handler())
	t.Cleanup(srv.Close)
	cfg := DefaultConfig
	cfg.BaseURL = srv.URL
	sl, err := NewSuperlinked(cfg, "", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	ov := New(sl, 10*time.Millisecond, 1, time.Now, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ov.Run(ctx)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for ov.Probes() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if ov.Probes() < 3 || !ov.Available() {
		t.Fatalf("probes = %d available %v, want at least 3 and available", ov.Probes(), ov.Available())
	}
}

func TestNullBackend(t *testing.T) {
	ov := New(Null{}, 0, 1, nil, nil)
	if ov.Probe(context.Background()) || ov.Available() || ov.Name() != "null" {
		t.Fatal("the null backend became available")
	}
	if res := ov.Send(context.Background(), permit(t, decide.Internal), []byte(body)); res.Outcome != Unavailable {
		t.Fatalf("Send = %+v, want unavailable", res)
	}
	if _, err := (Null{}).Send(context.Background(), permit(t, decide.Internal), []byte(body)); !errors.Is(err, ErrNoBackend) {
		t.Errorf("Null.Send err = %v, want ErrNoBackend", err)
	}
}
