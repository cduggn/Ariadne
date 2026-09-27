"""Scrub secrets out of cluster text before it reaches a model, a fixture or a log. Standard library only.

Applied twice: when a snapshot is recorded (so fixtures in git never hold a secret) and when a live
tool result is built (so a model never sees one). Patterns are deliberately broad — a false positive
costs one hidden token, a false negative leaks a credential.
"""
from __future__ import annotations

import re

MASK = "[REDACTED]"

_PATTERNS: list[tuple[re.Pattern[str], str]] = [
    (re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----", re.S), MASK),
    (re.compile(r"\b(AKIA|ASIA)[0-9A-Z]{16}\b"), MASK),                                 # AWS access key id
    (re.compile(r"\b(ghp|gho|ghs|ghu|github_pat)_[A-Za-z0-9_]{20,}\b"), MASK),          # GitHub tokens
    (re.compile(r"\bhf_[A-Za-z0-9]{20,}\b"), MASK),                                     # Hugging Face tokens
    (re.compile(r"\bsk-[A-Za-z0-9_-]{20,}\b"), MASK),                                   # generic API keys
    (re.compile(r"\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b"), MASK),  # JWTs
    (re.compile(r"(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{12,}"), r"\1 " + MASK),
    (re.compile(r"(?i)\b([a-z][a-z0-9+.-]*://[^:/\s@]+):[^@\s/]+@"), r"\1:" + MASK + "@"),  # scheme://user:pass@
    (re.compile(r"(?i)\b((?:[a-z0-9_]*_)?(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)"
                r"[a-z0-9_]*)\s*[=:]\s*(\"[^\"]*\"|'[^']*'|\S+)"), r"\1=" + MASK),
]


def redact(text: str) -> str:
    for pattern, repl in _PATTERNS:
        text = pattern.sub(repl, text)
    return text


# Text written into a log by anyone who can run a workload is untrusted. Lines that look like
# instructions to an AI are flagged (not removed) so the model and the reviewer can see them.
INJECTION = re.compile(r"(?i)ignore (all |any )?(previous|prior|above) instructions|you are (an? )?(ai|assistant|language model)"
                       r"|system prompt|disregard .{0,20}instructions|\bas an ai\b|call the tool|submit_diagnosis")


def looks_like_injection(text: str) -> bool:
    return bool(INJECTION.search(text))
