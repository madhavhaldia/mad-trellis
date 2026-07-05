# 0006 — The container-grain image contract (bring-your-own-image)

> Status: ADOPTED 2026-06-27. The container grain (Inv 10 grain dial, project
> `isolation-substrate`) runs each agent inside a Linux container. mad-trellis
> deliberately ships **NO container image** — the operator supplies one. This doc is
> the **contract** that image must satisfy, the full set of env knobs that configure
> the grain, and how cooperative credential forwarding lands an agent's host login
> inside the container.

*Companion to [GROUNDING.md](../GROUNDING.md) (the invariants), [0001](./0001-form-and-architecture.md) (form), [0003](./0003-project-breakdown.md) (projects). Reference Containerfiles + exact build commands live in [docs/containerfiles/](./containerfiles/).*

---

## Why mad-trellis ships no image

This is a deliberate, load-bearing decision — not an omission. Four reasons, in
descending order of force:

1. **The substrate must not own the agent (Inv 13 / Inv 10-decoupling).** mad-trellis is
   a *governance substrate*, not an orchestrator and not an agent distribution.
   Governance is ambient; the user drives a bare agent session. The moment mad-trellis
   bakes `codex` or `claude` into an image it ships, it has taken ownership of *which
   agent you run and which version* — it starts to *operate* the agent for you. That is
   exactly the coupling Inv 10 forbids ("couple to no agent/host/orchestrator") and the
   "no goals, no task dispatch" posture of Inv 13. The agent is the user's; the image is
   the user's; mad-trellis governs whatever they bring.

2. **Release-cadence coupling.** `@openai/codex` and `@anthropic-ai/claude-code` ship on
   their own fast, independent cadences (often multiple releases a week). If mad-trellis
   shipped an image pinning an agent version, every agent release would force a mad-trellis
   image re-release, and every mad-trellis release would freeze users to a stale agent. The
   binary's release cadence and the agent's release cadence are *correctly decoupled* by
   making the image a user artifact. The user upgrades the agent by rebuilding their image
   (`npm i -g` picks up latest) with zero mad-trellis involvement.

3. **Distribution bloat.** mad-trellis ships as a single static, cgo-free Go binary on the
   order of **~8.6 MB**. A usable agent image is a `node:20` base plus a global npm install
   — hundreds of megabytes to over a gigabyte. Bundling (or even *referencing* a pulled)
   image would inflate the distribution by two orders of magnitude and drag in a registry
   dependency, an image-signing story, and a pull-on-first-run surprise. The binary stays
   small and self-contained; the heavy, churning artifact stays outside it.

4. **Supply-chain ownership.** An image mad-trellis publishes is an image mad-trellis is on the
   hook to patch — base-OS CVEs, the Node runtime, the transitively-installed npm tree, the
   agent itself. That is a supply-chain liability the project refuses to assume on the
   user's behalf. A bring-your-own-image model puts provenance, pinning, scanning, and CVE
   response where they belong: with the operator who chose the contents and runs them
   against their own code and credentials.

The cost is real and accepted: the container grain does **not** work out of the box. A
runtime-less or image-less host falls back to (or simply uses) the default **worktree**
grain, and the container-grain conformance rows / `doctor` checks **skip with a reason**.
Lost parallelism/strength is acceptable (classify upward); shipping an image we cannot own
is not.

---

## The image contract

An image used as `MAD_CONTAINER_IMAGE` MUST provide all of the following. The
substrate bind-mounts the agent's own git clone at `/work`, sets it as cwd, holds the
container alive with `sleep infinity`, and then `container exec`s the agent into it — so
the image only has to supply the *environment*, not an entrypoint.

| Requirement | Why | How the grain uses it |
|-------------|-----|-----------------------|
| **The agent binary on `PATH`** (`codex`, `claude`, or whatever you launch) | The launcher `container exec`s the agent by name; if it is not on `PATH` the exec fails | resolved at exec time inside the container |
| **`git`** | the agent commits inside the container against the `/work` clone; in-container commits land on the host via the bind mount and are harvested back to the canonical repo | the in-container git dev loop (commit → harvest → lease-serialized integrate) |
| **A POSIX `sh`** (`/bin/sh`) | manifest **gates** run inside the boundary as `sh -c <gate>` (`ExecGate`); the hold command and exec also assume a shell | gate validation runs where the agent's deps actually live |
| **A writable `HOME` matching `MAD_CONTAINER_HOME`** (default `/root`) | the agent reads/writes its config + credential dirs (`~/.codex`, `~/.claude`) under `HOME`; cooperative credential mounts target paths under it | credential mount targets, agent config |
| **(claude only) a NON-ROOT user** selected via `MAD_CONTAINER_USER`, with `HOME` via `MAD_CONTAINER_HOME` | **Claude Code refuses `--dangerously-skip-permissions` when running as `uid 0`.** A root-only image makes the most common unattended claude flag unusable | the launcher execs as that user; the user's `HOME` is where claude's creds are mounted |

Writable surfaces are handled by the grain, not the image: the agent's per-agent host
state dir is bind-mounted at its identical host path (so injected `TMPDIR` / `XDG_*` /
`MAD_*` resolve to a writable location) and `/tmp` is a tmpfs. The image does not
need to pre-create those. It DOES need `HOME` to exist and be writable by the selected
user so credential mounts and agent config work — `node:20`'s `node` user already ships
with `/home/node`.

### The root vs non-root split, concretely

- **codex** runs fine as **root** with `HOME=/root` (the grain's default). codex's
  credentials are a portable directory (`~/.codex`), so a plain root image is the simplest
  correct image.
- **claude** MUST run as a **non-root** user. Claude Code hard-refuses
  `--dangerously-skip-permissions` as `uid 0`, which is the flag a governed unattended
  session relies on. The image must create a non-root user (the `node` user that ships in
  `node:20` is the obvious choice), and you must point the grain at it with
  `MAD_CONTAINER_USER=node` and `MAD_CONTAINER_HOME=/home/node` so the launcher
  execs as that user and claude's credentials are mounted under the right `HOME`.

---

## Env knobs (the full set)

Every knob is read from the environment of the **daemon/launcher** process (the host side),
not the container. Unsafe, flag-like values (anything starting with `-`, or containing
characters outside `[A-Za-z0-9._/:,=@-]`) are **rejected** and fall back to the safe
default — a value like `--cap-add SYS_ADMIN` can never be smuggled in as a `container run`
flag (arg-injection guard).

| Env var | Default | Meaning |
|---------|---------|---------|
| `MAD_GRAIN` | `worktree` | Selects the isolation grain. `container` opts into this grain (the Inv-10 dial: `worktree → container → VM`). Equivalent to `--grain container`. |
| `MAD_CONTAINER_IMAGE` | (built-in base) | The image the grain runs. **This is your bring-your-own image** (e.g. `nm-codex`, `nm-claude`). Rejected if flag-like/illegal. |
| `MAD_CONTAINER_HOME` | `/root` | The `HOME` inside the container. Must match the selected user's real home so credential mounts and agent config land correctly. Set to `/home/node` for the `node`-user claude image. |
| `MAD_CONTAINER_USER` | `root` | The user the agent is exec'd as inside the container. Set to `node` (or any non-root user the image provides) for **claude**, whose `--dangerously-skip-permissions` refuses to run as root. |
| `MAD_CONTAINER_CREDENTIALS` | (on) | Cooperative credential forwarding toggle. Set to `off` / `0` / `false` / `no` to forward **no** host credentials even in the cooperative grain. Forwarding is **always** withheld when confined. |
| `MAD_CONTAINER_NETWORK` | (runtime default) | The container network. Empty (the cooperative default) omits `--network` → the runtime's default NAT egress + DNS. `none` = no egress. A named network = that network. An explicit value **always wins** over the confined-implies-`none` rule. |
| `MAD_CONTAINER_CONFINED` | `false` | The master confinement opt-in (truthy: `1`/`true`/`on`/`yes`). Adds `--cap-drop ALL` + a `--read-only` rootfs, implies network `none` (unless `MAD_CONTAINER_NETWORK` overrides), and **withholds all credential forwarding** — the untrusted-agent mode. Default off = the cooperative grain (default caps, writable rootfs, egress, creds forwarded). |

Two further knobs exist for operational robustness and are not part of the image contract
proper: `MAD_CONTAINER_PLATFORM` (pins the run platform to the host arch — defaults to
`linux/<host GOARCH>` — so the runtime never mis-selects a wrong-arch multi-arch variant),
and `MAD_CONTAINER_NO_AUTOSTART` (disables the auto-start of the Apple `container`
apiserver, which is otherwise brought up fail-closed before any run).

### Host isolation does not depend on any of these

Worth stating plainly: confinement flags and the network knob govern the container's *own
ephemeral rootfs, capabilities, and egress* — they harden against a **hostile** agent. Host
isolation itself is **mount-structural** and holds in every mode: the only host paths bind-
mounted are the agent's own `/work` clone and its own per-agent state dir. The host FS, the
canonical trunk, and the daemon socket are simply never mounted, so no value of any knob lets
the agent reach them.

---

## Cooperative credential forwarding

The cooperative container grain mirrors the worktree grain's posture: a cooperative agent
gets the same login it has on the host, so the in-container agent authenticates with **no
re-login**. This is the credential analogue of the cooperative-default egress network. It is
**withheld entirely when `MAD_CONTAINER_CONFINED` is set** (withholding host secrets is
the whole point of confinement) and can be disabled even in cooperative mode with
`MAD_CONTAINER_CREDENTIALS=off`. The two agents differ because their credentials live
differently:

- **codex — a portable directory mount.** codex stores host-agnostic OAuth bearer tokens in
  `~/.codex/auth.json`. The grain bind-mounts the host `~/.codex` **read-write** at
  `$MAD_CONTAINER_HOME/.codex` inside the container (read-write so token refresh
  persists). The directory is portable as-is, so a direct mount is correct.

- **claude — a macOS-Keychain-sourced `.credentials.json`.** Claude Code's on-disk
  `~/.claude/.credentials.json` is frequently **stale**: Claude Code refreshes its OAuth
  tokens into the **macOS login Keychain** (generic-password service `Claude Code-credentials`),
  not the file. So the grain sources the **live** credentials from the Keychain (falling back
  to the on-disk file on a non-macOS host where the file *is* live), normalizes them to the
  `{"claudeAiOauth":{...}}` shape, writes a fresh per-agent `.credentials.json` (mode `0600`)
  into a staging dir, and bind-mounts that staging `.claude` at
  `$MAD_CONTAINER_HOME/.claude`. A plain directory mount like codex's would carry stale
  tokens — hence the live-source path. The token value is never logged.

When confined, neither mount is added: an untrusted container gets no host secrets.

---

## See also

- [docs/containerfiles/](./containerfiles/) — reference Containerfiles for codex and claude
  and the exact local build commands. mad-trellis ships no images; these are recipes you build.
- [docs/0003-project-breakdown.md](./0003-project-breakdown.md) — `isolation-substrate` owns
  Inv 1 and Inv 10-grainswap (the grain dial this doc's contract sits under).
- [GROUNDING.md](../GROUNDING.md) — Inv 10 (decoupling + grain dial) and Inv 13 (governance is
  ambient; the substrate does not own the agent).
