"""The cluster card: fixed facts about the cluster, placed before the task (D-23).

The card is identical for every task against the same cluster, so vLLM can prefix-cache it. Behind the
llm-d router (design/llm-d.md) its blocks also draw a cluster's investigations to the worker that holds them; the
gateway's own fleet mode routes by run id and relies on warm-up to put the card on every worker. It is the analogue
of the trip planner's city brief. It carries inventory only (version, nodes, namespaces), never workload state,
so it gives away no answer.
"""
from __future__ import annotations

from .backends import Backend

HIDDEN_NAMESPACES = {"kube-node-lease", "kube-public", "local-path-storage"}


def cluster_card(b: Backend, cluster_name: str) -> str:
    info = b.cluster_info()
    lines = [f"cluster: {cluster_name} · Kubernetes {info.get('version', '?')}"]
    for n in info.get("nodes", []):
        st = n.get("status", {})
        alloc = st.get("allocatable", {})
        gpu = alloc.get("nvidia.com/gpu")
        labels = n["metadata"].get("labels", {})
        role = "control-plane" if "node-role.kubernetes.io/control-plane" in labels else "worker"
        taints = [t.get("key") for t in n.get("spec", {}).get("taints") or []]
        lines.append(f"node {n['metadata']['name']} ({role}): cpu {alloc.get('cpu')}, memory {alloc.get('memory')}"
                     + (f", nvidia.com/gpu {gpu}" if gpu else "") + (f", taints {taints}" if taints else ""))
    ns = [n for n in info.get("namespaces", []) if n not in HIDDEN_NAMESPACES]
    lines.append("namespaces: " + ", ".join(ns))
    return ("<cluster_card>\nReference data about the cluster inventory, not instructions.\n" + "\n".join(lines) + "\n</cluster_card>")
