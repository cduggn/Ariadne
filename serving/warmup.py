#!/usr/bin/env python3
"""Write the request the gateway's warm-up probes replay (D-42).

    python3 -m serving.warmup qwen3-8b-awq > .cache/gateway/warm.json

It is the doctor's real step-1 body for the first golden task, captured from the same client the doctor uses, with
`max_completion_tokens` set to 1. The system ruleset, the tools and the cluster card come out exactly as live traffic
renders them, so a warm worker holds the prefix every run shares. Nothing leaves the process: the HTTP client is a
mock transport.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

import httpx

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from agent.agent import build_llm, initial_messages, load_profile  # noqa: E402
from evals.build_golden import backend_for  # noqa: E402

TASKS = ROOT / "evals" / "golden" / "tasks.jsonl"
_REPLY = {"id": "warmup", "object": "chat.completion", "created": 0, "model": "warmup",
          "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
          "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}


def warm_body(profile: str) -> bytes:
    sent: list[bytes] = []

    def answer(request: httpx.Request) -> httpx.Response:
        sent.append(request.content)
        return httpx.Response(200, json=_REPLY)

    task = json.loads(TASKS.read_text().splitlines()[0])
    served, client = load_profile(profile)
    llm = build_llm("http://warmup.invalid/v1", served, client=client,
                    http_client=httpx.Client(transport=httpx.MockTransport(answer)))
    llm.invoke(initial_messages(task, backend_for(task["snapshots"]), "doctor-lab"))
    body = json.loads(sent[0])
    body.pop("max_tokens", None)
    body["max_completion_tokens"] = 1
    return json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode()


if __name__ == "__main__":
    sys.stdout.buffer.write(warm_body(sys.argv[1] if len(sys.argv) > 1 else "qwen3-8b-awq"))
