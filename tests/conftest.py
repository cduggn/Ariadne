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
