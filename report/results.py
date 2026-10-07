"""Loaders for the report notebook (D-46). Every number in the notebook comes from a committed file in metrics/ through
these functions, so the notebook can be rebuilt from the repo alone (`make report`) and no figure is typed by hand.

    summary("gw-38-20261006-131640")          golden summary → dict
    rows("gw-38-20261006-131640")             golden rows (one per run) → list[dict]
    prom("gateway-sweep-c32-20261007-151105")  a saved /metrics scrape → {(name, labels): value}
    timeseries("run1")                        the newest Prometheus export (lab/export.py) → dict, or None
"""
from __future__ import annotations

import json
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
METRICS = ROOT / "metrics"
FIGURES = ROOT / "design" / "figures"

_SAMPLE = re.compile(r"^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+(\S+)$")
_LABEL = re.compile(r'(\w+)="((?:[^"\\]|\\.)*)"')


def summary(run: str) -> dict:
    """The golden summary for a run id: the part of the file name after `golden-`."""
    return json.loads((METRICS / f"golden-{run}.summary.json").read_text())


def rows(run: str) -> list[dict]:
    return [json.loads(line) for line in (METRICS / f"golden-{run}.jsonl").read_text().splitlines() if line.strip()]


def prom(scrape: str) -> dict[tuple[str, tuple[tuple[str, str], ...]], float]:
    """A Prometheus text scrape as {(metric, sorted label pairs): value}. Comments and malformed lines are skipped."""
    out = {}
    for line in (METRICS / f"{scrape}.prom").read_text().splitlines():
        m = _SAMPLE.match(line)
        if not m:
            continue
        labels = tuple(sorted(_LABEL.findall(m.group(2) or "")))
        try:
            out[(m.group(1), labels)] = float(m.group(3))
        except ValueError:
            continue
    return out


def total(samples: dict, metric: str, **match: str) -> float:
    """Sum of a metric's samples whose labels include every key=value in match."""
    return sum(v for (name, labels), v in samples.items()
               if name == metric and all((k, v_) in labels for k, v_ in match.items()))


def by_label(samples: dict, metric: str, label: str, **match: str) -> dict[str, float]:
    """A metric summed by one label, for samples matching the others."""
    out: dict[str, float] = {}
    for (name, labels), v in samples.items():
        d = dict(labels)
        if name == metric and label in d and all(d.get(k) == v_ for k, v_ in match.items()):
            out[d[label]] = out.get(d[label], 0.0) + v
    return out


def histogram(samples: dict, metric: str, **match: str) -> list[tuple[float, float]]:
    """A cumulative Prometheus histogram as (upper bound, count in that bucket) pairs, +Inf last."""
    cum = {}
    for (name, labels), v in samples.items():
        d = dict(labels)
        if name == f"{metric}_bucket" and all(d.get(k) == v_ for k, v_ in match.items()):
            le = float("inf") if d["le"] == "+Inf" else float(d["le"])
            cum[le] = cum.get(le, 0.0) + v
    edges = sorted(cum)
    return [(le, cum[le] - (cum[edges[i - 1]] if i else 0.0)) for i, le in enumerate(edges)]


def kv_measured(model: str, topology: str) -> int | None:
    """vLLM's own KV pool for a pair, from the newest metrics/kv-<model>-<topology>-*.log."""
    for f in sorted(METRICS.glob(f"kv-{model}-{topology}-*.log"), reverse=True):
        m = re.search(r"GPU KV cache size:\s*([\d,]+)\s*tokens", f.read_text())
        if m:
            return int(m.group(1).replace(",", ""))
    return None


def timeseries(tag: str | None = None) -> dict | None:
    """The newest metrics/ts-<tag>-<stamp>.json (any tag when None), parsed; None when there is none."""
    name = re.compile(rf"ts-{re.escape(tag) if tag else '.+'}-(\d{{8}}-\d{{6}})\.json")
    found = [(m.group(1), f) for f in METRICS.glob("ts-*.json") if (m := name.fullmatch(f.name))]
    return json.loads(max(found)[1].read_text()) if found else None


def series(ts: dict, name: str) -> list[tuple[dict, list[float], list[float]]]:
    """One query's results as (labels, seconds since the window's start, values); empty when missing or errored."""
    if name in ts.get("errors", {}):
        return []
    return [(r["labels"], [t - ts["start"] for t, _ in r["values"]], [v for _, v in r["values"]])
            for r in ts["series"].get(name, {}).get("results", [])]
