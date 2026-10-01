// Package metrics is the gateway's Prometheus surface. Observe feeds the
// request counters and the latency histogram from each serve.Event, and a
// collector derives every replica gauge from the Gate's view and the
// Fleet's status at scrape time, so a gauge can never drift from the state
// the gateway routes on. Every family lives on a private registry.
package metrics

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/cduggn/cluster-doctor/gateway/internal/decide"
	"github.com/cduggn/cluster-doctor/gateway/internal/fleet"
	"github.com/cduggn/cluster-doctor/gateway/internal/serve"
)

// MetricNames lists every orch_ family the gateway exports. The dashboard
// contract test checks each name a panel reads against this list, so a
// renamed metric fails the build instead of blanking a panel.
var MetricNames = []string{
	"orch_requests_total",
	"orch_shed_total",
	"orch_pick_total",
	"orch_pick_unknown_snapshot_total",
	"orch_sticky_total",
	"orch_completed_total",
	"orch_upstream_errors_total",
	"orch_prompt_tokens_total",
	"orch_completion_tokens_total",
	"orch_overflow_total",
	"orch_restricted_offbox_total",
	"orch_request_duration_seconds",
	"orch_replica_healthy",
	"orch_replica_warm",
	"orch_replica_phase",
	"orch_replica_saturating",
	"orch_replica_kv_free_ratio",
	"orch_replica_tokens_in_flight",
	"orch_replica_active_requests",
	"orch_replica_queue_depth",
	"orch_replica_snapshot_age_seconds",
	"orch_replica_mean_service_seconds",
	"orch_warmup_probe_seconds",
	"orch_replica_restarts_total",
	"orch_kv_free_ratio",
}

// durationBuckets span 5ms to 120s, the client's timeout.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

// preregisteredSheds are the shed label pairs exposed at 0 from startup, so
// a rate() over a reason that has never fired reads 0 instead of no data.
var preregisteredSheds = [][2]string{
	{string(decide.ReasonTenantTokens), "429"},
	{string(decide.ReasonKVFree), "503"},
	{string(decide.ReasonTimeoutQueue), "503"},
	{string(decide.ReasonP99Spread), "503"},
	{string(decide.ReasonNoEligiblePod), "503"},
	{string(decide.ReasonQueueFull), "503"},
}

// Metrics is one gateway's registry and the vectors Observe feeds.
type Metrics struct {
	registry     *prometheus.Registry
	sharedPrefix int

	requests         *prometheus.CounterVec
	shed             *prometheus.CounterVec
	pick             *prometheus.CounterVec
	pickUnknown      *prometheus.CounterVec
	sticky           *prometheus.CounterVec
	completed        *prometheus.CounterVec
	upstreamErrors   *prometheus.CounterVec
	promptTokens     *prometheus.CounterVec
	completionTokens *prometheus.CounterVec
	overflow         *prometheus.CounterVec
	restrictedOffbox prometheus.Counter
	duration         *prometheus.HistogramVec
}

// New builds the registry over g and f. pool is the pool label on every
// replica gauge. sharedPrefix is the token length of the prefix every
// prompt shares, which splits each response's cached tokens into the
// shared prefix and the run's own history.
func New(g *fleet.Gate, f *fleet.Fleet, pool string, sharedPrefix int) *Metrics {
	m := &Metrics{
		registry:     prometheus.NewRegistry(),
		sharedPrefix: sharedPrefix,
		requests: counter("orch_requests_total",
			"Requests received, including those the guard rejected.", "priority", "class"),
		shed: counter("orch_shed_total",
			"Requests refused by the gateway, by reason and status code.", "reason", "code"),
		pick: counter("orch_pick_total",
			"Requests placed on a worker, by pod and pick policy.", "pod", "policy"),
		pickUnknown: counter("orch_pick_unknown_snapshot_total",
			"Placements made on a worker whose telemetry was stale.", "pod"),
		sticky: counter("orch_sticky_total",
			"Stickiness outcomes relative to the run's bound worker.", "outcome"),
		completed: counter("orch_completed_total",
			"Worker answers relayed to the client, by pod, status and finish reason.", "pod", "status", "finish_reason"),
		upstreamErrors: counter("orch_upstream_errors_total",
			"Upstream failures, transport for no answer and status for a non-200 answer.", "pod", "kind"),
		promptTokens: counter("orch_prompt_tokens_total",
			"Prompt tokens by cache outcome: shared_hit, run_hit or miss.", "pod", "kind"),
		completionTokens: counter("orch_completion_tokens_total",
			"Completion tokens generated per pod.", "pod"),
		overflow: counter("orch_overflow_total",
			"Overflow decisions for gateway 503s: blocked_invariant kept a restricted request on the box, no_backend had nowhere to send it.", "result"),
		restrictedOffbox: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "orch_restricted_offbox_total",
			Help: "Restricted requests routed off the box. Must stay 0.",
		}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "orch_request_duration_seconds",
			Help:    "Request time by stage: gateway, queue, local (upstream) and e2e.",
			Buckets: durationBuckets,
		}, []string{"stage"}),
	}
	for _, s := range preregisteredSheds {
		m.shed.WithLabelValues(s[0], s[1])
	}
	m.overflow.WithLabelValues(decide.OverflowBlocked)
	m.overflow.WithLabelValues(decide.OverflowNoBackend)
	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requests, m.shed, m.pick, m.pickUnknown, m.sticky, m.completed, m.upstreamErrors,
		m.promptTokens, m.completionTokens, m.overflow, m.restrictedOffbox, m.duration,
		newFleetCollector(g, f, pool),
	)
	return m
}

func counter(name, help string, labels ...string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// outcome is how a request ended, as the counters see it.
type outcome uint8

const (
	// dropped means the client went away before a status was written.
	dropped outcome = iota
	// refused means the gateway answered the request itself: a 400 from the
	// guard, a 429 or 503 shed, or a 502 when the worker gave no answer.
	refused
	// answered means a worker's answer was relayed, whatever its status.
	answered
)

// outcomeOf classifies ev. A refusal always carries a reason, and a relayed
// worker answer never does.
func outcomeOf(ev serve.Event) outcome {
	switch {
	case ev.Status == 0:
		return dropped
	case ev.Reason != "":
		return refused
	default:
		return answered
	}
}

// Observe folds one finished request into the counters. It is the
// serve.Options.OnRequest hook. Queue and upstream time are observed only
// when the request spent any, so a refusal does not pull their quantiles
// toward 0.
func (m *Metrics) Observe(ev serve.Event) {
	m.requests.WithLabelValues(ev.Priority.String(), ev.Class.String()).Inc()
	m.duration.WithLabelValues("gateway").Observe(ev.Gateway.Seconds())
	m.duration.WithLabelValues("e2e").Observe(ev.Total.Seconds())
	if ev.Queue > 0 {
		m.duration.WithLabelValues("queue").Observe(ev.Queue.Seconds())
	}
	if ev.Upstream > 0 {
		m.duration.WithLabelValues("local").Observe(ev.Upstream.Seconds())
	}
	if ev.Sticky != "" {
		m.sticky.WithLabelValues(string(ev.Sticky)).Inc()
	}
	if ev.Pod != "" {
		m.pick.WithLabelValues(ev.Pod, string(ev.Policy)).Inc()
		if ev.Unknown {
			m.pickUnknown.WithLabelValues(ev.Pod).Inc()
		}
	}

	if ev.Overflow != "" {
		m.overflow.WithLabelValues(ev.Overflow).Inc()
	}

	status := strconv.Itoa(ev.Status)
	switch outcomeOf(ev) {
	case refused:
		m.shed.WithLabelValues(ev.Reason, status).Inc()
		if ev.Reason == "upstream_error" {
			m.upstreamErrors.WithLabelValues(ev.Pod, "transport").Inc()
		}
	case answered:
		m.completed.WithLabelValues(ev.Pod, status, ev.FinishReason).Inc()
		if ev.Status != http.StatusOK {
			m.upstreamErrors.WithLabelValues(ev.Pod, "status").Inc()
			return
		}
		m.observeTokens(ev)
	}
}

// observeTokens splits a 200's cached tokens at the shared prefix. The
// Event carries zero tokens for a 200 without usage, and then nothing is
// added.
func (m *Metrics) observeTokens(ev serve.Event) {
	sharedHit := min(ev.CachedTokens, m.sharedPrefix)
	runHit := max(0, ev.CachedTokens-m.sharedPrefix)
	miss := max(0, ev.PromptTokens-ev.CachedTokens)
	m.promptTokens.WithLabelValues(ev.Pod, "shared_hit").Add(float64(sharedHit))
	m.promptTokens.WithLabelValues(ev.Pod, "run_hit").Add(float64(runHit))
	m.promptTokens.WithLabelValues(ev.Pod, "miss").Add(float64(miss))
	m.completionTokens.WithLabelValues(ev.Pod).Add(float64(ev.CompletionTokens))
}

// replicaGauge is one per-pod gauge and how to read it from the gate's
// view of the worker and the fleet's status for it.
type replicaGauge struct {
	desc *prometheus.Desc
	read func(decide.WorkerState, fleet.WorkerStatus) float64
}

// phases are the label values of orch_replica_phase, every fleet.Phase.
var phases = []fleet.Phase{fleet.Down, fleet.Warming, fleet.Ready}

// fleetCollector derives the replica gauges on every scrape. It stores
// nothing, so a scrape always reports the state Admit would route on.
type fleetCollector struct {
	gate   *fleet.Gate
	fleet  *fleet.Fleet
	pool   string
	gauges []replicaGauge
	phase  *prometheus.Desc
	// restarts is a counter the fleet already keeps, exposed as a const
	// counter.
	restarts *prometheus.Desc
	kvFree   *prometheus.Desc
}

func newFleetCollector(g *fleet.Gate, f *fleet.Fleet, pool string) *fleetCollector {
	gauge := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc(name, help, []string{"pool", "pod"}, nil)
	}
	saturatedBelow := 1 - decide.DefaultPolicy.KVLine
	return &fleetCollector{
		gate:  g,
		fleet: f,
		pool:  pool,
		gauges: []replicaGauge{
			{gauge("orch_replica_healthy", "1 while the gate may place on the worker."),
				func(ws decide.WorkerState, _ fleet.WorkerStatus) float64 { return b2f(ws.Ready) }},
			{gauge("orch_replica_warm", "1 while the fleet's warm-up gate holds the worker Ready."),
				func(_ decide.WorkerState, st fleet.WorkerStatus) float64 { return b2f(st.Phase == fleet.Ready) }},
			{gauge("orch_replica_saturating", "1 while the worker's free KV is below the admission line."),
				func(ws decide.WorkerState, _ fleet.WorkerStatus) float64 {
					return b2f(ws.View.FreeRatio() < saturatedBelow)
				}},
			{gauge("orch_replica_kv_free_ratio", "Free KV as a fraction of the pool, net of the gateway's reservations."),
				func(ws decide.WorkerState, _ fleet.WorkerStatus) float64 { return ws.View.FreeRatio() }},
			{gauge("orch_replica_tokens_in_flight", "Tokens the gateway has reserved on the worker since its last scrape."),
				func(ws decide.WorkerState, _ fleet.WorkerStatus) float64 { return float64(ws.View.ReservedTokens) }},
			{gauge("orch_replica_active_requests", "Requests the gateway holds in flight on the worker."),
				func(ws decide.WorkerState, _ fleet.WorkerStatus) float64 { return float64(ws.View.InFlight) }},
			{gauge("orch_replica_queue_depth", "Requests waiting in the gateway's queue for the worker."),
				func(ws decide.WorkerState, _ fleet.WorkerStatus) float64 { return float64(ws.View.Queued) }},
			{gauge("orch_replica_snapshot_age_seconds", "Seconds since the worker's last successful scrape."),
				func(ws decide.WorkerState, _ fleet.WorkerStatus) float64 { return ws.Age.Seconds() }},
			{gauge("orch_replica_mean_service_seconds", "Recent mean seconds per request on the worker, 0 until the first."),
				func(ws decide.WorkerState, _ fleet.WorkerStatus) float64 { return ws.View.MeanServiceS }},
			{gauge("orch_warmup_probe_seconds", "Latency of the worker's latest warm-up probe, the re-quoted TTFT."),
				func(_ decide.WorkerState, st fleet.WorkerStatus) float64 { return st.LastProbeS }},
		},
		phase: prometheus.NewDesc("orch_replica_phase",
			"1 for the worker's current phase, 0 for the others.", []string{"pool", "pod", "phase"}, nil),
		restarts: prometheus.NewDesc("orch_replica_restarts_total",
			"Down to Warming transitions, including the first at startup.", []string{"pool", "pod"}, nil),
		kvFree: prometheus.NewDesc("orch_kv_free_ratio",
			"Free KV over the Ready workers as a fraction of their pools, 0 when none is Ready.", []string{"pool"}, nil),
	}
}

func (c *fleetCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, g := range c.gauges {
		ch <- g.desc
	}
	ch <- c.phase
	ch <- c.restarts
	ch <- c.kvFree
}

// Collect reads the gate and the fleet once and emits every gauge. The two
// are joined by pod, so a pod the fleet has not reported yet reads as Down.
func (c *fleetCollector) Collect(ch chan<- prometheus.Metric) {
	status := make(map[string]fleet.WorkerStatus)
	for _, st := range c.fleet.Status() {
		status[st.Pod] = st
	}
	var free, pool int
	for _, ws := range c.gate.View() {
		pod := ws.View.Pod
		st := status[pod]
		for _, g := range c.gauges {
			ch <- prometheus.MustNewConstMetric(g.desc, prometheus.GaugeValue, g.read(ws, st), c.pool, pod)
		}
		for _, p := range phases {
			ch <- prometheus.MustNewConstMetric(c.phase, prometheus.GaugeValue, b2f(st.Phase == p), c.pool, pod, p.String())
		}
		ch <- prometheus.MustNewConstMetric(c.restarts, prometheus.CounterValue, float64(st.Restarts), c.pool, pod)
		if ws.Ready {
			free += ws.View.FreeTokens()
			pool += ws.View.KVPoolTokens
		}
	}
	ratio := 0.0
	if pool > 0 {
		ratio = float64(free) / float64(pool)
	}
	ch <- prometheus.MustNewConstMetric(c.kvFree, prometheus.GaugeValue, ratio, c.pool)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
