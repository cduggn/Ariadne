"""report/results.py reads the committed metrics the notebook is built from (D-46)."""
import json

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


def _write_ts(dir_, name, tag, start):
    ts = {"tag": tag, "start": start, "end": start + 20, "step_s": 5,
          "series": {"vllm_waiting": {"query": "vllm:num_requests_waiting", "results": [
                         {"labels": {"pod": "vllm-0"}, "values": [[start, 0.0], [start + 5, 3.0]]},
                         {"labels": {"pod": "vllm-1"}, "values": [[start + 10, 1.5]]}]},
                     "gw_phase": {"query": "orch_replica_phase", "results": [
                         {"labels": {"pod": "vllm-1"}, "values": [[start, 1.0]]}]}},
          "events": [], "errors": {"gw_phase": "bad_data", "gpu_power_w": "timeout"}}
    (dir_ / name).write_text(json.dumps(ts))


def test_timeseries_picks_the_newest_export_by_stamp_and_tag(tmp_path, monkeypatch):
    monkeypatch.setattr(results, "METRICS", tmp_path)
    assert results.timeseries() is None
    _write_ts(tmp_path, "ts-zz-20261007-100000.json", "zz", 1.0)
    _write_ts(tmp_path, "ts-aa-20261007-120000.json", "aa", 2.0)
    _write_ts(tmp_path, "ts-aa-20261006-230000.json", "aa", 3.0)
    _write_ts(tmp_path, "ts-zz-x-20261008-000000.json", "zz-x", 4.0)
    assert results.timeseries()["start"] == 4.0
    assert results.timeseries("aa")["start"] == 2.0
    assert results.timeseries("zz")["start"] == 1.0
    assert results.timeseries("run1") is None


def test_series_gives_times_from_the_window_start_and_skips_errors(tmp_path, monkeypatch):
    monkeypatch.setattr(results, "METRICS", tmp_path)
    _write_ts(tmp_path, "ts-run1-20261007-100000.json", "run1", 1791400000.0)
    ts = results.timeseries("run1")
    assert results.series(ts, "vllm_waiting") == [({"pod": "vllm-0"}, [0.0, 5.0], [0.0, 3.0]),
                                                  ({"pod": "vllm-1"}, [10.0], [1.5])]
    assert results.series(ts, "gw_phase") == []
    assert results.series(ts, "gpu_power_w") == []
    assert results.series(ts, "gw_inflight") == []
