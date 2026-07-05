# 0007 — Event nudges for the integration review loop

> Status: ADOPTED 2026-07-05. The integration review loop now has a daemon-authored
> event plane: small wake-up records produced by governed state transitions and delivered
> opportunistically by the launcher/MCP layer. Events are not messages, not tasks, and not
> state. They exist only to make an idle agent re-read the authoritative state in place.

## Problem

The first review loop was pull-only. A builder could request integration and an integrator
could poll `mad_integration_pending`; after a verdict the builder could poll
`mad_integration_status`. That was safe, but it made progress depend on agents remembering to
poll while idle.

The tempting fix is an agent mail system. That is the wrong shape. Free-text agent-to-agent
mail is a cross-session channel: it smuggles prompt content across isolation boundaries and
starts behaving like orchestration. The substrate needs a wake-up, not a conversation.

## Principle

**Events are wake-ups. State is truth.**

The durable truth remains the integration request row: pending requests, claims, verdicts,
feedback, merge OIDs, and timestamps are read through the existing integration tools. An event
row says only that governed state changed and that a participant should re-read that state.

An `integration.events` item is deliberately tiny:

```json
{"id": 7, "kind": "integration.verdict", "branch": "nm/example", "created_at_ms": 1783235000000}
```

There is no title, feedback, body, payload, file content, prompt text, task instruction, or
destination-authored address. If an event is missed, startup reconciliation through
`mad_integration_pending` or `mad_integration_status` recovers the truth.

## Delivery Hierarchy

Delivery is best-effort and fail-soft. Failure to deliver a nudge must never make a governed
session more fragile than a bare agent session.

1. **Launcher PTY injection.** A `mad-trellis launch` builder session and an
   `integrator run` session poll `integration.events`. When the terminal has been quiet long
   enough, the launcher writes one fixed-template line plus Enter into the child PTY. The
   politeness guard defers while user input is fresh, so a nudge does not interleave with a
   live prompt.
2. **MCP piggyback fallback.** If the agent was not launched through the PTY wrapper, the MCP
   server polls the same event stream and appends queued fixed-template lines to the next tool
   result. This preserves the in-place loop without requiring a separate inbox UI.
3. **Reconcile on start.** Every role guide tells the agent to drain authoritative state on
   startup: integrators run `mad_integration_pending`; builders use `mad_integration_status`
   for their own branch. This is the recovery path for missed events, disabled nudges, daemon
   restarts, and GC.

`MAD_NUDGES=off` disables the nudge delivery layer. It does not disable the integration state
machine or the event rows; it only removes the ambient wake-up mechanism.

## Audience Model

Events are daemon-authored and audience-scoped.

- `integrator` audience events are queue wake-ups such as `integration.requested` and
  `integration.requeued`. They tell an integrator to re-read `mad_integration_pending`.
- `branch:<branch>` audience events are branch wake-ups such as `integration.claimed` and
  `integration.verdict`. They tell the branch holder to re-read status.

Authorization is checked at poll time through the public daemon contract. A branch event is
readable only by the launcher-pattern session for `nm/<session>` or by the request record's
holder. A third unrelated session polling that branch gets no branch events. The holder still
gets the verdict event, which is the positive control that proves events existed and the
absence is authorization, not silence.

The event plane has no authoring RPC. There is no `integration.publish`, `integration.emit`,
`integration.notify`, `integration.broadcast`, `integration.send`, or `integration.message`.
Agents cannot address another agent and cannot write event rows.

## Injection Rule

Every injected line is selected from a fixed daemon-side template by event kind and audience.
Only structured daemon state such as the validated branch ref or an event count may be
interpolated. Agent-authored free text is forbidden: no title, feedback, message body, prompt,
or review prose may appear in the injected line.

This is deliberately stricter than free-text agent-mail systems. The nudge can say "a verdict
exists; run the status tool." It cannot say what the verdict says, what to change, or what task
to do next. That content stays behind the explicit state-read tool, where the agent asked for
it and the daemon returned the authoritative row.

## Integrator Resurrection

`mad-trellis integrator run` is the trunk-side wrapper for the integrator agent. It wires the
integrator MCP role, runs the agent under a PTY, polls integrator-audience events, and restarts
after non-zero crashes with bounded backoff unless `--no-keepalive` is set.

The zombie guard is simple: after a crash, before restarting, the wrapper inspects the
integrator presence lease. If another integrator is now live, this wrapper exits instead of
fighting it. Repeated rapid crashes park the loop until the operator presses Enter. The daemon
still owns the lease truth; the wrapper is only the process-level resurrection policy.

`mad-trellis integrator start` opens a visible terminal on `integrator run`. It is a
convenience surface, not a daemon dispatcher.

## Lifecycle

The event lifecycle is intentionally ad hoc and bounded.

- A new consumer starts with cursor `0`, so it catches up on every authorized event still
  inside the retention window.
- Polling is read-and-advance per consumer: the cursor moves only after authorized events are
  returned.
- Event GC is a small wake-up retention window, currently 24 hours. Cursors are not the truth
  and are not enough for correctness.
- Durable integration rows outlive missed events. Agents must reconcile from pending/status
  whenever they start, restart, or suspect they missed a nudge.

The safety argument is the same as the rest of the substrate: no peer mesh, no agent-authored
payload, no task dispatch, and no correctness dependence on delivery. The event plane makes the
ambient governance loop more responsive without changing who drives the work.
