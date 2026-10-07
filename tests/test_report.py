"""report/results.py reads the committed metrics the notebook is built from (D-46)."""
from report import results


def test_histogram_buckets_add_up_to_the_count_vllm_reports():
    samples = results.prom("vllm-0-kv-control-20261007-160843")
    buckets = results.histogram(samples, "vllm:iteration_tokens_total")
    assert buckets[-1][0] == float("inf") and all(n >= 0 for _, n in buckets)
    assert sum(n for _, n in buckets) == results.total(samples, "vllm:iteration_tokens_total_count")


def test_labels_select_and_group_samples():
    samples = results.prom("gateway-sweep-c32-20261007-151105")
    reasons = results.by_label(samples, "orch_shed_total", "reason")
    assert reasons["timeout_queue"] == 125 and reasons["kv_free"] == 4
    assert results.total(samples, "orch_shed_total", reason="timeout_queue") == 125
    assert results.total(samples, "orch_restricted_offbox_total") == 0


def test_summaries_rows_and_kv_logs_load():
    assert results.summary("gw-38-20261006-131640")["pass_rate_v2"] == 0.865
    assert len(results.rows("gw-38-20261006-131640")) == 52
    assert results.kv_measured("qwen3.8-27b-fp8", "h100-half") == 51092
