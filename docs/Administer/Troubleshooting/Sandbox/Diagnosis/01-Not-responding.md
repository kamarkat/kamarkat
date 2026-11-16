# Brief: why the sandbox isn't responding to the client (diagnosis only)

Goal: find, with evidence, why an oranger chat session's sandbox doesn't respond to the browser. Diagnose only, and change nothing.

Context:
- The last release changed the request path:
  - harness 0.1.19, with ADR-015's job tracking and reporting middleware, and wake-up runs;
  - waypoint SDK 0.1.4, with migration 0014 applied by an init container, the share routes, and run-keyed media placement;
  - ui-session 0.1.4 and oranger 1.0.3.
- A sandbox that seems stuck is often just a cold image pull, about 2.5 minutes on nutgraf-01. Rule that out first.

Access

KUBECONFIG=${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/nutgraf-01.kubeconfig. Use the hub kubeconfig only if the evidence points to the hub. Never print, copy or log anything from these files, or any Secret.

Which session

Ask the lead for the session id and the time the user sent the message. If you don't have them, take the newest ej-sandbox-oranger-* pod and say so in the report.

Checks, in order (all read-only)

1. Is a sandbox pod running for the session?
   - Its phase, its containers' readiness and restart counts, and its events (kubectl describe, events section only).
   - Distinguish: still pulling the image, CrashLoopBackOff, OOMKilled, Pending (no capacity), or Running.
2. Did the harness start cleanly?
   - The workload container's logs from its start: look for import errors, tracebacks, a failed background_jobs import, or a state-schema or create_agent error when the graph is built.
   - If it has restarted, also read the previous container's logs (--previous).
3. Did the message reach the harness?
   - Look for turn_scheduled, turn_completed, wake_*, session_run_turn_failed and graph_run_reason around the user's timestamp.
   - Note: whether a turn started and whether it ended.
4. Did the SDK proxy it?
   - The waypoint SDK pod's logs around that time: the proxyToSandbox result, sandbox errors, HTTP status codes, timeouts.
   - Also check that the SDK pod's migration init container finished (its state and logs).
5. Did the stream reach the client?
   - In the harness logs: event_generator_* and sse_* around the timestamp.
   - In the SDK logs: /events and /notifications for the session.
6. What does the checkpoint show? Only if steps 1–5 point at the graph.
   - Read the session's latest checkpoint the way the lead did before (PostgresSaver.get_tuple, from inside the pod, read-only).
   - Report whether an interrupt is pending, pending_jobs, and the last 3 message types. Never the message content.

Deliverable

- A timeline of what happened, with UTC timestamps.
- For each check: the evidence (log lines, trimmed and with no secrets or user content) and your conclusion.
- One most likely root cause, marked as confirmed or suspected, plus the runner-up.
- The component that owns the fix: harness, waypoint SDK, oranger, or platform.

Boundaries: stop and report, don't cross

┌──────┬────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│ Gate │                                                         Don't                                                          │
├──────┼────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ D1   │ Restart, delete, scale, patch, exec anything that writes, or change any resource on any cluster.                       │
├──────┼────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ D2   │ Print, decode or copy Secrets or kubeconfig contents, or include user message content in the report.                   │
├──────┼────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ D3   │ Fix code, change config, or make a test deploy, even if the cause is obvious.                                          │
├──────┼────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ D4   │ Spend more than about 30 minutes in one area without a finding. Report what you saw and stop.                          │
├──────┼────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ D5   │ Go further if the evidence points to platform internals (gateway, operator, Cilium). Report the evidence and stop      │
│      │ there.                                                                                                                 │
└──────┴────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┘

When the report comes back, I'll review it and write the fix brief for whoever owns the cause.