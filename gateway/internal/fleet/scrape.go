package fleet

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// maxMetricsBytes caps one /metrics body, so a misbehaving worker cannot
// exhaust the gateway's memory through the scrape path.
const maxMetricsBytes = 8 << 20

// Histogram is one Prometheus histogram's cumulative bucket counts. Bounds
// are the upper bounds in ascending order with +Inf last, and Counts has
// one cumulative count per bound. Two histograms with the same Bounds can
// be subtracted bucket by bucket.
type Histogram struct {
	Bounds []float64
	Counts []float64
}

// Sample is one worker's /metrics body reduced to what the Gate needs.
// KVUsage is a 0 to 1 ratio. KVPoolTokens is the KV pool's capacity in
// tokens, or 0 when vLLM did not export its cache config. OK is set only
// when the body carried a KV usage metric, because without it the sample
// says nothing about the worker's load.
type Sample struct {
	KVUsage      float64
	KVPoolTokens int
	Waiting      float64
	TTFT         Histogram
	OK           bool
}

// ParseMetrics reads one Prometheus text exposition body into a Sample.
// It reads at most maxMetricsBytes. It sums gauges across label sets, and
// histogram buckets with the same bound. KV usage is
// normalised per label set, because older vLLM releases exported it as a
// percentage. It returns a panic inside the upstream parser as an error,
// because a malformed body from another process must not take down the
// gateway.
func ParseMetrics(r io.Reader) (s Sample, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			s, err = Sample{}, fmt.Errorf("malformed metrics body: %v", rec)
		}
	}()

	p := expfmt.NewTextParser(model.UTF8Validation)
	families, err := p.TextToMetricFamilies(io.LimitReader(r, maxMetricsBytes))
	if err != nil {
		return Sample{}, fmt.Errorf("parse metrics: %w", err)
	}

	if mf, ok := first(families, "vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc"); ok {
		for _, m := range mf.GetMetric() {
			s.KVUsage += ratio(value(m))
		}
		s.OK = true
	}
	if mf, ok := first(families, "vllm:num_requests_waiting"); ok {
		for _, m := range mf.GetMetric() {
			s.Waiting += value(m)
		}
	}
	if mf, ok := first(families, "vllm:cache_config_info"); ok {
		for _, m := range mf.GetMetric() {
			s.KVPoolTokens += labelInt(m, "num_gpu_blocks") * labelInt(m, "block_size")
		}
	}
	if mf, ok := first(families, "vllm:time_to_first_token_seconds"); ok {
		s.TTFT = histogram(mf)
	}
	return s, nil
}

// first returns the first family present under any of names. vLLM has
// renamed metrics across releases, and reading zero from a renamed metric
// would make the Gate confidently wrong.
func first(families map[string]*dto.MetricFamily, names ...string) (*dto.MetricFamily, bool) {
	for _, n := range names {
		if mf, ok := families[n]; ok {
			return mf, true
		}
	}
	return nil, false
}

// value reads a gauge, counter or untyped sample's value.
func value(m *dto.Metric) float64 {
	switch {
	case m.GetGauge() != nil:
		return m.GetGauge().GetValue()
	case m.GetCounter() != nil:
		return m.GetCounter().GetValue()
	default:
		return m.GetUntyped().GetValue()
	}
}

// ratio turns a usage that may be a percentage into a 0 to 1 ratio.
func ratio(v float64) float64 {
	if v > 1 {
		return v / 100
	}
	return v
}

// labelInt reads an integer label from a metric, or 0 when it is missing
// or not a number.
func labelInt(m *dto.Metric, name string) int {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			n, err := strconv.Atoi(lp.GetValue())
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
}

// histogram sums a family's buckets across label sets by bound.
func histogram(mf *dto.MetricFamily) Histogram {
	byBound := make(map[float64]float64)
	for _, m := range mf.GetMetric() {
		for _, b := range m.GetHistogram().GetBucket() {
			byBound[b.GetUpperBound()] += float64(b.GetCumulativeCount())
		}
	}
	h := Histogram{Bounds: make([]float64, 0, len(byBound)), Counts: make([]float64, 0, len(byBound))}
	for bound := range byBound {
		h.Bounds = append(h.Bounds, bound)
	}
	sort.Float64s(h.Bounds)
	for _, bound := range h.Bounds {
		h.Counts = append(h.Counts, byBound[bound])
	}
	return h
}

// Quantile returns the q quantile of the observations in h that are not in
// prev, interpolating linearly inside the chosen bucket as Prometheus's
// histogram_quantile does. prev may be empty. A prev whose bounds differ
// from h's is ignored, because the two cannot be subtracted. The +Inf
// bucket returns the highest finite bound. An empty delta returns 0.
func Quantile(h, prev Histogram, q float64) float64 {
	n := len(h.Bounds)
	if n == 0 || len(h.Counts) != n {
		return 0
	}
	delta := make([]float64, n)
	copy(delta, h.Counts)
	if sameBounds(h, prev) {
		for i := range delta {
			delta[i] -= prev.Counts[i]
		}
	}
	total := delta[n-1]
	if total <= 0 {
		return 0
	}
	q = math.Max(0, math.Min(1, q))
	rank := q * total
	b := sort.Search(n, func(i int) bool { return delta[i] >= rank })
	if b >= n-1 {
		if n < 2 {
			return 0
		}
		return h.Bounds[n-2]
	}
	start, before := 0.0, 0.0
	if b > 0 {
		start, before = h.Bounds[b-1], delta[b-1]
	}
	end, count := h.Bounds[b], delta[b]
	if count <= before {
		return end
	}
	return start + (end-start)*(rank-before)/(count-before)
}

// sameBounds reports whether prev has h's bounds and a count per bound, so
// the two can be subtracted bucket by bucket.
func sameBounds(h, prev Histogram) bool {
	if len(prev.Bounds) != len(h.Bounds) || len(prev.Counts) != len(h.Bounds) {
		return false
	}
	for i := range h.Bounds {
		if prev.Bounds[i] != h.Bounds[i] {
			return false
		}
	}
	return true
}
