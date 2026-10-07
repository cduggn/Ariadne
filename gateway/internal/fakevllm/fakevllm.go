// Package fakevllm is a fake vLLM worker for tests and the laptop demo. It
// serves the endpoints the gateway touches, /metrics and
// /v1/chat/completions, and records every chat request it receives. It also
// plays vLLM's MooncakeConnector: GET /query is its bootstrap registry, a
// request marked do_remote_decode stops at its one-token cap as a held
// prefill does, and a request marked do_remote_prefill reports its whole
// prompt as cached, as a pulled KV would make it.
package fakevllm

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/cduggn/cluster-doctor/gateway/internal/decide"
)

// Settings are the knobs a test or a demo turns. KVUsage, PoolBlocks and
// Waiting shape /metrics. Latency delays every chat answer. A Status other
// than 200 makes chat answer that status with an OpenAI error body, and
// MetricsDown makes /metrics answer 503. Steps is how many read-tool calls
// a run makes before it submits, so a demo run has the doctor's chained
// shape. At 0 every answer submits. EngineID is the engine the bootstrap
// registry reports.
type Settings struct {
	KVUsage     float64
	PoolBlocks  int
	Waiting     float64
	Latency     time.Duration
	Status      int
	MetricsDown bool
	Steps       int
	EngineID    string
}

// Recorded is one chat request as the worker saw it, and the bytes it
// answered with. Response is nil while the request is still being answered.
type Recorded struct {
	Header   http.Header
	Body     []byte
	Response []byte
}

// Worker is one fake vLLM. Every field is guarded by mu. lastBody keeps the
// previous chat body per run, so a later step of the same run reports the
// shared prefix as cached tokens, as vLLM's prefix cache would.
type Worker struct {
	mu       sync.Mutex
	settings Settings
	requests []Recorded
	lastBody map[decide.RunID][]byte
}

// New returns a Worker with an empty KV cache, the 4941-block pool of the
// reference deployment, no latency and a 200 status.
func New() *Worker {
	return &Worker{
		settings: Settings{PoolBlocks: 4941, Status: http.StatusOK, EngineID: "fake-engine"},
		lastBody: make(map[decide.RunID][]byte),
	}
}

// Set edits the settings under the lock.
func (w *Worker) Set(fn func(*Settings)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fn(&w.settings)
}

// Requests returns a copy of every chat request so far, oldest first.
func (w *Worker) Requests() []Recorded {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Recorded(nil), w.requests...)
}

// Handler serves GET /metrics, GET /query and POST /v1/chat/completions.
func (w *Worker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", w.metrics)
	mux.HandleFunc("GET /query", w.query)
	mux.HandleFunc("POST /v1/chat/completions", w.chat)
	return mux
}

// query is the Mooncake bootstrap registry: one engine at data-parallel rank 0.
func (w *Worker) query(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	engine := w.settings.EngineID
	w.mu.Unlock()
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(map[string]any{"0": map[string]any{"engine_id": engine}}) // the client is gone if this fails
}

// metricsText is the subset of vLLM's exposition the fleet reads, with the
// pool size, the waiting count and the KV usage filled in.
const metricsText = `# HELP vllm:cache_config_info Information of the LLMEngine CacheConfig
# TYPE vllm:cache_config_info gauge
vllm:cache_config_info{block_size="16",num_gpu_blocks="%d"} 1
# HELP vllm:num_requests_waiting Number of requests waiting to be processed.
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{model_name="fake"} %g
# HELP vllm:kv_cache_usage_perc KV-cache usage. 1 means 100 percent usage.
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{model_name="fake"} %g
# HELP vllm:time_to_first_token_seconds Histogram of time to first token in seconds.
# TYPE vllm:time_to_first_token_seconds histogram
vllm:time_to_first_token_seconds_bucket{model_name="fake",le="0.1"} 40
vllm:time_to_first_token_seconds_bucket{model_name="fake",le="0.5"} 90
vllm:time_to_first_token_seconds_bucket{model_name="fake",le="1.0"} 99
vllm:time_to_first_token_seconds_bucket{model_name="fake",le="+Inf"} 100
vllm:time_to_first_token_seconds_count{model_name="fake"} 100
vllm:time_to_first_token_seconds_sum{model_name="fake"} 21.5
`

func (w *Worker) metrics(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	s := w.settings
	w.mu.Unlock()
	if s.MetricsDown {
		http.Error(rw, "metrics unavailable", http.StatusServiceUnavailable)
		return
	}
	rw.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(rw, metricsText, s.PoolBlocks, s.Waiting, s.KVUsage)
}

// chat records the request, waits Latency, then answers. A request whose
// client goes away during the wait gets no answer, as a cancelled upstream
// call would.
func (w *Worker) chat(rw http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(rw, "read body", http.StatusBadRequest)
		return
	}

	w.mu.Lock()
	s := w.settings
	cached := 0
	if run, _ := decide.ParseRequestID(r.Header.Get("X-Request-Id")); run != "" {
		cached = tokens(commonPrefix(w.lastBody[run], body))
		w.lastBody[run] = body
	}
	idx := len(w.requests)
	w.requests = append(w.requests, Recorded{Header: r.Header.Clone(), Body: body})
	w.mu.Unlock()

	select {
	case <-time.After(s.Latency):
	case <-r.Context().Done():
		return
	}

	resp := completion(body, cached, s.Steps)
	switch hop := kvTransfer(body); {
	case hop.DoRemoteDecode:
		resp = held(body)
	case hop.DoRemotePrefill:
		resp = completion(body, tokens(len(body)), s.Steps)
	}
	if s.Status != http.StatusOK {
		resp = errorBody(s.Status)
	}
	w.mu.Lock()
	w.requests[idx].Response = resp
	w.mu.Unlock()

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(s.Status)
	_, _ = rw.Write(resp) // the client is gone if this fails
}

type chatCompletion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   usage    `json:"usage"`
}

type choice struct {
	Index        int     `json:"index"`
	Message      message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type message struct {
	Role      string     `json:"role"`
	Content   *string    `json:"content"`
	ToolCalls []toolCall `json:"tool_calls"`
}

type toolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function function `json:"function"`
}

type function struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type usage struct {
	PromptTokens     int          `json:"prompt_tokens"`
	CompletionTokens int          `json:"completion_tokens"`
	TotalTokens      int          `json:"total_tokens"`
	Details          usageDetails `json:"prompt_tokens_details"`
}

type usageDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// script is the read-tool calls a run makes, in order, before it submits.
var script = []function{
	{Name: "list_problem_pods", Arguments: `{"namespace":"default"}`},
	{Name: "list_resources", Arguments: `{"kind":"pod","namespace":"default"}`},
	{Name: "get_events", Arguments: `{"namespace":"default","object_name":"","limit":20}`},
}

// submit ends a run as inconclusive, the one answer that cites nothing.
var submit = function{
	Name:      "submit_diagnosis",
	Arguments: `{"status":"inconclusive","findings":[],"summary":"fake worker: no model behind this answer"}`,
}

// completion builds the answer every successful chat gets. The one
// assistant message calls the next tool in script until the request
// carries steps tool results, then submit_diagnosis. The prompt count is
// the body's bytes divided by 3.5, the gateway's own estimate, so the two
// agree.
func completion(body []byte, cached, steps int) []byte {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req) // a malformed body reads as the zero value
	done := 0
	for _, m := range req.Messages {
		if m.Role == "tool" {
			done++
		}
	}
	call := submit
	if done < min(steps, len(script)) {
		call = script[done]
	}
	prompt := tokens(len(body))
	out, _ := json.Marshal(chatCompletion{
		ID:      "chatcmpl-fake",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []choice{{
			Message: message{
				Role: "assistant",
				ToolCalls: []toolCall{{
					ID:       fmt.Sprintf("call_fake_%d", done+1),
					Type:     "function",
					Function: call,
				}},
			},
			FinishReason: "tool_calls",
		}},
		Usage: usage{
			PromptTokens:     prompt,
			CompletionTokens: 35,
			TotalTokens:      prompt + 35,
			Details:          usageDetails{CachedTokens: cached},
		},
	})
	return out
}

// transferParams is the part of kv_transfer_params the fake acts on.
type transferParams struct {
	DoRemoteDecode  bool `json:"do_remote_decode"`
	DoRemotePrefill bool `json:"do_remote_prefill"`
}

// kvTransfer reads a request's kv_transfer_params, zero when it has none.
func kvTransfer(body []byte) transferParams {
	var req struct {
		Params transferParams `json:"kv_transfer_params"`
	}
	_ = json.Unmarshal(body, &req) // a malformed body reads as the zero value
	return req.Params
}

// held is the answer to a do_remote_decode request: one token and a length
// stop, the only finish for which vLLM's MooncakeConnector holds the blocks.
func held(body []byte) []byte {
	prompt := tokens(len(body))
	empty := ""
	out, _ := json.Marshal(chatCompletion{
		ID:      "chatcmpl-fake-hold",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Choices: []choice{{Message: message{Role: "assistant", Content: &empty}, FinishReason: "length"}},
		Usage:   usage{PromptTokens: prompt, CompletionTokens: 1, TotalTokens: prompt + 1},
	})
	return out
}

// errorBody is the OpenAI error shape vLLM uses for a failed request.
func errorBody(status int) []byte {
	out, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": fmt.Sprintf("fake worker answered %d", status),
		"type":    "server_error",
		"code":    status,
	}})
	return out
}

// tokens returns ceil(n / 3.5), the byte-to-token rule the gateway uses.
func tokens(n int) int {
	return int(math.Ceil(float64(n) / 3.5))
}

// commonPrefix returns the length of the longest byte prefix a and b share.
func commonPrefix(a, b []byte) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
