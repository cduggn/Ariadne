package fakevllm

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cduggn/cluster-doctor/gateway/internal/fleet"
)

// bodyOfLength builds a chat body of exactly n bytes by padding the user
// message.
func bodyOfLength(t *testing.T, n int) []byte {
	t.Helper()
	head := `{"model":"m","messages":[{"role":"user","content":"`
	tail := `"}]}`
	body := head + strings.Repeat("a", n-len(head)-len(tail)) + tail
	if len(body) != n {
		t.Fatalf("body is %d bytes, want %d", len(body), n)
	}
	return []byte(body)
}

type chatResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		Details          struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func postChat(t *testing.T, url, requestID string, body []byte) (int, chatResponse) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("X-Request-Id", requestID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out chatResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("response is not JSON: %v\n%s", err, raw)
		}
	}
	return resp.StatusCode, out
}

func TestChatUsageCountsBodyBytes(t *testing.T) {
	w := New()
	srv := httptest.NewServer(w.Handler())
	defer srv.Close()

	status, out := postChat(t, srv.URL, "run-a-s1", bodyOfLength(t, 70))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	u := out.Usage
	if u.PromptTokens != 20 || u.CompletionTokens != 35 || u.TotalTokens != 55 || u.Details.CachedTokens != 0 {
		t.Fatalf("usage = %+v, want prompt 20, completion 35, total 55, cached 0", u)
	}
	c := out.Choices[0]
	if c.FinishReason != "tool_calls" || c.Message.ToolCalls[0].Function.Name != "submit_diagnosis" ||
		!strings.Contains(c.Message.ToolCalls[0].Function.Arguments, `"status":"inconclusive"`) {
		t.Fatalf("choice = %+v, want a submit_diagnosis tool call", c)
	}
	recorded := w.Requests()
	if len(recorded) != 1 || len(recorded[0].Body) != 70 || recorded[0].Header.Get("X-Request-Id") != "run-a-s1" {
		t.Fatalf("recorded = %+v, want one 70-byte request with its id", recorded)
	}
}

func TestStepsCallReadToolsBeforeSubmitting(t *testing.T) {
	w := New()
	w.Set(func(s *Settings) { s.Steps = 2 })
	srv := httptest.NewServer(w.Handler())
	defer srv.Close()

	tool := `{"role":"tool","tool_call_id":"c","content":"x"}`
	var got []string
	for done := 0; done <= 2; done++ {
		msgs := `{"role":"user","content":"go"}` + strings.Repeat(","+tool, done)
		_, out := postChat(t, srv.URL, "run-a-s1", []byte(`{"model":"m","messages":[`+msgs+`]}`))
		got = append(got, out.Choices[0].Message.ToolCalls[0].Function.Name)
	}
	want := []string{"list_problem_pods", "list_resources", "submit_diagnosis"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestChatCachesSharedPrefixWithinRun(t *testing.T) {
	w := New()
	srv := httptest.NewServer(w.Handler())
	defer srv.Close()

	first := bodyOfLength(t, 70)
	second := append(append([]byte(nil), first[:67]...), []byte(strings.Repeat("b", 10)+`"}]}`)...)

	postChat(t, srv.URL, "run-a-s1", first)
	_, same := postChat(t, srv.URL, "run-a-s2", second)
	if same.Usage.Details.CachedTokens != 20 || same.Usage.PromptTokens != 24 {
		t.Fatalf("same run usage = %+v, want cached 20 of prompt 24", same.Usage)
	}
	_, other := postChat(t, srv.URL, "run-b-s1", second)
	if other.Usage.Details.CachedTokens != 0 {
		t.Fatalf("other run cached = %d, want 0", other.Usage.Details.CachedTokens)
	}
}

func TestMetricsAndStatusFollowSettings(t *testing.T) {
	w := New()
	srv := httptest.NewServer(w.Handler())
	defer srv.Close()

	w.Set(func(s *Settings) { s.KVUsage = 0.42; s.Waiting = 3 })
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics error = %v", err)
	}
	s, err := fleet.ParseMetrics(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("ParseMetrics error = %v", err)
	}
	if !s.OK || s.KVUsage != 0.42 || s.Waiting != 3 || s.KVPoolTokens != 79056 {
		t.Fatalf("sample = %+v, want KV 0.42, waiting 3, pool 79056", s)
	}

	w.Set(func(s *Settings) { s.Status = http.StatusInternalServerError; s.MetricsDown = true })
	if status, _ := postChat(t, srv.URL, "run-a-s1", bodyOfLength(t, 70)); status != http.StatusInternalServerError {
		t.Fatalf("chat status = %d, want 500", status)
	}
	resp, _ = http.Get(srv.URL + "/metrics")
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("metrics status = %d, want 503", resp.StatusCode)
	}
}
