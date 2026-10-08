"""Convert Kubernetes resource quantities to numbers. CPU in millicores, memory in MiB. Standard library only."""
from __future__ import annotations

import re

_MEM = {"Ki": 2**10, "Mi": 2**20, "Gi": 2**30, "Ti": 2**40, "K": 10**3, "M": 10**6, "G": 10**9, "T": 10**12, "k": 10**3}
_Q = re.compile(r"^([0-9.]+(?:e[0-9]+)?)([a-zA-Z]*)$")


def cpu_m(q: str | int | float | None) -> float | None:
    """'250m' → 250, '1' → 1000, '0.5' → 500, '1500000n' → 1.5."""
    if q in (None, ""):
        return None
    m = _Q.match(str(q).strip())
    if not m:
        raise ValueError(f"bad cpu quantity {q!r}")
    v, unit = float(m.group(1)), m.group(2)
    return {"": v * 1000, "m": v, "u": v / 1000, "n": v / 1e6}[unit]


def mem_mi(q: str | int | float | None) -> float | None:
    """'512Mi' → 512, '1Gi' → 1024, '134217728' → 128, '200M' → 190.7."""
    if q in (None, ""):
        return None
    m = _Q.match(str(q).strip())
    if not m:
        raise ValueError(f"bad memory quantity {q!r}")
    v, unit = float(m.group(1)), m.group(2)
    if unit and unit not in _MEM:
        raise ValueError(f"bad memory unit {unit!r}")
    return v * _MEM.get(unit, 1) / 2**20


def pct(xs: list[float], p: float) -> float | None:
    xs = sorted(x for x in xs if x is not None)
    if not xs:
        return None
    return xs[min(len(xs) - 1, int(round(p * (len(xs) - 1))))]
