# Reference image for the mad-substrate container grain — OpenAI Codex.
#
# mad-substrate ships NO image (see docs/0006-container-grain-image-contract.md). This is
# a recipe you build LOCALLY. codex runs fine as root, so this is the simple case:
# the agent binary on PATH, git, a POSIX sh (node:20 is Debian-based), and HOME=/root
# (the grain's default MAD_CONTAINER_HOME). codex's credentials are a portable
# ~/.codex dir the grain bind-mounts in, so no user/credential gymnastics are needed.

FROM node:20

# git: the agent commits inside the container against the /work clone.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# The agent binary on PATH. `npm i -g` picks up the latest codex release at build
# time — re-run the build to upgrade (the agent cadence is decoupled from mad-substrate).
RUN npm install -g @openai/codex

# Run as root with HOME=/root, matching the grain's default MAD_CONTAINER_HOME.
# codex does not refuse root (unlike claude), so root is fine here.
ENV HOME=/root
WORKDIR /work

# No ENTRYPOINT/CMD is needed: the substrate holds the container alive with
# `sleep infinity` (via --entrypoint) and `container exec`s codex into /work.
