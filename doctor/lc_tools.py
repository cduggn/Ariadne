"""LangChain tool objects for the doctor's tools (D-33).

`tools.json` stays the single source of truth for what the model sees: each StructuredTool is built from
its entry (name, description, JSON-schema arguments) and executes the matching function in
`doctor/tools.py` against a Backend. The model is bound to the raw `tools.json` dicts (byte-identical,
tested), so LangChain never rewrites a schema and grammar-constrained decoding keeps every enum/pattern.

    tools = make_tools(backend)            # {name: StructuredTool}
    tools["describe"].invoke({"kind": "service", "namespace": "pricing", "name": "pricing-api"})
"""
from __future__ import annotations

import json
from pathlib import Path

from langchain_core.tools import StructuredTool

from . import tools as T
from .backends import Backend

SCHEMAS = json.loads((Path(__file__).parent / "schemas" / "tools.json").read_text())


def make_tools(backend: Backend) -> dict[str, StructuredTool]:
    """One StructuredTool per tools.json entry, bound to `backend`. `submit_diagnosis` is included for
    completeness; the graph intercepts it (validation, repairs, fail closed) before it would run."""
    out: dict[str, StructuredTool] = {}
    for entry in SCHEMAS:
        fn = entry["function"]
        name = fn["name"]

        def run(_name: str = name, **kwargs):
            return T.call(backend, _name, kwargs)          # never raises; errors come back as {"error": …}

        out[name] = StructuredTool.from_function(func=run, name=name, description=fn["description"],
                                                 args_schema=fn["parameters"], infer_schema=False)
    return out
