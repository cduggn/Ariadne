"""Condensed live view of the gateway log: one aligned row per request.

    lam ssh cluster-doctor -- kubectl logs deploy/gateway -f | python3 lab/tail.py
"""
import json
import sys

print(f"{'time':8} {'run':16} {'st':>3} {'pod':6} {'sticky':11} {'code':4} {'reason':12} {'queue':>6} {'prompt':>6}")
for line in sys.stdin:
    try:
        r = json.loads(line)
    except ValueError:
        continue
    if r.get("msg") != "request":
        continue
    print(f"{r['time'][11:19]:8} {r['run'][:16]:16} {r['step']:>3} {r['pod'] or '-':6} {r['sticky'] or '-':11} "
          f"{r['status']:>4} {(r.get('reason') or '-')[:12]:12} {r['queue_ms']/1000:>5.1f}s {r['prompt_tokens']:>6}", flush=True)
