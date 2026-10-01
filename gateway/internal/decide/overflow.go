package decide

// Overflow results, the label values of orch_overflow_total.
const (
	// OverflowBlocked means a 503 for a restricted request reached the
	// overflow decision and was kept on the box.
	OverflowBlocked = "blocked_invariant"
	// OverflowNoBackend means a 503 for a non-restricted request may leave
	// the box, but no overflow backend is configured.
	OverflowNoBackend = "no_backend"
)

// Offboxable is permission for one request to leave the box. Only MayLeave
// builds a valid one. Its fields are unexported, so no other package can
// forge one, and the zero value is invalid.
type Offboxable struct {
	ok    bool
	class DataClass
}

// Valid reports whether o came from MayLeave. An overflow backend checks it
// before sending anything, so a zero Offboxable refuses too.
func (o Offboxable) Valid() bool {
	return o.ok && o.class != Restricted
}

// MayLeave is the stay-or-leave decision for a refused request. Only a 503
// may leave, and never for a Restricted request. ok is false whenever the
// request stays.
func MayLeave(r Request, v Verdict) (o Offboxable, ok bool) {
	if !v.Shed || v.Stays() || r.Class == Restricted {
		return Offboxable{}, false
	}
	return Offboxable{ok: true, class: r.Class}, true
}

// OverflowResult names what the overflow decision did with a 503. The
// shipped gateway has no overflow backend, so a request that may leave
// still gets the 503.
func OverflowResult(r Request, v Verdict) string {
	if _, ok := MayLeave(r, v); ok {
		return OverflowNoBackend
	}
	return OverflowBlocked
}
