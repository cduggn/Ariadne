package decide

import "time"

// PolicyRouter is the policy label of a request the llm-d router placed. The
// gateway never picks with it, so ParsePickPolicy does not accept it.
const PolicyRouter PickPolicy = "router"

// Header names of the llm-d router's request and response contract
// (llm-d-router v1.0.0, pkg/epp/metadata/consts.go and the flow-control docs).
const (
	// RouterFairnessHeader names the flow a request queues under in llm-d's
	// flow control. The gateway sets it to the tenant.
	RouterFairnessHeader = "x-llm-d-inference-fairness-id"
	// RouterObjectiveHeader names the InferenceObjective, which gives the
	// request its priority in llm-d's flow control.
	RouterObjectiveHeader = "x-llm-d-inference-objective"
	// RouterDropHeader is the response header llm-d sets when flow control
	// refused or evicted a request instead of serving it.
	RouterDropHeader = "x-llm-d-request-dropped-reason"
	// RouterServedHeader is the response header naming the endpoint that
	// served the request.
	RouterServedHeader = "x-gateway-destination-endpoint-served"
	// RouterDestinationHeader is the request header the EPP sets to choose
	// an endpoint. A client never sends it through the gateway.
	RouterDestinationHeader = "x-gateway-destination-endpoint"
)

// The InferenceObjective names the gateway sends, one per Priority. The
// router's deployment must define both.
const (
	ObjectiveInteractive = "doctor-interactive"
	ObjectiveBatch       = "doctor-batch"
)

// routerRetryAfter is the retry hint on every router refusal. llm-d does
// not say how long its queue will stay full, so it is the short hint the
// gateway's own queue refusals carry.
const routerRetryAfter = time.Second

// routerDrops maps llm-d's drop reasons to the gateway's refusal reasons.
// Every entry answers 503: llm-d sheds saturation with 429, but the client
// (D-44) and MayLeave read 429 as the tenant's own quota, which stays, and
// only 503 as capacity, which may leave the box. Relaying llm-d's 429 would
// switch overflow off behind the router. llm-d v1.0 defines the two evicted-*
// sub-reasons but sends only the plain "evicted" so far.
var routerDrops = map[string]Reason{
	"rejected-saturated":     ReasonRouterSaturated,
	"rejected-ttl-expired":   ReasonRouterTTLExpired,
	"evicted":                ReasonRouterEvicted,
	"evicted-queue-pressure": ReasonRouterEvicted,
	"evicted-priority":       ReasonRouterEvicted,
	"rejected-no-endpoints":  ReasonRouterNoEndpoints,
	"rejected-shutting-down": ReasonRouterShuttingDown,
}

// RouterDrop reads the router's answer as a capacity refusal. ok is true
// for a 429 or 503 that carries a drop reason. rejected-internal comes with
// a 500, and a 429 or 503 without the header is not flow control's
// refusal, so both report ok false and the caller relays them as they are.
// An unknown reason, including rejected-context-cancelled, still refuses,
// as router_unknown, because llm-d said it did not serve the request.
func RouterDrop(status int, dropReason string) (Verdict, bool) {
	if dropReason == "" || (status != 429 && status != 503) {
		return Verdict{}, false
	}
	reason, known := routerDrops[dropReason]
	if !known {
		reason = ReasonRouterUnknown
	}
	return Verdict{
		Shed:       true,
		Code:       503,
		Reason:     reason,
		RetryAfter: routerRetryAfter,
		Detail:     "llm-d router dropped the request: " + dropReason,
	}, true
}

// RouterHeaders are the headers the gateway sets on a request to the
// router, after the guard has parsed the priority and the tenant has been
// charged. llm-d trusts them as given, so they come from the parsed Request
// and the charge, never from the client. flow is the charged bucket's name
// (fleet.Charge.Flow), so fairness is shared exactly as the quota is.
func RouterHeaders(r Request, flow string) map[string]string {
	objective := ObjectiveBatch
	if r.Priority == Interactive {
		objective = ObjectiveInteractive
	}
	return map[string]string{
		RouterFairnessHeader:  flow,
		RouterObjectiveHeader: objective,
	}
}
