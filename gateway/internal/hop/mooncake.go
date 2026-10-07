package hop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
)

// DefaultBootstrapPort is vLLM's VLLM_MOONCAKE_BOOTSTRAP_PORT default. Each
// worker running MooncakeConnector serves its engine registry there.
const DefaultBootstrapPort = 8998

// maxReplyBytes caps the source's one-token answer and the registry reply.
const maxReplyBytes = 1 << 20

// Endpoint is how the gateway reaches one worker. BaseURL is its OpenAI
// server and BootstrapURL its Mooncake bootstrap server.
type Endpoint struct {
	BaseURL      string
	BootstrapURL string
}

// BootstrapURL is base's host with the bootstrap port, the address vLLM's
// MooncakeConnector listens on beside the OpenAI server.
func BootstrapURL(base string, port int) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Hostname() == "" {
		return "", fmt.Errorf("worker URL %q has no scheme or host", base)
	}
	return u.Scheme + "://" + u.Hostname() + ":" + strconv.Itoa(port), nil
}

// Params tell the destination where to pull a held KV from.
type Params struct {
	BootstrapURL string
	EngineID     string
	TransferID   string
}

// ErrNotHeld means the source answered but did not end at its length cap.
// vLLM's MooncakeConnector holds the KV only for a request that did, so
// there is nothing to pull.
var ErrNotHeld = errors.New("source did not hold the KV: finish_reason is not length")

// Mooncake speaks vLLM's MooncakeConnector protocol, as vLLM v0.29.0's
// examples/disaggregated/mooncake_connector proxy does. The source gets the
// request with do_remote_decode and a one-token cap, prefills it (mostly from
// its prefix cache) and holds the blocks. The destination gets the request
// with do_remote_prefill and pulls the blocks it does not already hold. Both
// workers run the connector with kv_role kv_both.
type Mooncake struct {
	client *http.Client

	mu      sync.Mutex
	engines map[string]string // bootstrap URL → engine id of data-parallel rank 0
}

// NewMooncake returns a Mooncake that sends with client.
func NewMooncake(client *http.Client) *Mooncake {
	return &Mooncake{client: client, engines: make(map[string]string)}
}

// Send makes src prefill body and hold its KV under transferID, and returns
// the Params the destination needs. On any error the cached engine id is
// dropped, because the likeliest cause is a restarted worker with a new one.
func (m *Mooncake) Send(ctx context.Context, src Endpoint, body []byte, requestID, transferID string) (Params, error) {
	engine, err := m.engine(ctx, src.BootstrapURL)
	if err != nil {
		return Params{}, err
	}
	if err := m.hold(ctx, src.BaseURL, body, requestID, transferID); err != nil {
		m.forget(src.BootstrapURL)
		return Params{}, err
	}
	return Params{BootstrapURL: src.BootstrapURL, EngineID: engine, TransferID: transferID}, nil
}

// hold posts the source request and checks that the source kept the blocks.
func (m *Mooncake) hold(ctx context.Context, base string, body []byte, requestID, transferID string) error {
	out, err := sourceBody(body, transferID)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(out))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", requestID)
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("source answered %d", resp.StatusCode)
	}
	var wire struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(reply, &wire); err != nil {
		return fmt.Errorf("source reply: %w", err)
	}
	if len(wire.Choices) == 0 || wire.Choices[0].FinishReason != "length" {
		return ErrNotHeld
	}
	return nil
}

// engine returns the engine id behind bootstrap, asking its registry once
// and caching the answer.
func (m *Mooncake) engine(ctx context.Context, bootstrap string) (string, error) {
	m.mu.Lock()
	id, ok := m.engines[bootstrap]
	m.mu.Unlock()
	if ok {
		return id, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bootstrap+"/query", nil)
	if err != nil {
		return "", err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bootstrap registry answered %d", resp.StatusCode)
	}
	var ranks map[string]struct {
		EngineID string `json:"engine_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReplyBytes)).Decode(&ranks); err != nil {
		return "", fmt.Errorf("bootstrap registry: %w", err)
	}
	id = ranks["0"].EngineID
	if id == "" {
		return "", errors.New("bootstrap registry lists no engine for rank 0")
	}
	m.mu.Lock()
	m.engines[bootstrap] = id
	m.mu.Unlock()
	return id, nil
}

func (m *Mooncake) forget(bootstrap string) {
	m.mu.Lock()
	delete(m.engines, bootstrap)
	m.mu.Unlock()
}

// sourceBody is body capped to one token, not streamed, and marked for the
// source to hold its KV.
func sourceBody(body []byte, transferID string) ([]byte, error) {
	fields, err := object(body)
	if err != nil {
		return nil, err
	}
	fields["kv_transfer_params"] = mustJSON(map[string]any{
		"do_remote_decode":  true,
		"do_remote_prefill": false,
		"transfer_id":       transferID,
	})
	fields["stream"] = json.RawMessage("false")
	fields["max_tokens"] = json.RawMessage("1")
	if _, ok := fields["max_completion_tokens"]; ok {
		fields["max_completion_tokens"] = json.RawMessage("1")
	}
	delete(fields, "stream_options")
	return json.Marshal(fields)
}

// Receive returns body marked for the destination to pull the KV p names.
// Every other field is kept as it was.
func Receive(body []byte, p Params) ([]byte, error) {
	fields, err := object(body)
	if err != nil {
		return nil, err
	}
	fields["kv_transfer_params"] = mustJSON(map[string]any{
		"do_remote_decode":      false,
		"do_remote_prefill":     true,
		"remote_bootstrap_addr": p.BootstrapURL,
		"remote_engine_id":      p.EngineID,
		"transfer_id":           p.TransferID,
	})
	return json.Marshal(fields)
}

// object decodes a request body's top level, leaving every value as raw JSON.
func object(body []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("request body: %w", err)
	}
	if fields == nil {
		return nil, errors.New("request body is not a JSON object")
	}
	return fields, nil
}

// mustJSON marshals a value built from strings and booleans, which cannot fail.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
