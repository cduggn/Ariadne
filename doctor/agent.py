"""The investigation agent as a LangGraph state graph over LangChain tools (D-22, D-33).

    llm = build_llm(base_url, model, client=load_profile("qwen3-8b-awq")[1])   # ChatOpenAI bound to tools.json
    run = run_task(task, backend, llm)               # -> {"diagnosis", "trace", "stop", "steps", "repairs", …}

Graph (state = messages + counters + diagnosis; backend, task and model travel in the run config):

      START ──► agent ──(model error)──────────────────────────────► END   stop = http_<code> | transport_error | bad_response
                  │  (a gateway refusal, 429/503, is retried after Retry-After up to REFUSAL_RETRIES times first, D-44)
                  │
                  ▼
                 act ──(diagnosis accepted / failed closed)────────► END   stop = submitted | abstained | inconclusive
                  │ ──(step cap or context budget)─────────────────► END   stop = step_cap | context_budget
                  └──────────────► agent

  agent  one model call. Prompt layout is fixed so the serving stack can cache and route on it:
         system = triage ruleset (+ tool schemas rendered by the chat template) · user = cluster card ·
         user = task · then assistant tool calls and tool results.
  act    executes every tool call through LangChain StructuredTools, behind the harness guards (exact-repeat
         refusal, per-tool budgets × namespaces, submit-now nudge). Every result the model receives is recorded in the
         observation ledger; `submit_diagnosis` is validated against it (expect-blind, doctor/validate.py, D-41): up to
         2 repairs, then FAIL CLOSED to `inconclusive` (D-24). A model that submits `inconclusive` itself has abstained.

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
from collections.abc import Callable
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
from .validate import ledger_size, new_ledger, observe, validate

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
# Gateway refusals (D-44). A 429 or 503 carries its reason (error.code: kv_free, queue_full, tenant_tokens, …) and a
# Retry-After. The step waits max(Retry-After, 1, 2, 4, 8 s) and asks again, up to REFUSAL_RETRIES times (about 15 s at
# most), so a run rides out a load spike instead of failing. Every refusal is recorded on its step, so retries never
# hide one. DOCTOR_REFUSAL_RETRIES=0 restores failing on the first refusal.
REFUSAL_STATUSES = (429, 503)
REFUSAL_RETRIES = int(os.environ.get("DOCTOR_REFUSAL_RETRIES", "4"))
REFUSAL_WAIT_MAX_S = 8.0
WARN_STEPS_LEFT = 2
TOOL_BUDGET = {"list_problem_pods": 2, "get_events": 3, "describe": 5, "pod_logs": 5, "list_resources": 3,
               "resource_usage": 2, "inspect_certificate": 3, "rightsizing": 1, "s3_bucket_stats": 1,
               "cost_report": 2}                                                     # per namespace in the task

_REQUEST_HEADERS: contextvars.ContextVar[dict | None] = contextvars.ContextVar("doctor_request_headers", default=None)
_sleep = time.sleep                               # tests replace it, so a retried refusal does not wait in real time


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


class _ToolSafeChatOpenAI(ChatOpenAI):
    """A tool call whose arguments decode to something other than a JSON object (seen on the GPU: a JSON string
    holding a Python dict repr) would crash message construction. Turn it into an invalid tool call instead, so the
    act node tells the model and the run continues (D-39)."""

    def _create_chat_result(self, response, generation_info=None):
        data = response if isinstance(response, dict) else response.model_dump()
        for choice in data.get("choices") or []:
            for tc in (choice.get("message") or {}).get("tool_calls") or []:
                fn = tc.get("function") or {}
                try:
                    ok = isinstance(json.loads(fn.get("arguments") or "{}"), dict)
                except (TypeError, ValueError):
                    ok = True                                    # not JSON at all: LangChain already marks it invalid
                if not ok:
                    fn["arguments"] = "not a JSON object: " + str(fn.get("arguments"))[:500]
        return super()._create_chat_result(data, generation_info)


# Qwen3 non-thinking sampling as its model card recommends (greedy decoding causes endless repetition). Used when no
# model profile is given; a profile's `client` block replaces it whole, so each family keeps its own card's settings (D-40).
SAMPLING = {"temperature": 0.7, "top_p": 0.8, "top_k": 20, "min_p": 0.0}
DEFAULT_CLIENT = {**SAMPLING, "chat_template_kwargs": {"enable_thinking": False}}      # Qwen3 thinks by default
_EXTRA_SAMPLING = ("top_k", "min_p", "repetition_penalty", "presence_penalty")          # vLLM extensions, sent in the body
PROFILES = HERE.parent / "deploy" / "models"


def load_profile(ref: str) -> tuple[str, dict]:
    """(served model name, client settings) from a model profile: a path, or a name in deploy/models/ (D-40)."""
    path = Path(ref) if ref.endswith(".json") else PROFILES / f"{ref}.json"
    p = json.loads(path.read_text())
    return p["serve"]["served_name"], p["client"]


def resolve_model(model: str | None, profile: str | None) -> tuple[str, dict | None]:
    """--model and --profile: a profile supplies the served name and its sampling; --model overrides only the name
    (e.g. a gateway alias). Without a profile: --model, DOCTOR_MODEL, or the baseline, with Qwen3 sampling."""
    if profile:
        served, client = load_profile(profile)
        return model or served, client
    return model or os.environ.get("DOCTOR_MODEL") or "Qwen/Qwen3-8B-AWQ", None


def build_llm(base_url: str, model: str, *, http_client: httpx.Client | None = None, client: dict | None = None):
    """ChatOpenAI against any OpenAI-compatible endpoint (vLLM or the gateway), bound to tools.json verbatim.
    tool_choice is "auto" (D-38): "required" made vLLM constrain decoding with a grammar that collapsed into
    whitespace until max_tokens on Qwen3-8B-AWQ; a reply without a tool call is nudged by the act node instead.
    `client` is a profile's sampling block (temperature, top_p, top_k, …, chat_template_kwargs); default Qwen3.
    The API key comes only from VLLM_API_KEY. The client's own retries are off: agent_node retries a gateway
    429/503 itself, after its Retry-After, and records every refusal on the step, so none is hidden (D-44)."""
    c = DEFAULT_CLIENT if client is None else client
    extra = {k: c[k] for k in _EXTRA_SAMPLING if k in c}
    if c.get("chat_template_kwargs"):
        extra["chat_template_kwargs"] = c["chat_template_kwargs"]
    http = http_client or httpx.Client(timeout=HTTP_TIMEOUT_S)
    http.event_hooks.setdefault("request", []).append(_add_headers)
    llm = _ToolSafeChatOpenAI(model=model, base_url=base_url, api_key=os.environ.get("VLLM_API_KEY") or "EMPTY",
                              temperature=c.get("temperature"), top_p=c.get("top_p"), max_tokens=MAX_TOKENS_PER_STEP,
                              max_retries=0, http_client=http, extra_body=extra or None)
    return llm.bind_tools(TOOLS, tool_choice="auto")


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
    observed: dict                                # the observation ledger: refs and tools per namespace (D-41)


def inconclusive(rejected: dict, errors: list[str]) -> dict:
    return {"status": "inconclusive", "findings": [], "summary": "Could not produce a grounded diagnosis; escalate to a human.",
            "rejected_submission": rejected, "validation_errors": errors[:8]}


def _cfg(config) -> dict:
    return config["configurable"]


def refusal_of(e: openai.APIStatusError) -> dict:
    """A gateway refusal as the step records it: the status, the gateway's reason code, and its Retry-After."""
    reason = getattr(e, "code", None)
    if reason is None and isinstance(e.body, dict):
        reason = (e.body.get("error") or e.body).get("code")
    try:
        retry_after = float(e.response.headers.get("retry-after", ""))
    except ValueError:
        retry_after = None
    return {"http_status": e.status_code, "reason": reason, "retry_after_s": retry_after}


def refusal_wait(refusal: dict, attempt: int) -> float:
    """Seconds to wait before retry `attempt` (0-based): the gateway's Retry-After or an exponential floor, capped."""
    return min(REFUSAL_WAIT_MAX_S, max(refusal["retry_after_s"] or 0.0, float(2 ** attempt)))


def agent_node(state: State, config) -> dict:
    c = _cfg(config)
    n = state["n"] + 1
    rid = f"{c['run_id']}-s{n}"
    token = _REQUEST_HEADERS.set({**request_headers(c["task"]), "X-Request-Id": rid})
    refusals: list[dict] = []
    t0 = time.perf_counter()
    try:
        while True:
            t0 = time.perf_counter()
            try:
                msg = c["llm"].invoke(state["messages"])
                break
            except openai.APIStatusError as e:
                if e.status_code not in REFUSAL_STATUSES or len(refusals) >= REFUSAL_RETRIES:
                    if e.status_code in REFUSAL_STATUSES:
                        refusals.append(refusal_of(e))
                    return {"n": n, "stop": f"http_{e.status_code}",
                            "steps": [{"step": n, "request_id": rid, "http_status": e.status_code, "refusals": refusals,
                                       "latency_s": round(time.perf_counter() - t0, 3)}]}
                r = refusal_of(e)
                r["waited_s"] = refusal_wait(r, len(refusals))
                refusals.append(r)
                _sleep(r["waited_s"])
    except (openai.APIConnectionError, openai.APITimeoutError, httpx.HTTPError) as e:
        return {"n": n, "stop": "transport_error",
                "steps": [{"step": n, "request_id": rid, "error": type(e).__name__, "latency_s": round(time.perf_counter() - t0, 3)}]}
    except (ValueError, TypeError) as e:          # a response we cannot turn into a message: end this task, never the run
        return {"n": n, "stop": "bad_response",
                "steps": [{"step": n, "request_id": rid, "error": f"{type(e).__name__}: {str(e)[:160]}",
                           "latency_s": round(time.perf_counter() - t0, 3)}]}
    finally:
        _REQUEST_HEADERS.reset(token)
    usage = msg.response_metadata.get("token_usage") or {}
    step = {"step": n, "request_id": rid, "latency_s": round(time.perf_counter() - t0, 3),
            "prompt_tokens": usage.get("prompt_tokens"), "completion_tokens": usage.get("completion_tokens"),
            "cached_tokens": (usage.get("prompt_tokens_details") or {}).get("cached_tokens"),
            "finish_reason": msg.response_metadata.get("finish_reason"), "refusals": refusals}
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
    ledger = state["observed"]
    out: list[AnyMessage] = []
    trace: list[str] = []
    calls: list[dict] = []
    for bad in getattr(msg, "invalid_tool_calls", None) or []:           # arguments that were not valid JSON
        out.append(ToolMessage(content=json.dumps({"error": "arguments are not valid JSON: send exactly one JSON object"}),
                               tool_call_id=bad.get("id") or "", name=bad.get("name") or ""))
        trace.append(bad.get("name") or "")
        calls.append({"step": state["n"], "name": bad.get("name"), "error": "arguments are not valid JSON", "rejected": False})
    if not msg.tool_calls and not out:
        out.append(HumanMessage("Call a tool. Finish with submit_diagnosis."))
        calls.append({"step": state["n"], "name": None, "error": "no tool call", "rejected": False})
    for tc in msg.tool_calls:
        name, args = tc["name"], tc["args"]
        if name == "submit_diagnosis":
            verdict = validate(args, backend, task["namespaces"], observed=ledger) if harness else {"pass": True, "failed": []}
            if verdict["pass"]:
                diagnosis, result = args, {"accepted": True, "findings": len(args.get("findings", []))}
            elif repairs < MAX_REPAIRS:
                repairs += 1
                result = {"accepted": False, "errors": verdict["failed"][:8],
                          "fix": "Fix exactly these problems — cite only refs that tool results in this conversation returned, name "
                                 "only objects that exist — then submit the whole diagnosis again."}
            else:
                diagnosis = inconclusive(args, verdict["failed"])            # fail closed (D-24)
                result = {"accepted": False, "final": "inconclusive"}
        elif name not in tools:
            result = {"error": f"unknown tool {name}"}
        else:
            result = (harness and _guard(name, args, seen, used, len(task["namespaces"]))) or tools[name].invoke(args)
            ledger = observe(ledger, name, result)
        out.append(ToolMessage(content=json.dumps(result, ensure_ascii=False), tool_call_id=tc["id"], name=name))
        trace.append(name)
        rec = {"step": state["n"], "name": name, "error": result.get("error") if isinstance(result, dict) else None,
               "rejected": isinstance(result, dict) and result.get("accepted") is False}
        if name != "submit_diagnosis":
            rec["args"] = args                                               # small; the diagnosis is kept separately
        elif rec["rejected"]:
            rec["validation_errors"] = verdict["failed"][:8]
        calls.append(rec)
    if diagnosis is None and harness and task.get("max_steps", 16) - state["n"] == WARN_STEPS_LEFT:
        out.append(HumanMessage(f"Only {WARN_STEPS_LEFT} model calls left. Call submit_diagnosis now with the findings "
                                "you can ground in evidence. If you cannot ground any, submit status inconclusive and say in "
                                "summary what you could not check; healthy only if you checked and found nothing wrong."))
    update: dict[str, Any] = {"messages": out, "trace": trace, "calls": calls, "seen": seen, "used": used, "repairs": repairs,
                              "observed": ledger}
    if diagnosis is not None:
        update["diagnosis"] = diagnosis
        update["stop"] = ("submitted" if diagnosis.get("status") != "inconclusive" else
                          "inconclusive" if "rejected_submission" in diagnosis else "abstained")
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
             graph=None, on_update: Callable[[str, dict], None] | None = None) -> dict:
    """Run one task to a stop. `on_update(node, update)` sees every node's state update as it happens
    (the CLI prints progress from it); the returned record is the same with or without it."""
    run_id = f"{task['id']}-{uuid.uuid4().hex[:8]}"
    state: State = {"messages": initial_messages(task, backend, cluster), "n": 0, "trace": [], "steps": [], "calls": [],
                    "repairs": 0, "seen": [], "used": {}, "diagnosis": None, "stop": "", "last_tokens": 0, "observed": new_ledger()}
    config = {"configurable": {"task": task, "backend": backend, "llm": llm, "harness": harness, "run_id": run_id,
                               "tools": make_tools(backend), "thread_id": run_id},
              "recursion_limit": 3 * task.get("max_steps", 16) + 10}
    final: dict = {}
    for mode, chunk in (graph or GRAPH).stream(state, config, stream_mode=["updates", "values"]):
        if mode == "values":
            final = chunk
        elif on_update:
            for node, update in chunk.items():
                on_update(node, update or {})
    steps = [dict(s, calls=[c for c in final["calls"] if c["step"] == s["step"]]) for s in final["steps"]]
    return {"task_id": task["id"], "run_id": run_id, "diagnosis": final["diagnosis"], "trace": final["trace"],
            "stop": final["stop"] or "step_cap", "steps": steps, "repairs": final["repairs"], "harness": harness,
            "headers": request_headers(task), "observed": final["observed"], "observed_refs": ledger_size(final["observed"])}
