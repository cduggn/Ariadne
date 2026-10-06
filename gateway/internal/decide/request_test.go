package decide

import (
	"strings"
	"testing"
	"time"
)

func TestParseRequestID(t *testing.T) {
	cases := []struct {
		name    string
		id      string
		wantRun RunID
		wantN   int
	}{
		{"doctor run", "crashloop-a1b2c3d4-s3", "crashloop-a1b2c3d4", 3},
		{"doctor run two digit step", "dx-oom-12ab34cd-s12", "dx-oom-12ab34cd", 12},
		{"no suffix", "crashloop-a1b2c3d4", "", 0},
		{"empty run part", "-s3", "", 0},
		{"s with no digits", "foo-s", "", 0},
		{"empty string", "", "", 0},
		{"trailing dash s but no digits at all", "foo-s-bar", "", 0},
		{"rightmost -s wins", "a-s1-s2", "a-s1", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotRun, gotN := ParseRequestID(tc.id)
			if gotRun != tc.wantRun || gotN != tc.wantN {
				t.Errorf("ParseRequestID(%q) = (%q, %d), want (%q, %d)", tc.id, gotRun, gotN, tc.wantRun, tc.wantN)
			}
		})
	}
}

func TestParsePriority(t *testing.T) {
	cases := []struct {
		in   string
		want Priority
	}{
		{"interactive", Interactive},
		{"Interactive", Interactive},
		{"INTERACTIVE", Interactive},
		{"  interactive  ", Interactive},
		{"batch", Batch},
		{"", Batch},
		{"garbage", Batch},
		{"interactive ", Interactive},
	}
	for _, tc := range cases {
		if got := ParsePriority(tc.in); got != tc.want {
			t.Errorf("ParsePriority(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseDataClass(t *testing.T) {
	cases := []struct {
		in   string
		want DataClass
	}{
		{"internal", Internal},
		{"Internal", Internal},
		{"INTERNAL", Internal},
		{"public", Public},
		{" Public ", Public},
		{"restricted", Restricted},
		{"", Restricted},
		{"garbage", Restricted},
	}
	for _, tc := range cases {
		if got := ParseDataClass(tc.in); got != tc.want {
			t.Errorf("ParseDataClass(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestPriorityString(t *testing.T) {
	if got := Batch.String(); got != "batch" {
		t.Errorf("Batch.String() = %q, want %q", got, "batch")
	}
	if got := Interactive.String(); got != "interactive" {
		t.Errorf("Interactive.String() = %q, want %q", got, "interactive")
	}
}

func TestDataClassString(t *testing.T) {
	cases := []struct {
		in   DataClass
		want string
	}{
		{Restricted, "restricted"},
		{Internal, "internal"},
		{Public, "public"},
	}
	for _, tc := range cases {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("%v.String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// doctorBody is a realistic doctor step-3 request: two messages, a tools
// array, sampling params the gateway does not care about, and an explicit
// max_completion_tokens.
const doctorBody = `{
  "model": "Qwen/Qwen3-8B-AWQ",
  "messages": [
    {"role": "system", "content": "You are a Kubernetes cluster doctor."},
    {"role": "user", "content": "Diagnose the crashloop on pod api-7d9f4."}
  ],
  "tools": [
    {"type": "function", "function": {"name": "get_pod_logs", "parameters": {"type": "object"}}}
  ],
  "max_completion_tokens": 768,
  "temperature": 0.7,
  "top_k": 20,
  "chat_template_kwargs": {"enable_thinking": false}
}`

func TestInspectHappyPath(t *testing.T) {
	body := []byte(doctorBody)
	headers := Headers{
		RequestID: "dx-oom-12ab34cd-s3",
		Tenant:    "",
		App:       "doctor",
		Priority:  "interactive",
		DataClass: "restricted",
	}
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	guard := Inspect(body, headers, now, DefaultBudgets, DefaultMaxBody)

	if !guard.OK() {
		t.Fatalf("Inspect rejected a valid doctor request: %+v", guard.Reject)
	}

	got := guard.Req
	if got.ID != "dx-oom-12ab34cd-s3" {
		t.Errorf("ID = %q, want %q", got.ID, "dx-oom-12ab34cd-s3")
	}
	if got.Run != "dx-oom-12ab34cd" {
		t.Errorf("Run = %q, want %q", got.Run, "dx-oom-12ab34cd")
	}
	if got.Step != 3 {
		t.Errorf("Step = %d, want %d", got.Step, 3)
	}
	if got.Tenant != "platform" {
		t.Errorf("Tenant = %q, want %q", got.Tenant, "platform")
	}
	if got.App != "doctor" {
		t.Errorf("App = %q, want %q", got.App, "doctor")
	}
	if got.Priority != Interactive {
		t.Errorf("Priority = %v, want %v", got.Priority, Interactive)
	}
	if got.Class != Restricted {
		t.Errorf("Class = %v, want %v", got.Class, Restricted)
	}
	if got.BodyBytes != len(body) {
		t.Errorf("BodyBytes = %d, want %d", got.BodyBytes, len(body))
	}
	if got.MaxOut != 768 {
		t.Errorf("MaxOut = %d, want %d", got.MaxOut, 768)
	}
	if !got.Arrived.Equal(now) {
		t.Errorf("Arrived = %v, want %v", got.Arrived, now)
	}
	wantDeadline := now.Add(10 * time.Second)
	if !got.Deadline.Equal(wantDeadline) {
		t.Errorf("Deadline = %v, want %v", got.Deadline, wantDeadline)
	}
}

func TestInspectBatchDeadline(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	headers := Headers{RequestID: "run-abcd1234-s1"}
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	guard := Inspect(body, headers, now, DefaultBudgets, DefaultMaxBody)

	if !guard.OK() {
		t.Fatalf("Inspect rejected a valid batch request: %+v", guard.Reject)
	}
	if guard.Req.Priority != Batch {
		t.Errorf("Priority = %v, want %v", guard.Req.Priority, Batch)
	}
	wantDeadline := now.Add(30 * time.Second)
	if !guard.Req.Deadline.Equal(wantDeadline) {
		t.Errorf("Deadline = %v, want %v", guard.Req.Deadline, wantDeadline)
	}
}

func TestInspectRejectReasons(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	validMessages := `{"role":"user","content":"hi"}`

	cases := []struct {
		name       string
		body       string
		maxBody    int
		wantReason string
	}{
		{
			name:       "body too large",
			body:       `{"model":"m","messages":[` + validMessages + `]}`,
			maxBody:    10,
			wantReason: "body_too_large",
		},
		{
			name:       "not json",
			body:       `not json at all`,
			maxBody:    DefaultMaxBody,
			wantReason: "bad_json",
		},
		{
			name:       "json but not an object",
			body:       `[1, 2, 3]`,
			maxBody:    DefaultMaxBody,
			wantReason: "bad_json",
		},
		{
			name:       "messages missing",
			body:       `{"model":"m"}`,
			maxBody:    DefaultMaxBody,
			wantReason: "no_messages",
		},
		{
			name:       "messages empty",
			body:       `{"model":"m","messages":[]}`,
			maxBody:    DefaultMaxBody,
			wantReason: "no_messages",
		},
		{
			name:       "model missing",
			body:       `{"messages":[` + validMessages + `]}`,
			maxBody:    DefaultMaxBody,
			wantReason: "no_model",
		},
		{
			name:       "model empty",
			body:       `{"model":"","messages":[` + validMessages + `]}`,
			maxBody:    DefaultMaxBody,
			wantReason: "no_model",
		},
		{
			name:       "stream true",
			body:       `{"model":"m","messages":[` + validMessages + `],"stream":true}`,
			maxBody:    DefaultMaxBody,
			wantReason: "stream_unsupported",
		},
		{
			name:       "client sets kv_transfer_params",
			body:       `{"model":"m","messages":[` + validMessages + `],"kv_transfer_params":{"do_remote_prefill":true,"remote_bootstrap_addr":"http://attacker:8998"}}`,
			maxBody:    DefaultMaxBody,
			wantReason: "kv_transfer_params",
		},
		{
			name:       "max_tokens zero",
			body:       `{"model":"m","messages":[` + validMessages + `],"max_tokens":0}`,
			maxBody:    DefaultMaxBody,
			wantReason: "bad_max_tokens",
		},
		{
			name:       "max_tokens negative",
			body:       `{"model":"m","messages":[` + validMessages + `],"max_tokens":-5}`,
			maxBody:    DefaultMaxBody,
			wantReason: "bad_max_tokens",
		},
		{
			name:       "max_tokens fractional",
			body:       `{"model":"m","messages":[` + validMessages + `],"max_tokens":12.5}`,
			maxBody:    DefaultMaxBody,
			wantReason: "bad_max_tokens",
		},
		{
			name:       "max_completion_tokens not a number",
			body:       `{"model":"m","messages":[` + validMessages + `],"max_completion_tokens":"five"}`,
			maxBody:    DefaultMaxBody,
			wantReason: "bad_max_tokens",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guard := Inspect([]byte(tc.body), Headers{}, now, DefaultBudgets, tc.maxBody)
			if guard.OK() {
				t.Fatalf("Inspect(%q) = OK, want reject %q", tc.body, tc.wantReason)
			}
			if guard.Reject.Code != 400 {
				t.Errorf("Reject.Code = %d, want 400", guard.Reject.Code)
			}
			if guard.Reject.Reason != tc.wantReason {
				t.Errorf("Reject.Reason = %q, want %q", guard.Reject.Reason, tc.wantReason)
			}
			if guard.Reject.Detail == "" {
				t.Errorf("Reject.Detail is empty, want a human-readable message")
			}
		})
	}
}

func TestInspectTenantSanitizing(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	longRaw := strings.Repeat("a", 100)
	wantLong := strings.Repeat("a", 64)

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty defaults to platform", "", "platform"},
		{"whitespace only defaults to platform", "   ", "platform"},
		{"mixed case lowered", "ACME-Corp", "acme-corp"},
		{"allowed punctuation kept", "tenant.42_ok", "tenant.42_ok"},
		{"disallowed chars stripped", "Tenant!!!", "tenant"},
		{"nothing survives defaults to platform", "!!!", "platform"},
		{"truncated to 64 chars", longRaw, wantLong},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guard := Inspect(body, Headers{Tenant: tc.raw}, now, DefaultBudgets, DefaultMaxBody)
			if !guard.OK() {
				t.Fatalf("Inspect rejected a valid request: %+v", guard.Reject)
			}
			if guard.Req.Tenant != tc.want {
				t.Errorf("Tenant = %q, want %q", guard.Req.Tenant, tc.want)
			}
		})
	}
}
