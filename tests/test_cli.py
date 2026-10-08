"""The command line end to end: recorded snapshots and a fake read-only kubectl, a scripted model server."""
import io
import json
import os
import stat
import sys

from agent import agent, cli
from evals.build_golden import backend_for
from tests.mockllm import Server, llm_for, reference_script


def run(argv, server):
    out, err = io.StringIO(), io.StringIO()
    code = cli.main(argv, llm=llm_for(server), stdout=out, stderr=err)
    return code, out.getvalue(), err.getvalue()


def test_snapshot_investigation_reports_the_root_cause_and_shows_each_step(refs):
    pod = backend_for(["crashloop"]).objects("pods", "orders")[0]["metadata"]["name"]
    s = Server(("list_problem_pods", {"namespace": "orders"}),
               ("pod_logs", {"namespace": "orders", "pod": pod, "container": "", "previous": False, "tail": 40}),
               ("submit_diagnosis", refs["dx-crashloop"]))
    code, out, err = run(["investigate", "-n", "orders", "--snapshot", "crashloop", "orders-api keeps restarting"], s)
    assert code == cli.EXIT_ISSUE
    assert "ISSUE — 1 root cause" in out and "crashloop_app_error · Deployment orders/orders-api · confidence high" in out
    assert "lg-orders-api-" in out and "never changes the cluster" in out and "prompt 12,900 tokens (88% cached)" in out
    assert "s1  model" in err and "list_problem_pods(namespace=orders)" in err and f"pod={pod}" in err
    assert "submit_diagnosis → accepted" in err
    body, headers = s.requests[0]["body"], s.requests[0]["headers"]
    assert headers["x-priority"] == "interactive" and headers["x-request-id"].startswith("cli-investigate-")
    assert "orders-api keeps restarting" in body["messages"][2]["content"]
    golden = Server(("submit_diagnosis", refs["dx-crashloop"]))           # same card as a golden run, so the same cached prefix
    agent.run_task({"id": "g", "task_type": "investigate", "namespaces": ["orders"], "report": "x"}, backend_for(["crashloop"]), llm_for(golden))
    assert body["messages"][:2] == golden.requests[0]["body"]["messages"][:2]


def test_json_and_out_file_healthy_exit_zero(refs, tmp_path):
    s = Server(*reference_script("dx-healthy", refs["dx-healthy"]))       # list_problem_pods first: healthy needs coverage
    path = tmp_path / "run.json"
    code, out, err = run(["audit", "-n", "status", "--snapshot", "healthy", "--json", "--out", str(path), "-q"], s)
    record = json.loads(out)
    assert code == cli.EXIT_HEALTHY and err == "" and record["diagnosis"]["status"] == "healthy"
    assert json.loads(path.read_text()) == record and record["mode"] == "audit" and record["source"] == "snapshot healthy"
    assert s.requests[0]["headers"]["x-priority"] == "batch"


def test_refusal_and_fail_closed_exit_two(refs):
    code, out, _ = run(["investigate", "-n", "orders", "--snapshot", "crashloop"], Server(status=503))
    assert code == cli.EXIT_NO_DIAGNOSIS and "NO GROUNDED DIAGNOSIS" in out and "refused by the gateway" in out
    bad = json.loads(json.dumps(refs["dx-crashloop"]))
    bad["findings"][0]["evidence"] = ["lg-invented-x-c1"]
    code, out, err = run(["investigate", "-n", "orders", "--snapshot", "crashloop"], Server(("submit_diagnosis", bad)))
    assert code == cli.EXIT_NO_DIAGNOSIS and "escalate to a human" in out and "evidence-exists" in out
    assert err.count("submit_diagnosis → rejected") == 3


def test_usage_errors_never_reach_the_model():
    s = Server(("submit_diagnosis", {}))
    for argv in (["investigate", "-n", "Bad_NS", "--snapshot", "crashloop"],
                 ["investigate", "-n", "billing", "--snapshot", "crashloop"],            # not in the recording
                 ["investigate", "-n", "orders", "--snapshot", "../cluster"],
                 ["investigate", "-n", "orders", "--snapshot", "crashloop", "--tenant", "a\nb"],
                 ["investigate", "--snapshot", "crashloop"],
                 ["fix", "-n", "orders"]):
        assert run(argv, s)[0] == cli.EXIT_USAGE, argv
    assert s.requests == []


FAKE_KUBECTL = """#!{python}
import json, sys
open({log!r}, "a").write(json.dumps(sys.argv[1:]) + "\\n")
args = sys.argv[1:]
if args[:1] == ["--context"]:
    if args[1] != "lab":
        sys.exit("error: context was not found for specified context: " + args[1])
    args = args[2:]
if args[:2] == ["get", "namespaces"]:
    print(json.dumps({{"items": [{{"metadata": {{"name": "orders"}}}}]}}))
elif args[:1] == ["version"]:
    print(json.dumps({{"serverVersion": {{"gitVersion": "v1.33.1"}}}}))
else:
    print(json.dumps({{"items": []}}))
"""


def test_live_cluster_through_read_only_kubectl(refs, tmp_path, monkeypatch):
    log = tmp_path / "kubectl.log"
    fake = tmp_path / "kubectl"
    fake.write_text(FAKE_KUBECTL.format(python=sys.executable, log=str(log)))
    fake.chmod(fake.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv("DOCTOR_KUBECTL", str(fake))
    monkeypatch.delenv("KUBECONFIG", raising=False)
    s = Server(("list_problem_pods", {"namespace": "orders"}), ("submit_diagnosis", refs["dx-healthy"]))
    code, out, err = run(["investigate", "-n", "orders", "--context", "lab", "--kubeconfig", str(tmp_path / "kc")], s)
    assert code == cli.EXIT_HEALTHY and "HEALTHY" in out and "context lab" in err
    assert os.environ["KUBECONFIG"] == str(tmp_path / "kc")
    assert "cluster: lab · Kubernetes v1.33.1" in s.requests[0]["body"]["messages"][1]["content"]
    calls = [json.loads(line) for line in log.read_text().splitlines()]
    assert calls and all(c[:2] == ["--context", "lab"] and c[2] in ("get", "logs", "top", "version") for c in calls)
    assert run(["investigate", "-n", "payments", "--context", "lab"], s)[0] == cli.EXIT_USAGE      # typo must not read as healthy
    code, _, err = run(["investigate", "-n", "orders", "--context", "other"], s)
    assert code == cli.EXIT_NO_DIAGNOSIS and "cannot read the cluster" in err and "make kubeconfig" in err
