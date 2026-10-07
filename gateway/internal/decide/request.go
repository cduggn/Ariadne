// Package decide is the gateway's pure domain core. It has no side effects.
// Callers pass the clock in, and the package imports only a small stdlib set.
// imports_test.go enforces that set, so purity does not depend on review.
package decide

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

// RunID is the agent run a request belongs to. It is the X-Request-Id text
// before the final "-s<n>" suffix. The zero value means the request has no
// run, so it gets no stickiness.
type RunID string

// Priority is X-Priority. Any value other than "interactive" is Batch, so a
// missing or garbled header can never jump the queue.
type Priority uint8

const (
	Batch Priority = iota // zero value: the conservative lane
	Interactive
)

// String returns the metric label for p: "batch" or "interactive".
func (p Priority) String() string {
	if p == Interactive {
		return "interactive"
	}
	return "batch"
}

// DataClass is X-Data-Class. The zero value is Restricted, and parsing maps
// every value other than "internal" or "public" to Restricted. A missing
// header, a typo or a new class name therefore keeps the request on the box.
type DataClass uint8

const (
	Restricted DataClass = iota // zero value, on purpose
	Internal
	Public
)

// String returns the metric label for c: "restricted", "internal" or
// "public".
func (c DataClass) String() string {
	switch c {
	case Internal:
		return "internal"
	case Public:
		return "public"
	default:
		return "restricted"
	}
}

// ParsePriority maps every input to a value, and an unknown input to Batch,
// the safe lane. It trims spaces and ignores case, so "Interactive",
// " interactive " and "interactive" all count.
func ParsePriority(s string) Priority {
	if strings.EqualFold(strings.TrimSpace(s), "interactive") {
		return Interactive
	}
	return Batch
}

// ParseDataClass maps every input to a value, and an unknown or empty input
// to Restricted, the safe class.
func ParseDataClass(s string) DataClass {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "internal":
		return Internal
	case "public":
		return Public
	default:
		return Restricted
	}
}

// ParseRequestID splits "<run>-s<n>" into (RunID, n) at the final
// "-s<digits>" suffix. It returns ("", 0) when the id has no such suffix, an
// empty run part, or a non-digit tail. The doctor client builds ids the same
// way, so the run id is the text before the final -s<n>.
func ParseRequestID(id string) (RunID, int) {
	idx := strings.LastIndex(id, "-s")
	if idx <= 0 {
		return "", 0
	}
	tail := id[idx+2:]
	if tail == "" {
		return "", 0
	}
	for _, r := range tail {
		if r < '0' || r > '9' {
			return "", 0
		}
	}
	n, err := strconv.Atoi(tail)
	if err != nil {
		return "", 0
	}
	return RunID(id[:idx]), n
}

// Headers is the handful of header values Inspect reads. The shell copies
// them out of http.Header so this package never sees net/http.
type Headers struct {
	RequestID, Tenant, App, Priority, DataClass string
}

// DefaultMaxOut is the maximum output token count used when a request
// specifies neither max_completion_tokens nor max_tokens.
const DefaultMaxOut = 768

// DefaultMaxBody is the default request size ceiling. The largest doctor
// context is roughly 100 KiB, so 1 MiB leaves ample headroom.
const DefaultMaxBody = 1 << 20

// Request is the validated, typed view of one inbound call. Only Inspect
// builds it, and later steps trust every field. It holds the body's length,
// not its bytes, because the proxy forwards the original bytes untouched.
type Request struct {
	ID        string    // X-Request-Id verbatim, forwarded unchanged
	Run       RunID     // "" when the id has no -s<n> suffix
	Step      int       // n from -s<n>; 0 when unknown
	Tenant    string    // sanitised X-Tenant, default "platform"
	App       string    // X-App, for labels only
	Priority  Priority  // X-Priority
	Class     DataClass // X-Data-Class
	BodyBytes int       // len(body)
	MaxOut    int       // max_completion_tokens, else max_tokens, else DefaultMaxOut
	Arrived   time.Time // gateway receive time; deadlines derive from it
	Deadline  time.Time // Arrived + per-priority queue budget; never zero after Inspect
}

// Budgets holds the per-priority queue budgets used to stamp Deadline.
type Budgets struct {
	Interactive, Batch time.Duration
}

// DefaultBudgets give interactive requests 10s and batch requests 30s, both
// well under the client's 120s timeout.
var DefaultBudgets = Budgets{Interactive: 10 * time.Second, Batch: 30 * time.Second}

// Reject is Inspect's verdict for a malformed request. Code is always 400,
// because the guard rejects bad input and never judges capacity.
type Reject struct {
	Code   int
	Reason string
	Detail string
}

// Guard is Inspect's verdict. Either Req or Reject is meaningful, and OK
// reports which.
type Guard struct {
	Req    Request
	Reject *Reject
}

// OK reports whether the request passed the guard.
func (g Guard) OK() bool { return g.Reject == nil }

// Inspect validates a raw request body and headers, and returns either a
// typed Request or a 400 Reject. It checks, in order, the body size, that the
// body is a JSON object, a non-empty messages array, a non-empty model, that
// stream is not set, and the max output tokens. Headers never cause a 400. A
// missing X-Request-Id gives Run == "" and no stickiness.
func Inspect(body []byte, h Headers, now time.Time, b Budgets, maxBody int) Guard {
	if len(body) > maxBody {
		return reject("body_too_large", "body exceeds "+strconv.Itoa(maxBody)+" bytes")
	}

	var top any
	if err := json.Unmarshal(body, &top); err != nil {
		return reject("bad_json", "body is not valid JSON")
	}
	obj, ok := top.(map[string]any)
	if !ok {
		return reject("bad_json", "body must be a JSON object")
	}

	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return reject("no_messages", "messages must be a non-empty array")
	}

	model, ok := obj["model"].(string)
	if !ok || model == "" {
		return reject("no_model", "model must be a non-empty string")
	}

	if stream, ok := obj["stream"].(bool); ok && stream {
		return reject("stream_unsupported", "stream is not supported; the gateway serves non-streaming only")
	}

	// kv_transfer_params is the gateway's own instruction to a worker running
	// vLLM's MooncakeConnector (internal/hop). From a client it could make a
	// worker connect to any address, or pull another request's KV under its
	// transfer id, so the guard refuses it whether or not the hop is on.
	if _, present := obj["kv_transfer_params"]; present {
		return reject("kv_transfer_params", "kv_transfer_params is set by the gateway, never by a client")
	}

	maxOut := DefaultMaxOut
	if v, present := obj["max_completion_tokens"]; present {
		n, ok := positiveInt(v)
		if !ok {
			return reject("bad_max_tokens", "max_completion_tokens must be a positive integer")
		}
		maxOut = n
	} else if v, present := obj["max_tokens"]; present {
		n, ok := positiveInt(v)
		if !ok {
			return reject("bad_max_tokens", "max_tokens must be a positive integer")
		}
		maxOut = n
	}

	run, step := ParseRequestID(h.RequestID)
	priority := ParsePriority(h.Priority)
	budget := b.Batch
	if priority == Interactive {
		budget = b.Interactive
	}

	return Guard{Req: Request{
		ID:        h.RequestID,
		Run:       run,
		Step:      step,
		Tenant:    sanitizeTenant(h.Tenant),
		App:       h.App,
		Priority:  priority,
		Class:     ParseDataClass(h.DataClass),
		BodyBytes: len(body),
		MaxOut:    maxOut,
		Arrived:   now,
		Deadline:  now.Add(budget),
	}}
}

func reject(reason, detail string) Guard {
	return Guard{Reject: &Reject{Code: 400, Reason: reason, Detail: detail}}
}

// positiveInt reports whether v, a decoded JSON value, is a positive whole
// number, and returns it as an int.
func positiveInt(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f <= 0 || f != math.Trunc(f) {
		return 0, false
	}
	return int(f), true
}

// sanitizeTenant lowercases raw, keeps only [a-z0-9._-], and caps the result
// at 64 characters. Empty input, or input with nothing left after
// filtering, becomes "platform".
func sanitizeTenant(raw string) string {
	lowered := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range lowered {
		if b.Len() >= 64 {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "platform"
	}
	return b.String()
}
