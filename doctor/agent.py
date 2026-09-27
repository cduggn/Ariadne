"""The investigation loop: a tool-using agent against any OpenAI-compatible endpoint (D-22). Stdlib only.

    run = run_task(task, backend, chat)          # chat = http_chat(base_url, model) or a fake in tests
    # -> {"diagnosis": {...}, "trace": [...], "stop": "submitted" | "inconclusive" | "step_cap" | …, "steps": [...]}

Prompt layout (fixed, so the serving stack can cache and route on it):
    system = triage ruleset + tool schemas (rendered by the chat template)   shared by every task
    user   = cluster card                                                    shared by every task on this cluster
    user   = the task: type, namespaces, the user's report                  first unique token
    then assistant tool calls and tool results, growing each step

Harness guards (carried over from the trip planner, D-13 there): tool_choice="required"; exact-repeat
calls refused; per-tool budgets scaled by the number of namespaces; a submit-now nudge near the step cap;
expect-blind validation of the diagnosis with up to two repairs; then fail closed to `inconclusive`.
"""
from __future__ import annotations

import json
import os
import time
import urllib.error
import urllib.request
import uuid
from collections.abc import Callable
from pathlib import Path

from . import tools as T
from .backends import Backend
from .card import cluster_card
from .validate import validate

HERE = Path(__file__).resolve().parent
TOOLS = json.loads((HERE / "schemas" / "tools.json").read_text())
RULESET = (HERE / "packs" / "triage.md").read_text()

MAX_TOKENS_PER_STEP = 768
CONTEXT_BUDGET = 15_000          # stop before prompt + max_tokens can pass --max-model-len 16384
HTTP_TIMEOUT_S = 120
MAX_REPAIRS = 2
WARN_STEPS_LEFT = 2
TOOL_BUDGET = {"list_problem_pods": 2, "get_events": 3, "describe": 4, "pod_logs": 4, "list_resources": 3,
               "resource_usage": 2, "s3_bucket_stats": 1, "cost_report": 2}          # per namespace in the task

Chat = Callable[[list[dict], str, dict], dict]      # (messages, request_id, headers) -> chat.completion


def user_prompt(task: dict) -> str:
    body = {"task_type": task["task_type"], "namespaces": task["namespaces"], "report": task["report"]}
    verb = "Investigate this report" if task["task_type"] == "investigate" else "Audit these namespaces"
    return f"{verb}. Finish by calling submit_diagnosis exactly once.\n" + json.dumps(body, ensure_ascii=False)


def request_headers(task: dict) -> dict:
    """What the gateway sees: who, which app, how urgent, and a data class that must never leave (D-25)."""
    return {"X-Tenant": task.get("tenant", "platform"), "X-App": "cluster-doctor",
            "X-Priority": "interactive" if task["task_type"] == "investigate" else "batch",
            "X-Data-Class": "restricted"}


def http_chat(base_url: str, model: str, *, temperature: float = 0.0) -> Chat:
    """A Chat bound to one endpoint. The API key comes only from VLLM_API_KEY."""
    key = os.environ.get("VLLM_API_KEY", "")

    def chat(messages: list[dict], request_id: str, headers: dict) -> dict:
        payload = {"model": model, "messages": messages, "tools": TOOLS, "tool_choice": "required",
                   "temperature": temperature, "max_tokens": MAX_TOKENS_PER_STEP,
                   "chat_template_kwargs": {"enable_thinking": False}}          # Qwen3 thinks by default
        h = {"Content-Type": "application/json", "X-Request-Id": request_id, **headers}
        if key:
            h["Authorization"] = f"Bearer {key}"
        req = urllib.request.Request(f"{base_url.rstrip('/')}/chat/completions", json.dumps(payload).encode(), h)
        with urllib.request.urlopen(req, timeout=HTTP_TIMEOUT_S) as r:
            return json.loads(r.read())

    return chat


def _parse_args(raw) -> tuple[dict | None, str]:
    if isinstance(raw, dict):
        return raw, ""
    try:
        args = json.loads(raw or "{}")
        return (args, "") if isinstance(args, dict) else (None, "arguments are not a JSON object")
    except json.JSONDecodeError as e:
        return None, f"arguments are not valid JSON: {e.msg}"


def _guard(name: str, args: dict, seen: set[str], used: dict[str, int], scale: int) -> dict | None:
    key = name + json.dumps(args, sort_keys=True)
    if key in seen:
        return {"error": f"duplicate call: {name} with these exact arguments already returned a result above. "
                         "Use that result and move to the next step."}
    limit = TOOL_BUDGET.get(name, 10**6) * scale
    if used.get(name, 0) >= limit:
        return {"error": f"budget for {name} used up ({limit} calls). Work with the results you already have."}
    seen.add(key)
    used[name] = used.get(name, 0) + 1
    return None


def inconclusive(rejected: dict, errors: list[str]) -> dict:
    return {"status": "inconclusive", "findings": [], "summary": "Could not produce a grounded diagnosis; escalate to a human.",
            "rejected_submission": rejected, "validation_errors": errors[:8]}


def run_task(task: dict, backend: Backend, chat: Chat, *, cluster: str = "doctor-lab", harness: bool = True) -> dict:
    run_id = f"{task['id']}-{uuid.uuid4().hex[:8]}"
    namespaces = task["namespaces"]
    max_steps = task.get("max_steps", 16)
    headers = request_headers(task)
    messages = [{"role": "system", "content": RULESET},
                {"role": "user", "content": cluster_card(backend, cluster)},    # shared per cluster → prefix-cacheable
                {"role": "user", "content": user_prompt(task)}]
    trace: list[str] = []
    steps: list[dict] = []
    diagnosis, stop = None, "step_cap"
    seen: set[str] = set()
    used: dict[str, int] = {}
    repairs = 0

    for n in range(1, max_steps + 1):
        rid = f"{run_id}-s{n}"
        t0 = time.perf_counter()
        try:
            resp = chat(messages, rid, headers)
        except urllib.error.HTTPError as e:
            steps.append({"step": n, "request_id": rid, "http_status": e.code, "latency_s": round(time.perf_counter() - t0, 3)})
            stop = f"http_{e.code}"                  # gateway sheds (429/503) end the task with a named reason
            break
        except (urllib.error.URLError, TimeoutError, OSError) as e:
            steps.append({"step": n, "request_id": rid, "error": type(e).__name__, "latency_s": round(time.perf_counter() - t0, 3)})
            stop = "transport_error"
            break
        usage = resp.get("usage") or {}
        msg = resp["choices"][0]["message"]
        calls = msg.get("tool_calls") or []
        step = {"step": n, "request_id": rid, "latency_s": round(time.perf_counter() - t0, 3),
                "prompt_tokens": usage.get("prompt_tokens"), "completion_tokens": usage.get("completion_tokens"),
                "cached_tokens": (usage.get("prompt_tokens_details") or {}).get("cached_tokens"),
                "finish_reason": resp["choices"][0].get("finish_reason"), "calls": []}
        steps.append(step)
        if not calls:
            messages.append({"role": "assistant", "content": msg.get("content") or ""})
            messages.append({"role": "user", "content": "Call a tool. Finish with submit_diagnosis."})
            step["calls"].append({"name": None, "error": "no tool call"})
            continue

        messages.append({"role": "assistant", "content": msg.get("content"), "tool_calls": calls})
        for c in calls:
            name = c["function"]["name"]
            args, err = _parse_args(c["function"].get("arguments"))
            if err:
                result = {"error": err}
            elif name == "submit_diagnosis":
                verdict = validate(args, backend, namespaces) if harness else {"pass": True, "failed": []}
                if verdict["pass"]:
                    diagnosis, result = args, {"accepted": True, "findings": len(args.get("findings", []))}
                elif repairs < MAX_REPAIRS:
                    repairs += 1
                    result = {"accepted": False, "errors": verdict["failed"][:8],
                              "fix": "Fix exactly these problems — copy refs exactly from tool results, name only objects that exist — "
                                     "then submit the whole diagnosis again."}
                else:
                    diagnosis = inconclusive(args, verdict["failed"])        # fail closed (D-24)
                    result = {"accepted": False, "final": "inconclusive"}
            else:
                result = (harness and _guard(name, args, seen, used, len(namespaces))) or T.call(backend, name, args)
            trace.append(name)
            step["calls"].append({"name": name, "error": result.get("error") if isinstance(result, dict) else None,
                                  "rejected": isinstance(result, dict) and result.get("accepted") is False})
            messages.append({"role": "tool", "tool_call_id": c.get("id", ""), "name": name,
                             "content": json.dumps(result, ensure_ascii=False)})
        if diagnosis is not None:
            stop = "inconclusive" if diagnosis.get("status") == "inconclusive" else "submitted"
            break
        if harness and max_steps - n == WARN_STEPS_LEFT:
            messages.append({"role": "user", "content": f"Only {WARN_STEPS_LEFT} model calls left. Call submit_diagnosis now "
                                                        "with the findings you can ground in evidence; status healthy if none."})
        if (usage.get("prompt_tokens") or 0) + (usage.get("completion_tokens") or 0) > CONTEXT_BUDGET:
            stop = "context_budget"
            break

    return {"task_id": task["id"], "run_id": run_id, "diagnosis": diagnosis, "trace": trace, "stop": stop,
            "steps": steps, "repairs": repairs, "harness": harness, "headers": headers}
