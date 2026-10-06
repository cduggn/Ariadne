import json
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]


@pytest.fixture(scope="session")
def tasks():
    return {t["id"]: t for t in (json.loads(line) for line in (ROOT / "evals/golden/tasks.jsonl").read_text().splitlines() if line.strip())}


@pytest.fixture(scope="session")
def refs():
    return json.loads((ROOT / "evals/golden/reference_diagnoses.json").read_text())


@pytest.fixture(autouse=True)
def no_refusal_waits(monkeypatch):
    """A retried gateway refusal (D-44) waits for real in production; tests record the waits instead."""
    from doctor import agent
    waits = []
    monkeypatch.setattr(agent, "_sleep", waits.append)
    return waits
