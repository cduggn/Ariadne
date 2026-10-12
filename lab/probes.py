#!/usr/bin/env python3
"""Probe the queue on purpose during load, and log when each probe fired to metrics/events-<tag>-<stamp>.jsonl (D-48).

    python -m lab.probes session --tag run1-probes --load-cmd "make golden CONC=4 TAG=run1-probes"
    python -m lab.probes big-prompt --tag manual -n 1
    python -m lab.probes worker-return --tag manual --pod vllm-1
    python -m lab.probes session --tag ramp --schedule "30:worker-return" --load-cmd "make golden CONC=4 REPEAT=4 TAG=ramp"

big-prompt     one unique ~14k-token batch prompt: a long prefill next to the agents' decodes
client-gone    a long answer whose client hangs up after --abort-after seconds: who frees the KV. Samples each worker's
               vLLM metrics through the gateway before the request and at SAMPLE_AT seconds after the hang-up, so
               lab/verify.py can tell whether the engine stopped generating and gave the KV back (F43)
worker-return  delete a vLLM pod and follow its phase on the gateway until it is ready again

`session` starts --load-cmd as background load and fires the schedule's probes at their offsets (SCHEDULE, or
--schedule "offset:probe,..."). Each event is one JSON line, appended and flushed as it happens, so a crash leaves a
valid partial log. lab/export.py pairs the events with the node's time series; lab/verify.py reads the verdicts.
"""
from __future__ import annotations

import argparse
import json
import math
import random
import re
import shlex
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path

from serving.profiles import load_profile

ROOT = Path(__file__).resolve().parents[1]

WORDS = ("queue scheduler replica prefill decode token cache block page batch admit shed gateway worker pod node "
         "latency budget tenant priority window chunk stream socket kernel tensor memory weight layer router probe "
         "metric series sample bucket quantile signal deadline request response engine cluster volume network").split()
TOKENS_PER_WORD = 1.3
# ~14k tokens by the gateway's bytes/3.5 estimate: two 8,192-token prefill chunks, yet small enough to pass the door's
# KV check while golden runs hold part of a 51k-token half (a ~21k prompt was refused as kv_free on paper).
BIG_WORDS = 10_000
SCHEDULE: list[tuple[float, str]] = [(60, "big-prompt"), (90, "big-prompt"), (120, "big-prompt"), (180, "client-gone"),
                                     (200, "client-gone"), (220, "client-gone"), (300, "worker-return")]
# Seconds after the client hangs up at which client-gone samples the workers. An abort shows within 2 s; a request that
# runs on for its 768 tokens keeps vllm:generation_tokens_total climbing past 5 s.
SAMPLE_AT = (1.0, 2.0, 3.0, 5.0, 10.0, 20.0, 40.0)
METRIC_RE = re.compile(r"^([A-Za-z_:][A-Za-z0-9_:]*)(\{[^}]*\})?\s+(\S+)")
SAMPLED = {"vllm:num_requests_running": "running", "vllm:num_requests_waiting": "waiting",
           "vllm:kv_cache_usage_perc": "kv_usage", "vllm:gpu_cache_usage_perc": "kv_usage",
           "vllm:generation_tokens_total": "generation_tokens", "vllm:num_preemptions_total": "preemptions"}


def parse_schedule(text: str) -> list[tuple[float, str]]:
    """"30:worker-return,60:client-gone" as (offset, probe) pairs; an unknown probe is a usage error."""
    out = []
    for item in filter(None, (part.strip() for part in text.split(","))):
        offset, _, name = item.partition(":")
        if name not in PROBES:
            raise SystemExit(f"unknown probe {name!r} in --schedule; choose from {', '.join(PROBES)}")
        out.append((float(offset), name))
    return out


def parse_vllm(text: str) -> dict:
    """The vLLM series the verdicts need, summed over their labels, with request_success_total split by finished_reason."""
    out: dict = {"finished": {}}
    for line in text.splitlines():
        m = METRIC_RE.match(line)
        if not m:
            continue
        name, labels, raw = m.groups()
        try:
            v = float(raw)
        except ValueError:
            continue
        if not math.isfinite(v):
            continue
        if name in SAMPLED:
            out[SAMPLED[name]] = out.get(SAMPLED[name], 0.0) + v
        elif name == "vllm:request_success_total":
            reason = re.search(r'finished_reason="([^"]*)"', labels or "")
            key = reason.group(1) if reason else "?"
            out["finished"][key] = out["finished"].get(key, 0.0) + v
    return out


@dataclass
class ProbeContext:
    base_url: str
    model: str
    events: Path
    remote: Callable[[list[str]], str]
    now: Callable[[], float] = time.time
    sleep: Callable[[float], None] = time.sleep
    abort_after: float = 3.0
    pod: str = "vllm-1"
    return_timeout: float = 1200.0
    pods: tuple[str, ...] = ("vllm-0", "vllm-1")
    lock: threading.Lock = field(default_factory=threading.Lock)

    def event(self, kind: str, probe: str, **detail) -> None:
        line = json.dumps({"t": self.now(), "kind": kind, "probe": probe, "detail": detail})
        with self.lock, self.events.open("a") as f:
            f.write(line + "\n")
            print(line, flush=True)


def lam_remote(node: str) -> Callable[[list[str]], str]:
    def remote(cmd: list[str]) -> str:                  # ssh joins the argv into a remote shell line, so quote each word
        return subprocess.run(["lam", "ssh", node, "--", *map(shlex.quote, cmd)], check=True, capture_output=True, text=True).stdout
    return remote


def chat(ctx: ProbeContext, messages: list[dict], max_tokens: int, headers: dict, timeout: float) -> tuple[int, dict]:
    """(status, body) for a chat completion; an HTTP error status is a result here, not an exception."""
    req = urllib.request.Request(ctx.base_url.rstrip("/") + "/chat/completions", method="POST",
                                 data=json.dumps({"model": ctx.model, "messages": messages, "max_tokens": max_tokens}).encode(),
                                 headers={"Content-Type": "application/json", **headers})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.load(r)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw)
        except ValueError:
            return e.code, {"error": {"message": raw.decode(errors="replace")[:200]}}


def big_prompt(ctx: ProbeContext, n: int) -> None:
    run = f"probe-big-{n}"
    rng = random.Random(f"{run}@{ctx.now()}")  # noqa: S311 (the clock too, so a second session's probes miss the prefix cache)
    text = " ".join(rng.choice(WORDS) for _ in range(BIG_WORDS))
    ctx.event("big_prompt_sent", "big-prompt", run=run, prompt_tokens_est=round(BIG_WORDS * TOKENS_PER_WORD))
    t0 = ctx.now()
    # No X-Tenant: the platform tenant's token bucket never binds, so the probe meets the queue, not the rate limit.
    status, body = chat(ctx, [{"role": "user", "content": "Reply with one word.\n\n" + text}], 16,
                        {"X-Request-Id": f"{run}-s1", "X-Priority": "batch", "X-App": "probe"}, timeout=600)
    if status == 200:
        ctx.event("big_prompt_done", "big-prompt", run=run, status=status, seconds=round(ctx.now() - t0, 3),
                  prompt_tokens=body.get("usage", {}).get("prompt_tokens"))
    else:
        err = body.get("error") or {}
        ctx.event("big_prompt_refused", "big-prompt", run=run, status=status, reason=err.get("code") or err.get("message"))


def vllm_sample(ctx: ProbeContext, pod: str) -> dict | None:
    """One worker's vLLM metrics through the gateway's /debug/workers/{pod}/metrics, or None if unreachable."""
    url = ctx.base_url.rstrip("/").removesuffix("/v1") + f"/debug/workers/{pod}/metrics"
    try:
        with urllib.request.urlopen(url, timeout=10) as r:
            return parse_vllm(r.read().decode(errors="replace"))
    except (urllib.error.URLError, TimeoutError):
        return None


def sample_pods(ctx: ProbeContext, probe: str, run: str, at_s: float) -> None:
    for pod in ctx.pods:
        sample = vllm_sample(ctx, pod)
        ctx.event("vllm_sample", probe, run=run, pod=pod, at_s=at_s, **(sample if sample is not None else {"error": "unreachable"}))


def client_gone(ctx: ProbeContext, n: int) -> None:
    run = f"probe-gone-{n}"
    sample_pods(ctx, "client-gone", run, at_s=-1.0)
    ctx.event("client_gone_sent", "client-gone", run=run)
    t0 = ctx.now()
    try:
        status, _ = chat(ctx, [{"role": "user", "content": "Write a detailed 2,000-word essay about Kubernetes scheduling."}], 768,
                         {"X-Request-Id": f"{run}-s1", "X-Priority": "interactive", "X-App": "probe"}, timeout=ctx.abort_after)
    except (TimeoutError, urllib.error.URLError) as e:  # urllib wraps a timeout before the response in URLError
        if not isinstance(e, TimeoutError) and not isinstance(e.reason, TimeoutError):
            raise
        gone = ctx.now()
        ctx.event("client_gone_aborted", "client-gone", run=run, seconds=round(gone - t0, 3))
        for at in SAMPLE_AT:
            ctx.sleep(max(0.0, gone + at - ctx.now()))
            sample_pods(ctx, "client-gone", run, at_s=at)
        return
    ctx.event("client_gone_finished_early", "client-gone", run=run, status=status)


def worker_return(ctx: ProbeContext, n: int) -> None:
    ctx.remote(["kubectl", "delete", "pod", ctx.pod, "--wait=false"])
    ctx.event("worker_deleted", "worker-return", pod=ctx.pod)
    workers = ctx.base_url.rstrip("/").removesuffix("/v1") + "/debug/workers"
    deadline, last = ctx.now() + ctx.return_timeout, None
    while ctx.now() < deadline:
        try:
            with urllib.request.urlopen(workers, timeout=10) as r:
                phase = next((w["phase"] for w in json.load(r) if w["pod"] == ctx.pod), None)
        except (urllib.error.URLError, TimeoutError, ValueError):   # one failed poll must not lose the return timeline
            ctx.sleep(2)
            continue
        if phase != last:
            ctx.event("worker_phase", "worker-return", pod=ctx.pod, phase=phase)
            # The gateway can still say ready right after the delete; only a return to ready ends the probe.
            if phase == "ready" and last is not None:
                return
            last = phase
        ctx.sleep(2)
    ctx.event("worker_return_timeout", "worker-return", pod=ctx.pod, seconds=ctx.return_timeout)


PROBES: dict[str, Callable[[ProbeContext, int], None]] = {
    "big-prompt": big_prompt,
    "client-gone": client_gone,
    "worker-return": worker_return,
}


def session(ctx: ProbeContext, load_cmd: str | None, schedule: list[tuple[float, str]] = SCHEDULE) -> int:
    """Fire each probe in its own thread at its offset from the start while load_cmd runs; the load's exit code."""
    ctx.event("session_start", "session", load_cmd=load_cmd, schedule=schedule)
    load = subprocess.Popen(load_cmd, shell=True) if load_cmd else None  # noqa: S602 (the Makefile's own command line)
    t0, counts = ctx.now(), dict.fromkeys(PROBES, 0)

    def fire(offset: float, name: str, n: int) -> None:
        ctx.sleep(max(0.0, t0 + offset - ctx.now()))
        try:
            PROBES[name](ctx, n)
        except Exception as e:                          # a probe failing must not stop the others or the load
            ctx.event("probe_error", name, error=f"{type(e).__name__}: {e}")

    threads = []
    for offset, name in schedule:
        counts[name] += 1
        threads.append(threading.Thread(target=fire, args=(offset, name, counts[name])))
        threads[-1].start()
    for t in threads:
        t.join()
    code = load.wait() if load else 0
    ctx.event("session_end", "session", load_exit=code)
    return code


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("what", choices=["session", *PROBES])
    ap.add_argument("--tag", required=True)
    ap.add_argument("-n", type=int, default=1, help="the probe's number, in its run id (one probe by hand)")
    ap.add_argument("--events", help="default: metrics/events-<tag>-<stamp>.jsonl")
    ap.add_argument("--base-url", default="http://127.0.0.1:8000/v1", help="the gateway")
    ap.add_argument("--profile", default="qwen3-8b-awq", help="model profile in deploy/models/ (the served name)")
    ap.add_argument("--model", help="served model name override (default: the profile's)")
    ap.add_argument("--node", default="cluster-doctor", help="the lam instance worker-return deletes a pod on")
    ap.add_argument("--load-cmd", help="session: the background load, one shell command line")
    ap.add_argument("--schedule", help='session: "offset:probe,..." instead of SCHEDULE, e.g. "30:worker-return"')
    ap.add_argument("--pods", default="vllm-0,vllm-1", help="client-gone: the workers to sample, comma-separated")
    ap.add_argument("--abort-after", type=float, default=3.0, help="client-gone: seconds before the client hangs up")
    ap.add_argument("--pod", default="vllm-1", help="worker-return: the pod to delete")
    ap.add_argument("--return-timeout", type=float, default=1200.0, help="worker-return: seconds to wait for ready")
    a = ap.parse_args(argv)
    events = Path(a.events) if a.events else ROOT / "metrics" / f"events-{a.tag}-{time.strftime('%Y%m%d-%H%M%S')}.jsonl"
    events.parent.mkdir(parents=True, exist_ok=True)
    ctx = ProbeContext(a.base_url, a.model or load_profile(a.profile)["serve"]["served_name"], events, lam_remote(a.node),
                       abort_after=a.abort_after, pod=a.pod, return_timeout=a.return_timeout,
                       pods=tuple(filter(None, a.pods.split(","))))
    if a.what == "session":
        return session(ctx, a.load_cmd, parse_schedule(a.schedule) if a.schedule else SCHEDULE)
    PROBES[a.what](ctx, a.n)
    return 0


if __name__ == "__main__":
    sys.exit(main())
