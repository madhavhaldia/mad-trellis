# Reference Containerfiles for the mad-trellis container grain

**mad-trellis ships NO container images.** These are *reference recipes* — you build them
**locally** and point the container grain at the result with `MAD_CONTAINER_IMAGE`.
The full rationale and the complete image contract live in
[../0006-container-grain-image-contract.md](../0006-container-grain-image-contract.md).

The build commands below use Apple `container` on macOS / Apple Silicon (the runtime the
grain conducts). With Docker, substitute `docker build` for `container build`.

## codex

Build (root user, `HOME=/root` — the grain defaults):

```sh
container build --platform linux/arm64 -t nm-codex -f docs/containerfiles/codex.Containerfile docs/containerfiles
```

Run the grain with it (codex uses the defaults — no user/home override needed):

```sh
export MAD_GRAIN=container
export MAD_CONTAINER_IMAGE=nm-codex
```

## claude

Build (NON-root `node` user, `HOME=/home/node` — claude refuses `--dangerously-skip-permissions` as root):

```sh
container build --platform linux/arm64 -t nm-claude -f docs/containerfiles/claude.Containerfile docs/containerfiles
```

Run the grain with it — you MUST select the non-root user and its home:

```sh
export MAD_GRAIN=container
export MAD_CONTAINER_IMAGE=nm-claude
export MAD_CONTAINER_USER=node
export MAD_CONTAINER_HOME=/home/node
```

## Notes

- `--platform linux/arm64` matches Apple Silicon; use `linux/amd64` on an x86 host (the
  grain also pins the run platform to the host arch by default — see
  `MAD_CONTAINER_PLATFORM`).
- Upgrading the agent is just rebuilding: `npm i -g` in the Containerfile picks up the
  latest agent release. The agent's release cadence is decoupled from mad-trellis's by design.
- Cooperative credential forwarding lands your host login inside the container with no
  re-login (codex: `~/.codex` dir mount; claude: live macOS-Keychain `.credentials.json`).
  It is withheld when `MAD_CONTAINER_CONFINED=1` and can be disabled with
  `MAD_CONTAINER_CREDENTIALS=off`.
