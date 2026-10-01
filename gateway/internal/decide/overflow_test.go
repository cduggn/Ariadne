package decide

import (
	"strings"
	"testing"
	"time"
)

func shed(code int, reason Reason) Verdict {
	return Verdict{Shed: true, Code: code, Reason: reason}
}

func TestMayLeave(t *testing.T) {
	cases := []struct {
		name  string
		class DataClass
		v     Verdict
		ok    bool
		label string
	}{
		{"restricted 503 stays", Restricted, shed(503, ReasonKVFree), false, OverflowBlocked},
		{"internal 503 may leave", Internal, shed(503, ReasonKVFree), true, OverflowNoBackend},
		{"public 503 may leave", Public, shed(503, ReasonQueueFull), true, OverflowNoBackend},
		{"public 429 stays", Public, shed(429, ReasonTenantTokens), false, OverflowBlocked},
		{"admitted never leaves", Public, Verdict{}, false, OverflowBlocked},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Request{ID: "run-s1", Class: c.class}
			o, ok := MayLeave(r, c.v)
			if ok != c.ok || o.Valid() != c.ok {
				t.Errorf("MayLeave = (valid %v, ok %v), want %v", o.Valid(), ok, c.ok)
			}
			if got := OverflowResult(r, c.v); got != c.label {
				t.Errorf("OverflowResult = %q, want %q", got, c.label)
			}
		})
	}
}

func TestZeroOffboxableIsInvalid(t *testing.T) {
	if (Offboxable{}).Valid() {
		t.Fatal("the zero Offboxable is valid")
	}
}

// FuzzRestrictedNeverLeaves drives any header and body through Inspect and
// every refusal through MayLeave. A request that parsed as Restricted must
// never come out with a valid Offboxable.
func FuzzRestrictedNeverLeaves(f *testing.F) {
	f.Add("restricted", doctorBody, 503)
	f.Add("", doctorBody, 503)
	f.Add("RESTRICTED", doctorBody, 503)
	f.Add(" internal", doctorBody, 503)
	f.Add("public", doctorBody, 503)
	f.Add("internal", `{"model":"m","messages":[]}`, 503)
	f.Add("public", doctorBody, 429)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, class, body string, code int) {
		g := Inspect([]byte(body), Headers{RequestID: "run-s1", DataClass: class}, now, DefaultBudgets, DefaultMaxBody)
		if !g.OK() {
			return
		}
		o, ok := MayLeave(g.Req, Verdict{Shed: true, Code: code, Reason: ReasonKVFree})
		restricted := ParseDataClass(class) == Restricted
		if restricted && (ok || o.Valid()) {
			t.Fatalf("restricted request (X-Data-Class %q) got an overflow permit", class)
		}
		if g.Req.Class != ParseDataClass(class) {
			t.Fatalf("Inspect class %v, ParseDataClass %v", g.Req.Class, ParseDataClass(class))
		}
		if !restricted && code == 503 && !ok {
			t.Fatalf("%s 503 was kept on the box", strings.TrimSpace(class))
		}
	})
}
