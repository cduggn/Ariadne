"""A scripted OpenAI-compatible server for tests (httpx MockTransport) and a real LangChain client bound to it."""
import json

import httpx

from doctor import agent


class Server:
    """Replays scripted tool calls; records every request body and header set."""

    def __init__(self, *calls, status: int | None = None):
        self.calls, self.status, self.requests = list(calls), status, []

    def __call__(self, req: httpx.Request) -> httpx.Response:
        self.requests.append({"body": json.loads(req.content), "headers": dict(req.headers)})
        if self.status:
            return httpx.Response(self.status, json={"error": {"message": "shed", "type": "overloaded"}})
        i = len(self.requests) - 1
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
