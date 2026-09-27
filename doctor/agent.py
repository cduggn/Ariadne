"""The investigation agent as a LangGraph state graph over LangChain tools (D-22, D-33).

    llm = build_llm(base_url, model)                 # ChatOpenAI bound to tools.json, forced tool calls
    run = run_task(task, backend, llm)               # -> {"diagnosis", "trace", "stop", "steps", "repairs", …}

Graph (state = messages + counters + diagnosis; backend, task and model travel in the run config):

      START ──► agent ──(model error)──────────────────────────────► END   stop = http_<code> | transport_error
                  │
                  ▼
                 act ──(diagnosis accepted / failed closed)────────► END   stop = submitted | inconclusive
                  │ ──(step cap or context budget)─────────────────► END   stop = step_cap | context_budget
                  └──────────────► agent

  agent  one model call. Prompt layout is fixed so the serving stack can cache and route on it:
         system = triage ruleset (+ tool schemas rendered by the chat template) · user = cluster card ·
         user = task · then assistant tool calls and tool results.
  act    executes every tool call through LangChain StructuredTools, behind the harness guards (exact-repeat
         refusal, per-tool budgets × namespaces, submit-now nudge). `submit_diagnosis` is validated here
         (expect-blind, doctor/validate.py): up to 2 repairs, then FAIL CLOSED to `inconclusive` (D-24).

Every request carries X-Request-Id (task-run-step), X-Tenant, X-App, X-Priority and X-Data-Class, set per
step through an httpx hook, so the gateway can admit, place and refuse without parsing bodies (D-25).
"""
from __future__ import annotations

import contextvars
import json
import operator
import os
import time
import uuid
from pathlib import Path
from typing import Annotated, Any, TypedDict

import httpx
import openai
from langchain_core.language_models import BaseChatModel
from langchain_core.messages import AnyMessage, HumanMessage, SystemMessage, ToolMessage
from langchain_openai import ChatOpenAI
from langgraph.graph import END, START, StateGraph
from langgraph.graph.message import add_messages

from .backends import Backend
from .card import cluster_card
from .lc_tools import make_tools
from .validate import validate

# Cluster data must never leave self-hosted infrastructure (D-19, INV-13): LangChain's hosted tracing
# (LangSmith) is forced off regardless of the caller's environment. Use OpenTelemetry/self-hosted tracing instead.
for _v in ("LANGSMITH_TRACING", "LANGCHAIN_TRACING_V2", "LANGCHAIN_TRACING"):
    os.environ[_v] = "false"

HERE = Path(__file__).resolve().parent
TOOLS = json.loads((HERE / "schemas" / "tools.json").read_text())
RULESET = (HERE / "packs" / "triage.md").read_text()

MAX_TOKENS_PER_STEP = 768
CONTEXT_BUDGET = 23_000          # stop before prompt + max_tokens (768) can pass --max-model-len 24576
HTTP_TIMEOUT_S = 120
MAX_REPAIRS = 2
WARN_STEPS_LEFT = 2
TOOL_BUDGET = {"list_problem_pods": 2, "get_events": 3, "describe": 5, "pod_logs": 5, "list_resources": 3,
               "resource_usage": 2, "inspect_certificate": 3, "rightsizing": 1, "s3_bucket_stats": 1,
               "cost_report": 2}                                                     # per namespace in the task

_REQUEST_HEADERS: contextvars.ContextVar[dict | None] = contextvars.ContextVar("doctor_request_headers", default=None)


# ---- prompt and request shape ---------------------------------------------------------------

def user_prompt(task: dict) -> str:
    body = {"task_type": task["task_type"], "namespaces": task["namespaces"], "report": task["report"]}
    verb = {"investigate": "Investigate this report", "audit": "Audit these namespaces",
            "rightsize": "Find over-provisioned workloads"}[task["task_type"]]
    return f"{verb}. Finish by calling submit_diagnosis exactly once.\n" + json.dumps(body, ensure_ascii=False)


def request_headers(task: dict) -> dict:
    """What the gateway sees: who, which app, how urgent, and a data class that must never leave (D-25)."""
    return {"X-Tenant": task.get("tenant", "platform"), "X-App": "cluster-doctor",
            "X-Priority": "interactive" if task["task_type"] == "investigate" else "batch",   # audit, rightsize
            "X-Data-Class": "restricted"}


def initial_messages(task: dict, backend: Backend, cluster: str) -> list[AnyMessage]:
    return [SystemMessage(RULESET), HumanMessage(cluster_card(backend, cluster)), HumanMessage(user_prompt(task))]


def _add_headers(request: httpx.Request) -> None:
    for k, v in (_REQUEST_HEADERS.get() or {}).items():
        request.headers[k] = v


def build_llm(base_url: str, model: str, *, http_client: httpx.Client | None = None, temperature: float = 0.0):
    """ChatOpenAI against any OpenAI-compatible endpoint (vLLM or the gateway), bound to tools.json verbatim.
    The API key comes only from VLLM_API_KEY. Retries are off: a gateway 429/503 must surface, not be hidden."""
    client = http_client or httpx.Client(timeout=HTTP_TIMEOUT_S)
    client.event_hooks.setdefault("request", []).append(_add_headers)
    llm = ChatOpenAI(model=model, base_url=base_url, api_key=os.environ.get("VLLM_API_KEY") or "EMPTY",
                     temperature=temperature, max_tokens=MAX_TOKENS_PER_STEP, max_retries=0, http_client=client,
                     extra_body={"chat_template_kwargs": {"enable_thinking": False}})    # Qwen3 thinks by default
    return llm.bind_tools(TOOLS, tool_choice="required", strict=True)


# ---- graph -----------------------------------------------------------------------------------

class State(TypedDict):
    messages: Annotated[list[AnyMessage], add_messages]
    n: int                                        # model calls made
    trace: Annotated[list[str], operator.add]     # tool names called, in order
    steps: Annotated[list[dict], operator.add]    # one record per model call
    calls: Annotated[list[dict], operator.add]    # one record per tool call
    repairs: int
    seen: list[str]
    used: dict[str, int]
    diagnosis: dict | None
    stop: str
    last_tokens: int


def inconclusive(rejected: dict, errors: list[str]) -> dict:
    return {"status": "inconclusive", "findings": [], "summary": "Could not produce a grounded diagnosis; escalate to a human.",
            "rejected_submission": rejected, "validation_errors": errors[:8]}


def _cfg(config) -> dict:
    return config["configurable"]


def agent_node(state: State, config) -> dict:
    c = _cfg(config)
    n = state["n"] + 1
    rid = f"{c['run_id']}-s{n}"
    token = _REQUEST_HEADERS.set({**request_headers(c["task"]), "X-Request-Id": rid})
    t0 = time.perf_counter()
    try:
        msg = c["llm"].invoke(state["messages"])
    except openai.APIStatusError as e:              # gateway sheds (429/503) end the task with a named reason
        return {"n": n, "stop": f"http_{e.status_code}",
                "steps": [{"step": n, "request_id": rid, "http_status": e.status_code, "latency_s": round(time.perf_counter() - t0, 3)}]}
    except (openai.APIConnectionError, openai.APITimeoutError, httpx.HTTPError) as e:
        return {"n": n, "stop": "transport_error",
                "steps": [{"step": n, "request_id": rid, "error": type(e).__name__, "latency_s": round(time.perf_counter() - t0, 3)}]}
    finally:
        _REQUEST_HEADERS.reset(token)
    usage = msg.response_metadata.get("token_usage") or {}
    step = {"step": n, "request_id": rid, "latency_s": round(time.perf_counter() - t0, 3),
            "prompt_tokens": usage.get("prompt_tokens"), "completion_tokens": usage.get("completion_tokens"),
            "cached_tokens": (usage.get("prompt_tokens_details") or {}).get("cached_tokens"),
            "finish_reason": msg.response_metadata.get("finish_reason")}
    return {"messages": [msg], "n": n, "steps": [step],
            "last_tokens": (usage.get("prompt_tokens") or 0) + (usage.get("completion_tokens") or 0)}


def _guard(name: str, args: dict, seen: list[str], used: dict[str, int], scale: int) -> dict | None:
    key = name + json.dumps(args, sort_keys=True)
    if key in seen:
        return {"error": f"duplicate call: {name} with these exact arguments already returned a result above. "
                         "Use that result and move to the next step."}
    limit = TOOL_BUDGET.get(name, 10**6) * scale
    if used.get(name, 0) >= limit:
        return {"error": f"budget for {name} used up ({limit} calls). Work with the results you already have."}
    seen.append(key)
    used[name] = used.get(name, 0) + 1
    return None


def act_node(state: State, config) -> dict:
    c = _cfg(config)
    task, backend, harness, tools = c["task"], c["backend"], c["harness"], c["tools"]
    msg = state["messages"][-1]
    seen, used, repairs, diagnosis = list(state["seen"]), dict(state["used"]), state["repairs"], None
    out: list[AnyMessage] = []
    trace: list[str] = []
    calls: list[dict] = []
    for bad in getattr(msg, "invalid_tool_calls", None) or []:           # arguments that were not valid JSON
        out.append(ToolMessage(content=json.dumps({"error": "arguments are not valid JSON"}), tool_call_id=bad.get("id") or "", name=bad.get("name") or ""))
        trace.append(bad.get("name") or "")
        calls.append({"step": state["n"], "name": bad.get("name"), "error": "arguments are not valid JSON", "rejected": False})
    if not msg.tool_calls and not out:
        out.append(HumanMessage("Call a tool. Finish with submit_diagnosis."))
        calls.append({"step": state["n"], "name": None, "error": "no tool call", "rejected": False})
    for tc in msg.tool_calls:
        name, args = tc["name"], tc["args"]
        if name == "submit_diagnosis":
            verdict = validate(args, backend, task["namespaces"]) if harness else {"pass": True, "failed": []}
            if verdict["pass"]:
                diagnosis, result = args, {"accepted": True, "findings": len(args.get("findings", []))}
            elif repairs < MAX_REPAIRS:
                repairs += 1
                result = {"accepted": False, "errors": verdict["failed"][:8],
                          "fix": "Fix exactly these problems — copy refs exactly from tool results, name only objects that exist — "
                                 "then submit the whole diagnosis again."}
            else:
                diagnosis = inconclusive(args, verdict["failed"])            # fail closed (D-24)
                result = {"accepted": False, "final": "inconclusive"}
        elif name not in tools:
            result = {"error": f"unknown tool {name}"}
        else:
            result = (harness and _guard(name, args, seen, used, len(task["namespaces"]))) or tools[name].invoke(args)
        out.append(ToolMessage(content=json.dumps(result, ensure_ascii=False), tool_call_id=tc["id"], name=name))
        trace.append(name)
        calls.append({"step": state["n"], "name": name, "error": result.get("error") if isinstance(result, dict) else None,
                      "rejected": isinstance(result, dict) and result.get("accepted") is False})
    if diagnosis is None and harness and task.get("max_steps", 16) - state["n"] == WARN_STEPS_LEFT:
        out.append(HumanMessage(f"Only {WARN_STEPS_LEFT} model calls left. Call submit_diagnosis now with the findings "
                                "you can ground in evidence; status healthy if none."))
    update: dict[str, Any] = {"messages": out, "trace": trace, "calls": calls, "seen": seen, "used": used, "repairs": repairs}
    if diagnosis is not None:
        update["diagnosis"] = diagnosis
        update["stop"] = "inconclusive" if diagnosis.get("status") == "inconclusive" else "submitted"
    return update


def _after_agent(state: State) -> str:
    return END if state["stop"] else "act"


def _after_act(state: State, config) -> str:
    if state["stop"]:
        return END
    if state["n"] >= _cfg(config)["task"].get("max_steps", 16) or state["last_tokens"] > CONTEXT_BUDGET:
        return "limit"
    return "agent"


def _limit_node(state: State) -> dict:
    return {"stop": "context_budget" if state["last_tokens"] > CONTEXT_BUDGET else "step_cap"}


def build_graph(checkpointer=None):
    g = StateGraph(State)
    g.add_node("agent", agent_node)
    g.add_node("act", act_node)
    g.add_node("limit", _limit_node)
    g.add_edge(START, "agent")
    g.add_conditional_edges("agent", _after_agent, {"act": "act", END: END})
    g.add_conditional_edges("act", _after_act, {"agent": "agent", "limit": "limit", END: END})
    g.add_edge("limit", END)
    return g.compile(checkpointer=checkpointer)


GRAPH = build_graph()


def run_task(task: dict, backend: Backend, llm: BaseChatModel, *, cluster: str = "doctor-lab", harness: bool = True,
             graph=None) -> dict:
    run_id = f"{task['id']}-{uuid.uuid4().hex[:8]}"
    state: State = {"messages": initial_messages(task, backend, cluster), "n": 0, "trace": [], "steps": [], "calls": [],
                    "repairs": 0, "seen": [], "used": {}, "diagnosis": None, "stop": "", "last_tokens": 0}
    config = {"configurable": {"task": task, "backend": backend, "llm": llm, "harness": harness, "run_id": run_id,
                               "tools": make_tools(backend), "thread_id": run_id},
              "recursion_limit": 3 * task.get("max_steps", 16) + 10}
    final = (graph or GRAPH).invoke(state, config)
    steps = [dict(s, calls=[c for c in final["calls"] if c["step"] == s["step"]]) for s in final["steps"]]
    return {"task_id": task["id"], "run_id": run_id, "diagnosis": final["diagnosis"], "trace": final["trace"],
            "stop": final["stop"] or "step_cap", "steps": steps, "repairs": final["repairs"], "harness": harness,
            "headers": request_headers(task)}
