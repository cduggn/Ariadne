"""Where cluster data comes from. The model never sees this layer (D-21).

    Backend            raw Kubernetes objects, logs, usage, metrics and cost data
      ├── SnapshotBackend   a recorded dump (fixtures/snapshots/*.json) — tests, golden set, offline
      └── KubectlBackend    a live cluster through `kubectl -o json` (kind, k3s, EKS alike) plus
                            optional Prometheus / OpenCost / AWS readers

Tools (doctor/tools.py) turn raw data into small, referenced, redacted results. Because both backends
return the same raw shapes, one set of tool code serves both — a fixture exercises exactly the code
path a live cluster does.

Read-only by construction: the only kubectl verbs are `get`, `logs`, `top` and `version`; Secrets are
never requested; ConfigMap data is dropped except values that are public X.509 certificates (D-31).
"""
from __future__ import annotations

import datetime as dt
import json
import os
import re
import shutil
import subprocess
import time
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Protocol

KINDS = ("pods", "deployments", "replicasets", "services", "endpointslices", "jobs", "configmaps", "events", "nodes",
         "limitranges", "resourcequotas", "networkpolicies", "persistentvolumeclaims", "ingresses")
NAME_RE = re.compile(r"^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$")          # RFC 1123 subdomain
LOG_LINES_RECORDED = 200


class Unavailable(Exception):
    """The data source does not exist for this backend (e.g. Prometheus presets on kind)."""


class Backend(Protocol):
    name: str

    def objects(self, kind: str, namespace: str) -> list[dict]: ...
    def logs(self, namespace: str, pod: str, container: str, previous: bool) -> str: ...
    def usage(self, namespace: str) -> list[dict]: ...
    def usage_series(self, namespace: str) -> list[dict]: ...
    def namespaces(self) -> list[str]: ...
    def cluster_info(self) -> dict: ...
    def now(self) -> dt.datetime: ...
    def metric(self, preset: str, namespace: str) -> dict: ...
    def s3_bucket_stats(self, bucket: str) -> dict: ...
    def cost(self, query: str) -> dict: ...


def check_name(value: str, what: str = "name") -> str:
    if not isinstance(value, str) or not NAME_RE.match(value):
        raise ValueError(f"invalid {what}: {value!r}")
    return value


def is_public_cert(value: str) -> bool:
    return "-----BEGIN CERTIFICATE-----" in value and "PRIVATE KEY" not in value


def strip(obj: dict) -> dict:
    """Drop noisy or sensitive fields before a raw object is stored or used."""
    md = obj.get("metadata", {})
    for k in ("managedFields", "resourceVersion", "selfLink"):
        md.pop(k, None)
    ann = md.get("annotations") or {}
    for k in list(ann):
        if k.startswith("kubectl.kubernetes.io/last-applied") or k.startswith("deployment.kubernetes.io/"):
            ann.pop(k)
    if obj.get("kind") == "ConfigMap":
        data = obj.pop("data", None) or {}
        obj.pop("binaryData", None)
        obj["dataKeys"] = sorted(data)
        certs = {k: v for k, v in data.items() if isinstance(v, str) and is_public_cert(v)}
        if certs:
            obj["publicCertificates"] = certs
    return obj


# ---- snapshot ------------------------------------------------------------------------------

class SnapshotBackend:
    """Serve a recorded dump: {"cluster": {...}, "namespaces": {ns: {kind: [objects]}},
    "logs": {"ns/pod/container/current|previous": text}, "usage": {ns: [...]}, "usage_series": {ns: [...]},
    "metrics": {...}, "aws": {...}, "recorded": {"time": ...}}."""

    name = "snapshot"

    def __init__(self, dump: dict):
        self.dump = dump

    @classmethod
    def load(cls, *paths: Path) -> SnapshotBackend:
        merged: dict = {"cluster": {}, "namespaces": {}, "logs": {}, "usage": {}, "usage_series": {}, "metrics": {}, "aws": {}, "recorded": {}}
        for p in paths:
            d = json.loads(Path(p).read_text())
            merged["cluster"] = d.get("cluster") or merged["cluster"]
            for key in ("namespaces", "logs", "usage", "usage_series", "metrics", "aws"):
                merged[key].update(d.get(key, {}))
            if d.get("recorded", {}).get("time", "") > merged["recorded"].get("time", ""):
                merged["recorded"] = d["recorded"]
        return cls(merged)

    def objects(self, kind: str, namespace: str) -> list[dict]:
        if kind == "nodes":
            return self.dump["cluster"].get("nodes", [])
        return self.dump["namespaces"].get(namespace, {}).get(kind, [])

    def logs(self, namespace: str, pod: str, container: str, previous: bool) -> str:
        key = f"{namespace}/{pod}/{container}/{'previous' if previous else 'current'}"
        if key not in self.dump["logs"]:
            raise LookupError(f"no {'previous ' if previous else ''}logs for {namespace}/{pod}/{container}")
        return self.dump["logs"][key]

    def usage(self, namespace: str) -> list[dict]:
        return self.dump["usage"].get(namespace, [])

    def usage_series(self, namespace: str) -> list[dict]:
        return self.dump["usage_series"].get(namespace) or [{"t": self.dump["recorded"].get("time"), "rows": self.usage(namespace)}]

    def namespaces(self) -> list[str]:
        return sorted(self.dump["cluster"].get("namespaces", []))

    def cluster_info(self) -> dict:
        return self.dump["cluster"]

    def now(self) -> dt.datetime:
        """Recorded time, so certificate expiry and ages are judged as they were when the snapshot was taken."""
        t = self.dump["recorded"].get("time")
        return dt.datetime.strptime(t, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=dt.UTC) if t else dt.datetime.now(dt.UTC)

    def metric(self, preset: str, namespace: str) -> dict:
        v = self.dump["metrics"].get(f"{preset}/{namespace}")
        if v is None:
            raise Unavailable(f"metric preset {preset} was not recorded for {namespace}")
        return v

    def s3_bucket_stats(self, bucket: str) -> dict:
        v = self.dump["aws"].get(f"s3/{bucket}")
        if v is None:
            raise Unavailable("S3 data was not recorded in this snapshot")
        return v

    def cost(self, query: str) -> dict:
        v = self.dump["aws"].get(f"cost/{query}")
        if v is None:
            raise Unavailable(f"cost query {query} was not recorded in this snapshot")
        return v


# ---- live ----------------------------------------------------------------------------------

class KubectlBackend:
    """Read a live cluster through kubectl. Optional sources are configured by environment:
    DOCTOR_PROMETHEUS_URL, DOCTOR_OPENCOST_URL, DOCTOR_S3_BUCKET (+ the AWS CLI), DOCTOR_CCEXPLORER."""

    name = "live"
    TIMEOUT_S = 20

    def __init__(self, context: str | None = None, kubectl: str | None = None):
        self.context = context
        self.kubectl = kubectl or os.environ.get("DOCTOR_KUBECTL") or shutil.which("kubectl") or "kubectl"

    def _run(self, *args: str) -> str:
        if args[0] not in ("get", "logs", "top", "version"):
            raise PermissionError(f"verb {args[0]!r} is not allowed")      # read-only by construction
        cmd = [self.kubectl, *(["--context", self.context] if self.context else []), *args]
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=self.TIMEOUT_S, check=False)
        if r.returncode != 0:
            raise LookupError(r.stderr.strip().splitlines()[-1] if r.stderr.strip() else f"kubectl exited {r.returncode}")
        return r.stdout

    def objects(self, kind: str, namespace: str) -> list[dict]:
        if kind not in KINDS:
            raise ValueError(f"kind {kind!r} not readable")
        args = ["get", kind, "-o", "json"] + ([] if kind == "nodes" else ["-n", check_name(namespace, "namespace")])
        return [strip(o) for o in json.loads(self._run(*args)).get("items", [])]

    def logs(self, namespace: str, pod: str, container: str, previous: bool) -> str:
        args = ["logs", check_name(pod, "pod"), "-n", check_name(namespace, "namespace"), f"--tail={LOG_LINES_RECORDED}"]
        if container:
            args += ["-c", check_name(container, "container")]
        if previous:
            args.append("--previous")
        out = self._run(*args)
        if out.startswith("unable to retrieve container logs"):      # kubectl exits 0 with this when the
            raise LookupError(out.strip()[:120])                      # container was already garbage-collected
        return out

    def usage(self, namespace: str) -> list[dict]:
        out = self._run("top", "pods", "-n", check_name(namespace, "namespace"), "--containers", "--no-headers")
        rows = []
        for line in out.splitlines():
            parts = line.split()
            if len(parts) >= 4:
                rows.append({"pod": parts[0], "container": parts[1], "cpu": parts[2], "memory": parts[3]})
        return rows

    SERIES = {
        "cpu": 'sum by (pod, container) (rate(container_cpu_usage_seconds_total{namespace="%s",container!="",container!="POD"}[5m]))',
        "memory": 'max by (pod, container) (container_memory_working_set_bytes{namespace="%s",container!="",container!="POD"})',
    }

    def usage_series(self, namespace: str) -> list[dict]:
        """24 h of per-container usage from Prometheus when configured; otherwise one metrics-server sample."""
        url = os.environ.get("DOCTOR_PROMETHEUS_URL")
        ns = check_name(namespace, "namespace")
        if not url:
            return [{"t": dt.datetime.now(dt.UTC).strftime("%Y-%m-%dT%H:%M:%SZ"), "rows": self.usage(ns)}]
        end = time.time()
        points: dict[float, dict[tuple[str, str], dict]] = {}
        for res, q in self.SERIES.items():
            qs = urllib.parse.urlencode({"query": q % ns, "start": end - 86400, "end": end, "step": "600"})
            with urllib.request.urlopen(f"{url.rstrip('/')}/api/v1/query_range?{qs}", timeout=20) as r:
                for s in json.load(r)["data"]["result"]:
                    key = (s["metric"].get("pod", ""), s["metric"].get("container", ""))
                    for t, v in s["values"]:
                        row = points.setdefault(t, {}).setdefault(key, {"pod": key[0], "container": key[1]})
                        row[res] = f"{round(float(v) * 1000)}m" if res == "cpu" else f"{round(float(v) / 2**20)}Mi"
        return [{"t": dt.datetime.fromtimestamp(t, dt.UTC).strftime("%Y-%m-%dT%H:%M:%SZ"), "rows": list(rows.values())}
                for t, rows in sorted(points.items())]

    def namespaces(self) -> list[str]:
        return sorted(o["metadata"]["name"] for o in json.loads(self._run("get", "namespaces", "-o", "json"))["items"])

    def cluster_info(self) -> dict:
        v = json.loads(self._run("version", "-o", "json")).get("serverVersion", {}).get("gitVersion", "")
        return {"version": v, "nodes": self.objects("nodes", ""), "namespaces": self.namespaces()}

    def now(self) -> dt.datetime:
        return dt.datetime.now(dt.UTC)

    # Prometheus presets: fixed queries, never model-written PromQL.
    PRESETS = {
        "vllm_kv": 'max by (pod) (vllm:kv_cache_usage_perc{namespace="%s"})',
        "vllm_queue": 'sum by (pod) (vllm:num_requests_waiting{namespace="%s"})',
        "vllm_preemptions": 'sum by (pod) (rate(vllm:num_preemptions_total{namespace="%s"}[5m]))',
        "gpu_util": 'avg(DCGM_FI_DEV_GPU_UTIL)',
        "gpu_memory": 'sum(DCGM_FI_DEV_FB_USED) / sum(DCGM_FI_DEV_FB_USED + DCGM_FI_DEV_FB_FREE)',
    }

    def metric(self, preset: str, namespace: str) -> dict:
        url = os.environ.get("DOCTOR_PROMETHEUS_URL")
        if not url:
            raise Unavailable("no Prometheus configured (DOCTOR_PROMETHEUS_URL)")
        q = self.PRESETS[preset] % check_name(namespace, "namespace") if "%s" in self.PRESETS[preset] else self.PRESETS[preset]
        with urllib.request.urlopen(f"{url.rstrip('/')}/api/v1/query?{urllib.parse.urlencode({'query': q})}", timeout=10) as r:
            data = json.load(r)["data"]["result"]
        return {"preset": preset, "series": [{"labels": d["metric"], "value": d["value"][1]} for d in data][:20]}

    def s3_bucket_stats(self, bucket: str) -> dict:
        if bucket != os.environ.get("DOCTOR_S3_BUCKET"):
            raise PermissionError("only the configured lab bucket may be inspected (DOCTOR_S3_BUCKET)")
        r = subprocess.run(["aws", "s3api", "list-objects-v2", "--bucket", check_name(bucket, "bucket"), "--max-items", "100000",
                            "--query", "{count: length(Contents || `[]`), bytes: sum(Contents[].Size || `[0]`)}", "--output", "json"],
                           capture_output=True, text=True, timeout=60, check=False)
        if r.returncode != 0:
            raise LookupError(r.stderr.strip()[:200])
        return {"bucket": bucket, **json.loads(r.stdout)}

    COST_QUERIES = {   # flags checked against cduggn/ccExplorer cmd/cli/get_command.go
        "aws_by_service_7d": ["get", "aws", "-g", "DIMENSION=SERVICE", "-s", "{start7}", "-e", "{today}"],
        "aws_anomalies_30d": ["get", "aws", "anomalies", "-s", "{start30}", "-e", "{today}"],
    }

    def cost(self, query: str) -> dict:
        if query == "cluster_by_namespace_24h":
            url = os.environ.get("DOCTOR_OPENCOST_URL")
            if not url:
                raise Unavailable("no OpenCost configured (DOCTOR_OPENCOST_URL)")
            with urllib.request.urlopen(f"{url.rstrip('/')}/allocation/compute?window=24h&aggregate=namespace", timeout=15) as r:
                return {"query": query, "source": "opencost", "data": json.load(r).get("data", [])}
        exe = os.environ.get("DOCTOR_CCEXPLORER") or shutil.which("ccexplorer")
        if not exe or query not in self.COST_QUERIES:
            raise Unavailable("ccexplorer not installed or query unknown")
        today = dt.date.today()
        fill = {"today": today.isoformat(), "start7": (today - dt.timedelta(days=7)).isoformat(),
                "start30": (today - dt.timedelta(days=30)).isoformat()}
        args = [a.format(**fill) for a in self.COST_QUERIES[query]]
        r = subprocess.run([exe, *args], capture_output=True, text=True, timeout=60, check=False)
        if r.returncode != 0:
            raise LookupError(r.stderr.strip()[:200])
        return {"query": query, "source": "ccexplorer", "text": r.stdout[:6000]}
