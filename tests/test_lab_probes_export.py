"""lab/export.py saves the node's time series with the probe events; lab/probes.py fires the probes and logs them (D-48)."""
import json
import threading
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from lab import export, probes

MATRIX = {"status": "success", "data": {"resultType": "matrix", "result": [
    {"metric": {"__name__": "orch_replica_queue_depth", "pod": "vllm-0"}, "values": [[100, "1"], [105, "NaN"], [110, "2.5"]]},
    {"metric": {"__name__": "orch_replica_queue_depth", "pod": "vllm-1"}, "values": [[100, "0"]]}]}}
EMPTY = {"status": "success", "data": {"resultType": "matrix", "result": []}}


class FakeProm:
    def __init__(self):
        self.seen = []

    def __call__(self, path_and_query: str) -> dict:
        q = dict(urllib.parse.parse_qsl(path_and_query.split("?", 1)[1]))
        self.seen.append(q)
        if q["query"] == "vllm:kv_cache_usage_perc":
            raise RuntimeError("exit status 1: connection refused")
        return MATRIX if q["query"] in ("orch_replica_queue_depth", "vllm:num_requests_waiting") else EMPTY


@pytest.fixture
def lab(tmp_path, monkeypatch):
    monkeypatch.setattr(export, "METRICS", tmp_path / "metrics")
    monkeypatch.setattr(export, "MARKER", tmp_path / "bench.json")
    (tmp_path / "metrics").mkdir()
    return tmp_path


def written(lab) -> dict:
    [path] = (lab / "metrics").glob("ts-*.json")
    return json.loads(path.read_text())


def test_export_writes_floats_without_nan_records_errors_and_window_events(lab):
    (lab / "metrics/events-a.jsonl").write_text('{"t": 150, "kind": "b", "probe": "p", "detail": {}}\n'
                                                '{"t": 50, "kind": "early", "probe": "p", "detail": {}}\n')
    (lab / "metrics/events-b.jsonl").write_text('{"t": 120, "kind": "a", "probe": "p", "detail": {}}\n'
                                                '{"t": 999, "kind": "late", "probe": "p", "detail": {}}\n')
    (lab / "bench.json").write_text('{"tag": "run1", "start": 100}\n')
    assert export.main(["--end", "200"], fetch=FakeProm()) == 0
    ts = written(lab)
    assert (ts["tag"], ts["start"], ts["end"], ts["step_s"]) == ("run1", 100, 200, 5)
    assert ts["series"]["gw_queue_depth"]["results"] == [{"labels": {"pod": "vllm-0"}, "values": [[100.0, 1.0], [110.0, 2.5]]},
                                                         {"labels": {"pod": "vllm-1"}, "values": [[100.0, 0.0]]}]
    assert ts["series"]["vllm_waiting"]["query"] == "vllm:num_requests_waiting"
    assert ts["errors"] == {"vllm_kv_usage": "RuntimeError: exit status 1: connection refused"}
    assert "vllm_kv_usage" not in ts["series"]
    assert [e["kind"] for e in ts["events"]] == ["a", "b"]
    path = next((lab / "metrics").glob("ts-run1-*.json"))
    assert json.loads((lab / "bench.json").read_text()) == {"tag": "run1", "start": 100, "exported": str(path)}


def test_a_long_window_raises_the_step_to_fit_prometheus_point_cap(lab):
    prom = FakeProm()
    export.main(["--start", "0", "--end", str(20 * 3600), "--tag", "long"], fetch=prom)
    assert written(lab)["step_s"] == 7
    assert {q["step"] for q in prom.seen} == {"7"}


def test_a_window_set_by_hand_leaves_the_bench_unexported_so_make_down_still_warns(lab):
    (lab / "bench.json").write_text('{"tag": "run1", "start": 100}\n')
    assert export.main(["--start", "150", "--end", "200", "--tag", "slice"], fetch=FakeProm()) == 0
    assert json.loads((lab / "bench.json").read_text()) == {"tag": "run1", "start": 100}


def test_infinite_quantiles_are_dropped_like_nan(lab):
    inf = {"status": "success", "data": {"resultType": "matrix", "result": [
        {"metric": {}, "values": [[100, "+Inf"], [105, "0.5"], [110, "NaN"]]}]}}
    export.main(["--start", "100", "--end", "110", "--tag", "inf"], fetch=lambda _: inf)
    assert written(lab)["series"]["gw_e2e_p99"]["results"] == [{"labels": {}, "values": [[105.0, 0.5]]}]


def test_no_start_and_no_marker_exits_2(lab):
    assert export.main([], fetch=FakeProm()) == 2
    assert list((lab / "metrics").glob("ts-*.json")) == []


def test_no_data_in_any_series_exits_1(lab):
    assert export.main(["--start", "0", "--end", "60"], fetch=lambda _: EMPTY) == 1
    assert written(lab)["errors"] == {}


class Fake(BaseHTTPRequestHandler):
    """The gateway as the probes see it: scripted chat answers and a /debug/workers phase sequence."""
    chat_status, chat_delay, phases, requests = 200, 0.0, [], []

    def log_message(self, *a):
        pass

    def send(self, status, body):
        raw = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        type(self).requests.append({"headers": self.headers, "body": body})
        time.sleep(self.chat_delay)
        if self.chat_status != 200:
            return self.send(self.chat_status, {"error": {"message": "shed", "type": "overloaded", "code": "kv_free"}})
        self.send(200, {"choices": [{"message": {"role": "assistant", "content": "ok"}}], "usage": {"prompt_tokens": 19480}})

    def do_GET(self):
        phase = self.phases.pop(0) if len(self.phases) > 1 else self.phases[0]
        self.send(200, [{"pod": "vllm-0", "phase": "ready"}, {"pod": "vllm-1", "phase": phase}])


@pytest.fixture
def ctx(tmp_path, monkeypatch):
    monkeypatch.setattr(Fake, "requests", [])
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Fake)
    srv.daemon_threads = True
    threading.Thread(target=srv.serve_forever, args=(0.05,), daemon=True).start()
    commands = []
    c = probes.ProbeContext(f"http://127.0.0.1:{srv.server_port}/v1", "m", tmp_path / "events.jsonl",
                            lambda cmd: commands.append(cmd) or "", sleep=lambda s: None)
    c.commands = commands
    yield c
    srv.shutdown()
    srv.server_close()


def events(ctx) -> list[dict]:
    return [json.loads(line) for line in ctx.events.read_text().splitlines()]


def test_big_prompt_sends_a_unique_long_batch_prompt_and_logs_the_answer(ctx):
    probes.PROBES["big-prompt"](ctx, 1)
    sent, done = events(ctx)
    assert (sent["kind"], sent["probe"], sent["detail"]) == ("big_prompt_sent", "big-prompt", {"run": "probe-big-1", "prompt_tokens_est": 13000})
    assert done["kind"] == "big_prompt_done" and done["t"] >= sent["t"]
    assert {k: done["detail"][k] for k in ("run", "status", "prompt_tokens")} == {"run": "probe-big-1", "status": 200, "prompt_tokens": 19480}
    [req] = Fake.requests
    h = req["headers"]
    assert (h["X-Request-Id"], h["X-Priority"], h["X-App"], h["X-Tenant"]) == ("probe-big-1-s1", "batch", "probe", None)
    assert req["body"]["max_tokens"] == 16 and req["body"]["model"] == "m"
    assert len(req["body"]["messages"][0]["content"].split()) > 10_000


def test_a_refused_big_prompt_logs_the_status_and_reason(ctx, monkeypatch):
    monkeypatch.setattr(Fake, "chat_status", 503)
    probes.PROBES["big-prompt"](ctx, 2)
    assert events(ctx)[1] == {"t": events(ctx)[1]["t"], "kind": "big_prompt_refused", "probe": "big-prompt",
                              "detail": {"run": "probe-big-2", "status": 503, "reason": "kv_free"}}


def test_client_gone_hangs_up_before_a_slow_answer(ctx, monkeypatch):
    monkeypatch.setattr(Fake, "chat_delay", 5.0)
    ctx.abort_after = 0.3
    t0 = time.monotonic()
    probes.PROBES["client-gone"](ctx, 1)
    assert time.monotonic() - t0 < 2
    sent, gone = events(ctx)
    assert (sent["kind"], gone["kind"]) == ("client_gone_sent", "client_gone_aborted")
    assert 0.3 <= gone["detail"]["seconds"] < 2
    assert Fake.requests[0]["headers"]["X-Request-Id"] == "probe-gone-1-s1" and Fake.requests[0]["body"]["max_tokens"] == 768


def test_worker_return_deletes_the_pod_and_follows_its_phase_back_to_ready(ctx, monkeypatch):
    monkeypatch.setattr(Fake, "phases", ["ready", "down", "down", "warming", "ready"])
    probes.PROBES["worker-return"](ctx, 1)
    assert ctx.commands == [["kubectl", "delete", "pod", "vllm-1", "--wait=false"]]
    assert [(e["kind"], e["detail"].get("phase")) for e in events(ctx)] == [
        ("worker_deleted", None), ("worker_phase", "ready"), ("worker_phase", "down"), ("worker_phase", "warming"), ("worker_phase", "ready")]


def test_session_brackets_its_probes_and_survives_one_that_fails(ctx, monkeypatch):
    def broken(c, n):
        raise RuntimeError("gateway gone")
    monkeypatch.setitem(probes.PROBES, "broken", broken)
    assert probes.session(ctx, "true", [(0, "big-prompt"), (0, "broken")]) == 0
    got = events(ctx)
    assert (got[0]["kind"], got[-1]["kind"], got[-1]["detail"]) == ("session_start", "session_end", {"load_exit": 0})
    assert sorted(e["kind"] for e in got[1:-1]) == ["big_prompt_done", "big_prompt_sent", "probe_error"]
    assert next(e for e in got if e["kind"] == "probe_error")["detail"] == {"error": "RuntimeError: gateway gone"}
