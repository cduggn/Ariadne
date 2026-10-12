"""lab/verify.py turns a session's events and the gateway log into the F43, F44 and F45 verdicts."""
from __future__ import annotations

import json
from datetime import UTC, datetime

import pytest

from lab import verify

T0 = 1_791_500_000.0


def ev(kind, probe, t=T0, **detail):
    return {"t": t, "kind": kind, "probe": probe, "detail": detail}


def sample(run, pod, at_s, running, kv, tokens, aborts=0, t=T0):
    return ev("vllm_sample", "client-gone", t, run=run, pod=pod, at_s=at_s, running=running, kv_usage=kv,
              generation_tokens=tokens, finished={"stop": 10, "abort": aborts})


def gone_run(run, served, idle, aborted: bool):
    """A quiet cluster: the served pod takes the request; it either stops at the hang-up or runs on for 300 tokens."""
    rows = [sample(run, served, -1.0, 0, 0.10, 1000), sample(run, idle, -1.0, 0, 0.05, 2000),
            ev("client_gone_sent", "client-gone", run=run), ev("client_gone_aborted", "client-gone", run=run, seconds=3.0)]
    for at in verify_sample_at():
        if aborted:
            running, kv, tok = (1, 0.14, 1120) if at < 2 else (0, 0.10, 1130)
        else:
            running, kv, tok = (1, 0.14, 1000 + 40 * (3 + at)) if at < 10 else (0, 0.10, 1768)
        rows += [sample(run, served, at, running, kv, tok), sample(run, idle, at, 0, 0.05, 2000)]
    return rows


def verify_sample_at():
    from lab.probes import SAMPLE_AT
    return SAMPLE_AT


def test_client_gone_tells_an_abort_from_a_request_that_ran_on():
    events = gone_run("probe-gone-1", "vllm-1", "vllm-0", aborted=True) + gone_run("probe-gone-2", "vllm-0", "vllm-1", aborted=False)
    one, two = verify.client_gone_verdicts(events)
    assert (one["run"], one["pod"], one["verdict"], one["under_load"]) == ("probe-gone-1", "vllm-1", "aborted", False)
    assert "KV usage returned" in one["why"] and one["tokens_after_2s"] <= verify.ABORT_SLACK_TOKENS
    assert (two["pod"], two["verdict"], two["tokens_after_2s"]) == ("vllm-0", "ran_on", 1768 - 1200)


def test_client_gone_refuses_a_verdict_under_load_or_without_a_hang_up():
    busy = gone_run("probe-gone-1", "vllm-1", "vllm-0", aborted=True)
    busy[0]["detail"]["running"] = 3
    quiet = [sample("probe-gone-2", "vllm-0", -1.0, 0, 0.1, 5), ev("client_gone_finished_early", "client-gone", run="probe-gone-2", status=200)]
    old = [ev("client_gone_aborted", "client-gone", run="probe-gone-3", seconds=3.0)]
    one, two, three = verify.client_gone_verdicts(busy + quiet + old)
    assert (one["verdict"], one["under_load"]) == ("unclear", True)
    assert two["verdict"] == "unclear" and "never hung up" in two["why"]
    assert three["verdict"] == "unclear" and "no vllm_sample" in three["why"]


def req(run, step, pod, prompt, cached, hop="", status=200, t=T0, upstream_ms=900, hop_ms=0):
    return {"time": datetime.fromtimestamp(t, UTC).isoformat().replace("+00:00", "Z"), "msg": "request", "request_id": f"{run}-s{step}",
            "run": run, "step": step, "pod": pod, "status": status, "prompt_tokens": prompt, "cached_tokens": cached,
            "hop": hop, "hop_ms": hop_ms, "upstream_ms": upstream_ms}


def hop_log(transfer: bool):
    """Three runs: one stays put, one is moved below the threshold (recompute), one is hopped. Prefix is 3,900 tokens."""
    rows = [req("a", 1, "vllm-0", 4700, 3900), req("a", 2, "vllm-0", 5600, 4700), req("a", 3, "vllm-0", 6500, 5600),
            req("b", 1, "vllm-0", 4700, 3900), req("b", 2, "vllm-0", 5600, 4700), req("b", 3, "vllm-1", 6500, 3900, hop="below_threshold"),
            req("c", 1, "vllm-1", 4700, 3900), req("c", 2, "vllm-1", 5600, 4700),
            req("c", 3, "vllm-0", 6500, 5600 if transfer else 3900, hop="hopped", hop_ms=120)]
    return rows + [{"level": "INFO", "msg": "kv hop on"}, {"msg": "request", "run": "d", "step": 1, "status": 503}]


def test_hop_separates_transferred_from_recomputed_moves():
    got = verify.hop_groups(hop_log(transfer=True))
    assert got["shared_prefix_tokens"] == 3900
    assert {g: s["n"] for g, s in got["groups"].items()} == {"hopped": 1, "moved_recompute": 1, "same_pod": 4}
    assert got["groups"]["hopped"]["found_history_share"] == 1.0 and got["groups"]["moved_recompute"]["found_history_share"] == 0.0
    assert got["groups"]["hopped"]["median_hop_ms"] == 120 and got["verdict"] == "transferred"
    assert verify.hop_groups(hop_log(transfer=False))["verdict"] == "not_transferred"


def test_hop_without_a_hopped_step_is_unclear():
    log = [r for r in hop_log(True) if r.get("hop") != "hopped"]
    got = verify.hop_groups(log)
    assert got["verdict"] == "unclear" and "no hopped step" in got["why"]


def ramp_events(pod="vllm-1"):
    return [ev("worker_deleted", "worker-return", T0, pod=pod), ev("worker_phase", "worker-return", T0 + 1, pod=pod, phase="ready"),
            ev("worker_phase", "worker-return", T0 + 12, pod=pod, phase="down"),
            ev("worker_phase", "worker-return", T0 + 330, pod=pod, phase="warming"),
            ev("worker_phase", "worker-return", T0 + 337, pod=pod, phase="ready")]


def ramp_log():
    ready = T0 + 337
    rows = [req("x", 1, "vllm-1", 4700, 3900, t=T0 - 60), req("x", 2, "vllm-1", 5600, 4700, t=T0 - 30),
            req("x", 3, "vllm-1", 6500, 5600, t=T0 + 5, status=502, upstream_ms=None),
            req("y", 1, "vllm-0", 4700, 3900, t=T0 + 100), req("y", 2, "vllm-0", 5600, 4700, t=T0 + 200)]
    for i in range(6):                                   # the first minute back is slower, then it matches vllm-0
        slow = 2500 if i < 2 else 950
        rows += [req(f"r{i}", 1, "vllm-1", 4700, 3900, t=ready + 30 * i + 1, upstream_ms=slow),
                 req(f"r{i}", 2, "vllm-1", 5600, 4700, t=ready + 30 * i + 10, upstream_ms=slow),
                 req(f"r{i}", 3, "vllm-1", 6500, 5600, t=ready + 30 * i + 20, upstream_ms=slow),
                 req(f"o{i}", 1, "vllm-0", 4700, 3900, t=ready + 30 * i + 2), req(f"o{i}", 2, "vllm-0", 5600, 4700, t=ready + 30 * i + 15)]
    return rows


def test_ramp_reports_the_gap_and_each_window_after_ready():
    got = verify.ramp_windows(ramp_events(), ramp_log(), "vllm-1", windows=6)
    assert (got["down_s"], got["errors_while_down"]) == (337.0, 1)
    assert got["baseline_before_delete"] == {"placements": 2, "median_upstream_ms": 900, "median_cached_share": 0.835, "errors": 0}
    first, third = got["windows"][0], got["windows"][2]
    assert (first["returned"]["placements"], first["returned"]["median_upstream_ms"], first["others"]["median_upstream_ms"]) == (3, 2500, 900)
    assert (third["returned"]["median_upstream_ms"], third["runs_finished"]) == (950, 2)
    assert got["verdict"] == "ramped" and "by 60 s" in got["why"]


def test_ramp_without_a_return_is_unclear():
    got = verify.ramp_windows(ramp_events()[:3], ramp_log(), "vllm-1")
    assert got["verdict"] == "unclear"


def test_epoch_accepts_slog_nanoseconds():
    assert verify.epoch("2026-10-08T12:38:45.123456789Z") == pytest.approx(datetime(2026, 10, 8, 12, 38, 45, 123456, UTC).timestamp())


def test_cli_prints_a_verdict_line(tmp_path, capsys):
    log = tmp_path / "gw.jsonl"
    log.write_text("\n".join(json.dumps(r) for r in hop_log(True)) + "\nnot json\n")
    assert verify.main(["hop", str(log)]) == 0
    out = capsys.readouterr().out
    assert "verdict: transferred" in out and "hopped" in out
