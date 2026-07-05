# Security Policy

mad-trellis is a **safety substrate** — its whole purpose is to keep parallel agents from corrupting
each other or the trunk — so security reports are taken seriously.

## Reporting a vulnerability

**Do not open a public issue for a security vulnerability.**

Report it privately via one of:

- GitHub's **[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)**
  on this repository (Security → Report a vulnerability), or
- open a private GitHub Security Advisory at https://github.com/madhavhaldia/mad-trellis/security/advisories/new .

Please include: a description, the affected version/commit, reproduction steps or a proof of concept,
and the impact you observed. If you can, say which invariant (see [GROUNDING.md](./GROUNDING.md)) you
believe is violated.

This is currently a small project; expect a best-effort acknowledgement within a few days and a
candid discussion of timeline and fix. Please give a reasonable window to address an issue before any
public disclosure. We're happy to credit reporters who want it.

## Supported versions

Pre-1.0: only the latest `main` is supported. There are no backports yet.

## Scope & threat model (what "secure" means here)

mad-trellis defends the **substrate beneath agentic work**: an agent (cooperative or not) must not be
able to corrupt another agent's work or advance the trunk outside the validated, lease-gated
integrator. Especially in scope:

- bypassing the trunk single-writer / validated-integration path (Inv 6/7);
- one session affecting another's leases, boundary, or integration (Inv 4 identity binding);
- a singular resource reachable without a grant (Inv 8 default-deny);
- escaping the isolation boundary (note the **honest scope**: the `worktree` grain is
  grain-relative — it guarantees mad-trellis never *hands out* an escaping path, not that an
  uncooperative process can't `cd /`; **structural** confinement of an uncooperative agent is the
  `container` grain);
- the executable safety gate (`mad-trellis conform`) passing while a safety clause is actually
  violated (a vacuous check is itself a defect).

Out of scope / known limitations: the cooperative layer (the native `mad-trellis mcp` / `hook`
subcommands) is **advisory and fail-soft** by design (the hard floor, not the cooperative layer, is
the safety boundary); mad-trellis has not yet had an external security audit; pre-1.0 surfaces may
change.

If you're unsure whether something is in scope, report it privately and ask.
