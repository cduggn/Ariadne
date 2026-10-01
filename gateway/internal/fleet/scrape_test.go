package fleet

import (
	"math"
	"strings"
	"testing"
)

// vllmMetrics is a cut of a real vLLM /metrics body, with one engine and
// one model label set. The TTFT histogram holds 100 observations, 50 under
// 0.25s and 99 under 1s.
const vllmMetrics = `# HELP vllm:cache_config_info Information of the LLMEngine CacheConfig
# TYPE vllm:cache_config_info gauge
vllm:cache_config_info{block_size="16",cache_dtype="auto",calculate_kv_scales="False",cpu_offload_gb="0",enable_prefix_caching="True",gpu_memory_utilization="0.9",num_cpu_blocks="0",num_gpu_blocks="4941",sliding_window="None",swap_space="4"} 1.0
# HELP vllm:num_requests_running Number of requests in model execution batches.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0",model_name="Qwen/Qwen3-8B-AWQ"} 2.0
# HELP vllm:num_requests_waiting Number of requests waiting to be processed.
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0",model_name="Qwen/Qwen3-8B-AWQ"} 3.0
# HELP vllm:kv_cache_usage_perc KV-cache usage. 1 means 100 percent usage.
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0",model_name="Qwen/Qwen3-8B-AWQ"} 0.42
# HELP vllm:prompt_tokens_total Number of prefill tokens processed.
# TYPE vllm:prompt_tokens_total counter
vllm:prompt_tokens_total{engine="0",model_name="Qwen/Qwen3-8B-AWQ"} 812345.0
# HELP vllm:time_to_first_token_seconds Histogram of time to first token in seconds.
# TYPE vllm:time_to_first_token_seconds histogram
vllm:time_to_first_token_seconds_sum{engine="0",model_name="Qwen/Qwen3-8B-AWQ"} 41.7
vllm:time_to_first_token_seconds_bucket{engine="0",le="0.1",model_name="Qwen/Qwen3-8B-AWQ"} 0.0
vllm:time_to_first_token_seconds_bucket{engine="0",le="0.25",model_name="Qwen/Qwen3-8B-AWQ"} 50.0
vllm:time_to_first_token_seconds_bucket{engine="0",le="0.5",model_name="Qwen/Qwen3-8B-AWQ"} 50.0
vllm:time_to_first_token_seconds_bucket{engine="0",le="1.0",model_name="Qwen/Qwen3-8B-AWQ"} 99.0
vllm:time_to_first_token_seconds_bucket{engine="0",le="2.5",model_name="Qwen/Qwen3-8B-AWQ"} 100.0
vllm:time_to_first_token_seconds_bucket{engine="0",le="+Inf",model_name="Qwen/Qwen3-8B-AWQ"} 100.0
vllm:time_to_first_token_seconds_count{engine="0",model_name="Qwen/Qwen3-8B-AWQ"} 100.0
`

func mustParse(t *testing.T, body string) Sample {
	t.Helper()
	s, err := ParseMetrics(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseMetrics error = %v", err)
	}
	return s
}

func TestParseMetricsReadsVLLMBody(t *testing.T) {
	s := mustParse(t, vllmMetrics)

	if !s.OK {
		t.Fatal("OK = false, want true")
	}
	if s.KVUsage != 0.42 {
		t.Fatalf("KVUsage = %v, want 0.42", s.KVUsage)
	}
	if s.KVPoolTokens != 79056 {
		t.Fatalf("KVPoolTokens = %d, want 79056", s.KVPoolTokens)
	}
	if s.Waiting != 3 {
		t.Fatalf("Waiting = %v, want 3", s.Waiting)
	}
	wantBounds := []float64{0.1, 0.25, 0.5, 1, 2.5, math.Inf(1)}
	wantCounts := []float64{0, 50, 50, 99, 100, 100}
	if len(s.TTFT.Bounds) != len(wantBounds) {
		t.Fatalf("TTFT.Bounds = %v, want %v", s.TTFT.Bounds, wantBounds)
	}
	for i := range wantBounds {
		if s.TTFT.Bounds[i] != wantBounds[i] || s.TTFT.Counts[i] != wantCounts[i] {
			t.Fatalf("TTFT bucket %d = (%v, %v), want (%v, %v)", i, s.TTFT.Bounds[i], s.TTFT.Counts[i], wantBounds[i], wantCounts[i])
		}
	}
}

func TestParseMetricsNormalisesLegacyPercentName(t *testing.T) {
	s := mustParse(t, `# TYPE vllm:gpu_cache_usage_perc gauge
vllm:gpu_cache_usage_perc{model_name="m"} 42.0
`)
	if !s.OK || s.KVUsage != 0.42 {
		t.Fatalf("KVUsage = %v OK = %v, want 0.42 true", s.KVUsage, s.OK)
	}
	if s.KVPoolTokens != 0 {
		t.Fatalf("KVPoolTokens = %d, want 0 without cache_config_info", s.KVPoolTokens)
	}
}

func TestParseMetricsSumsAcrossLabelSets(t *testing.T) {
	s := mustParse(t, `# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0"} 0.1
vllm:kv_cache_usage_perc{engine="1"} 0.2
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0"} 1.0
vllm:num_requests_waiting{engine="1"} 4.0
# TYPE vllm:time_to_first_token_seconds histogram
vllm:time_to_first_token_seconds_bucket{engine="0",le="0.5"} 3.0
vllm:time_to_first_token_seconds_bucket{engine="0",le="+Inf"} 5.0
vllm:time_to_first_token_seconds_bucket{engine="1",le="0.5"} 2.0
vllm:time_to_first_token_seconds_bucket{engine="1",le="+Inf"} 7.0
`)
	if math.Abs(s.KVUsage-0.3) > 1e-9 {
		t.Fatalf("KVUsage = %v, want 0.3", s.KVUsage)
	}
	if s.Waiting != 5 {
		t.Fatalf("Waiting = %v, want 5", s.Waiting)
	}
	if got := s.TTFT.Counts; len(got) != 2 || got[0] != 5 || got[1] != 12 {
		t.Fatalf("TTFT.Counts = %v, want [5 12]", got)
	}
}

func TestParseMetricsWithoutKVUsageIsNotOK(t *testing.T) {
	s := mustParse(t, `# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0"} 3.0
`)
	if s.OK {
		t.Fatal("OK = true, want false without a KV usage metric")
	}
	if s.Waiting != 3 {
		t.Fatalf("Waiting = %v, want 3", s.Waiting)
	}
}

func TestParseMetricsRejectsMalformedBody(t *testing.T) {
	if _, err := ParseMetrics(strings.NewReader("vllm:kv_cache_usage_perc{engine=\"0\" 0.5\n")); err == nil {
		t.Fatal("ParseMetrics error = nil, want a parse error")
	}
}

func TestQuantileInterpolatesInsideBucket(t *testing.T) {
	h := mustParse(t, vllmMetrics).TTFT

	if got := Quantile(h, Histogram{}, 0.5); math.Abs(got-0.25) > 1e-9 {
		t.Fatalf("p50 = %v, want 0.25", got)
	}
	if got := Quantile(h, Histogram{}, 0.99); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("p99 = %v, want 1.0", got)
	}
	if got := Quantile(h, Histogram{}, 0.25); math.Abs(got-0.175) > 1e-9 {
		t.Fatalf("p25 = %v, want 0.175", got)
	}
}

func TestQuantileSubtractsPrev(t *testing.T) {
	h := mustParse(t, vllmMetrics).TTFT
	prev := Histogram{
		Bounds: h.Bounds,
		Counts: []float64{0, 0, 0, 0, 0, 0},
	}
	cur := Histogram{Bounds: h.Bounds, Counts: make([]float64, len(h.Counts))}
	for i := range h.Counts {
		prev.Counts[i] = 10
		cur.Counts[i] = h.Counts[i] + 10
	}

	if got := Quantile(cur, prev, 0.5); math.Abs(got-0.25) > 1e-9 {
		t.Fatalf("p50 of delta = %v, want 0.25", got)
	}
	if got := Quantile(cur, prev, 0.99); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("p99 of delta = %v, want 1.0", got)
	}
}

func TestQuantileEdges(t *testing.T) {
	h := mustParse(t, vllmMetrics).TTFT

	if got := Quantile(h, h, 0.5); got != 0 {
		t.Fatalf("p50 of empty delta = %v, want 0", got)
	}
	if got := Quantile(Histogram{}, Histogram{}, 0.5); got != 0 {
		t.Fatalf("p50 of empty histogram = %v, want 0", got)
	}

	tail := Histogram{Bounds: h.Bounds, Counts: []float64{0, 0, 0, 0, 0, 4}}
	if got := Quantile(tail, Histogram{}, 0.5); got != 2.5 {
		t.Fatalf("p50 in +Inf bucket = %v, want 2.5", got)
	}
}
