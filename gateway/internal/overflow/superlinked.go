package overflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cduggn/ariadne/gateway/internal/decide"
)

// Config shapes the Superlinked backend. The paths are flags because the
// hosted API's exact routes are unconfirmed (F22 saw only 402s): the
// defaults are the OpenAI-compatible ones SIE's HTTP reference lists, and a
// different hosted layout is a configuration change, not a code change.
type Config struct {
	// BaseURL is the backend's origin, without a path. A trailing "/" or
	// "/v1" is tolerated and dropped, because the course lab's
	// OVERFLOW_BASE_URL carried the "/v1".
	BaseURL string
	// ChatPath is posted a chat completion.
	ChatPath string
	// ProbePath is fetched by Probe with the key and must answer 2xx.
	ProbePath string
	// Model replaces the request's model name when set. Empty forwards the
	// client's model name, which only works if the backend serves it under
	// that name.
	Model string
	// Timeout bounds one chat call. It sits under the client's 120 s.
	Timeout time.Duration
	// ProbeInterval is how often the circuit re-probes.
	ProbeInterval time.Duration
	// MaxInflight caps overflow calls running at once.
	MaxInflight int
}

// DefaultConfig is the hosted Superlinked API as the course lab addressed
// it, on the OpenAI-compatible paths, with a timeout that leaves the client
// an answer under its own.
var DefaultConfig = Config{
	BaseURL:       "https://api.superlinked.com",
	ChatPath:      "/v1/chat/completions",
	ProbePath:     "/v1/models",
	Timeout:       60 * time.Second,
	ProbeInterval: 30 * time.Second,
	MaxInflight:   4,
}

// Validate checks the shape of c. An error here is a startup error.
func (c Config) Validate() error {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("base url %q is not an http(s) origin", c.BaseURL)
	}
	if !strings.HasPrefix(c.ChatPath, "/") || !strings.HasPrefix(c.ProbePath, "/") {
		return errors.New("chat path and probe path must start with /")
	}
	if c.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if c.MaxInflight < 1 {
		return errors.New("max inflight must be at least 1")
	}
	return nil
}

// origin is BaseURL without a trailing "/" or "/v1".
func (c Config) origin() string {
	s := strings.TrimSuffix(c.BaseURL, "/")
	return strings.TrimSuffix(s, "/v1")
}

// Superlinked is an OpenAI-compatible chat-completions client with a bearer
// key. It is named for the backend it was built for, but any server that
// speaks the shape works, the fake one in the tests included. It sends the
// body (with the model rewritten when configured) and nothing else: none of
// the client's headers, so no run, tenant or request id leaves the box.
type Superlinked struct {
	cfg    Config
	key    string
	client *http.Client
}

// NewSuperlinked builds the client. key may be empty, and then the backend
// is expected to refuse the probe, which keeps the circuit open.
func NewSuperlinked(cfg Config, key string, client *http.Client) (*Superlinked, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("overflow config: %w", err)
	}
	if client == nil {
		client = &http.Client{}
	}
	return &Superlinked{cfg: cfg, key: key, client: client}, nil
}

func (s *Superlinked) Name() string { return "superlinked" }

// maxResponseBytes caps one backend response body, as serve does for a
// worker's.
const maxResponseBytes = 32 << 20

// Send posts body to the chat path. The permit is checked again here, so a
// caller that skipped the circuit still cannot send a restricted request.
func (s *Superlinked) Send(ctx context.Context, permit decide.Offboxable, body []byte) (Answer, error) {
	if !permit.Valid() {
		return Answer{}, ErrNoPermit
	}
	out := body
	if s.cfg.Model != "" {
		var err error
		if out, err = rewriteModel(body, s.cfg.Model); err != nil {
			return Answer{}, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.origin()+s.cfg.ChatPath, bytes.NewReader(out))
	if err != nil {
		return Answer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	s.authorize(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return Answer{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Answer{}, err
	}
	return Answer{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: b}, nil
}

// Probe fetches the probe path with the key. Any 2xx passes. A 401, 402 or
// 403 means the key or the account is not usable, which is the case F22
// found, and reads as unavailable like any other failure.
func (s *Superlinked) Probe(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.origin()+s.cfg.ProbePath, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	s.authorize(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // drain so the connection is reused
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("probe %s answered %d", s.cfg.ProbePath, resp.StatusCode)
	}
	return nil
}

func (s *Superlinked) authorize(req *http.Request) {
	if s.key != "" {
		req.Header.Set("Authorization", "Bearer "+s.key)
	}
}

// rewriteModel returns body with its top-level "model" set to model. Keys
// come back sorted, which the backend does not mind; the fields themselves
// are untouched.
func rewriteModel(body []byte, model string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("overflow: body is not a JSON object: %w", err)
	}
	m, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	fields["model"] = m
	return json.Marshal(fields)
}
