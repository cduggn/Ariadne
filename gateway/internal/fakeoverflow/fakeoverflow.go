// Package fakeoverflow is a fake overflow backend for tests and the laptop
// demo. It serves the two routes the gateway's Superlinked client touches,
// GET /v1/models as the probe and POST /v1/chat/completions, checks the
// bearer key when one is configured, and records every chat request it
// receives, so a test can prove what left the box and what did not.
package fakeoverflow

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Settings are the knobs a test or a demo turns. Key, when set, is the
// bearer key every request must carry; a request without it gets 401.
// Down makes the probe answer 503, as a backend that is out would. Status
// other than 200 makes chat answer that status with an OpenAI error body,
// which is how a 402 out-of-credit backend looks. Latency delays every chat
// answer.
type Settings struct {
	Key     string
	Down    bool
	Status  int
	Latency time.Duration
}

// Recorded is one chat request as the backend saw it, and the bytes it
// answered with.
type Recorded struct {
	Header   http.Header
	Body     []byte
	Response []byte
}

// Backend is one fake overflow backend. Every field is guarded by mu.
type Backend struct {
	mu       sync.Mutex
	settings Settings
	requests []Recorded
	probes   int
}

// New returns a Backend that is up, needs no key and answers 200.
func New() *Backend {
	return &Backend{settings: Settings{Status: http.StatusOK}}
}

// Set edits the settings under the lock.
func (b *Backend) Set(fn func(*Settings)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fn(&b.settings)
}

// Requests returns a copy of every chat request so far, oldest first.
func (b *Backend) Requests() []Recorded {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Recorded(nil), b.requests...)
}

// Probes is how many probe requests the backend has answered.
func (b *Backend) Probes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.probes
}

// Handler serves GET /v1/models and POST /v1/chat/completions.
func (b *Backend) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", b.models)
	mux.HandleFunc("POST /v1/chat/completions", b.chat)
	return mux
}

// authorized checks the bearer key when one is configured.
func (b *Backend) authorized(rw http.ResponseWriter, r *http.Request, s Settings) bool {
	if s.Key == "" || r.Header.Get("Authorization") == "Bearer "+s.Key {
		return true
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusUnauthorized)
	_, _ = rw.Write(errorBody(http.StatusUnauthorized, "invalid api key"))
	return false
}

func (b *Backend) models(rw http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	s := b.settings
	b.probes++
	b.mu.Unlock()
	if !b.authorized(rw, r, s) {
		return
	}
	if s.Down {
		http.Error(rw, "backend down", http.StatusServiceUnavailable)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(rw, `{"object":"list","data":[{"id":"fake-overflow","object":"model"}]}`)
}

// chat records the request, waits Latency, then answers with one assistant
// message that submits an inconclusive diagnosis, so a doctor run that
// overflowed still ends.
func (b *Backend) chat(rw http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(rw, "read body", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	s := b.settings
	idx := len(b.requests)
	b.requests = append(b.requests, Recorded{Header: r.Header.Clone(), Body: body})
	b.mu.Unlock()
	if !b.authorized(rw, r, s) {
		return
	}

	select {
	case <-time.After(s.Latency):
	case <-r.Context().Done():
		return
	}

	resp := completion(body)
	if s.Status != http.StatusOK {
		resp = errorBody(s.Status, fmt.Sprintf("fake overflow backend answered %d", s.Status))
	}
	b.mu.Lock()
	b.requests[idx].Response = resp
	b.mu.Unlock()

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(s.Status)
	_, _ = rw.Write(resp) // the client is gone if this fails
}

// completion is the answer every successful chat gets. Its id marks it as
// the fake's, and its model echoes the request's, so a test can see the
// gateway's model rewrite.
func completion(body []byte) []byte {
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req) // a malformed body reads as the zero value
	prompt := (len(body) + 3) / 4
	out, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl-fake-overflow",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []map[string]any{{
					"id":   "call_overflow_1",
					"type": "function",
					"function": map[string]any{
						"name":      "submit_diagnosis",
						"arguments": `{"status":"inconclusive","findings":[],"summary":"fake overflow backend: no model behind this answer"}`,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": prompt, "completion_tokens": 30, "total_tokens": prompt + 30},
	})
	return out
}

// errorBody is the OpenAI error shape.
func errorBody(status int, message string) []byte {
	out, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": message,
		"type":    "server_error",
		"code":    status,
	}})
	return out
}
