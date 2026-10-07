"""A scripted OpenAI-compatible server for tests (httpx MockTransport) and a real LangChain client bound to it."""
import json
from pathlib import Path

import httpx

from doctor import agent

TRAJECTORIES = json.loads((Path(__file__).resolve().parents[1] / "evals/golden/reference_trajectories.json").read_text())


class Server:
    """Replays scripted tool calls; records every request body and header set."""

    def __init__(self, *calls, status: int | None = None, refuse: int = 0, reason: str = "kv_free"):
        """status refuses every request with that status; refuse=N refuses the first N with a 503, then replays the
        script. A refusal is shaped as the gateway sends it: the reason in error.code, and a Retry-After."""
        self.calls, self.status, self.refuse, self.reason, self.requests = list(calls), status, refuse, reason, []
        self.refused = 0

    def __call__(self, req: httpx.Request) -> httpx.Response:
        self.requests.append({"body": json.loads(req.content), "headers": dict(req.headers)})
        if self.status or self.refused < self.refuse:
            self.refused += 1
            return httpx.Response(self.status or 503, headers={"Retry-After": "1"},
                                  json={"error": {"message": "shed", "type": "overloaded", "code": self.reason}})
        i = len(self.requests) - 1 - self.refused
        name, args = self.calls[min(i, len(self.calls) - 1)]
        raw = args if isinstance(args, str) else json.dumps(args)
        return httpx.Response(200, json={
            "id": f"r{i}", "object": "chat.completion", "created": 0, "model": "m",
            "choices": [{"index": 0, "finish_reason": "tool_calls", "message": {"role": "assistant", "content": None,
                         "tool_calls": [{"id": f"c{i}", "type": "function", "function": {"name": name, "arguments": raw}}]}}],
            "usage": {"prompt_tokens": 4000 + 300 * i, "completion_tokens": 60, "total_tokens": 4060 + 300 * i,
                      "prompt_tokens_details": {"cached_tokens": 3800}}})


def llm_for(server: Server):
    return agent.build_llm("http://gateway.test/v1", "Qwen/Qwen3-8B-AWQ", http_client=httpx.Client(transport=httpx.MockTransport(server)))


def reference_script(task_id: str, submit: dict) -> list:
    """The reference trajectory (the tool calls that return every ref the reference cites), then submit_diagnosis.
    Every citation in this scripted run is in the observation ledger (D-41)."""
    return [(name, args) for name, args in TRAJECTORIES[task_id]] + [("submit_diagnosis", submit)]
