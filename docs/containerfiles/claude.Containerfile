# Reference image for the mad-trellis container grain — Anthropic Claude Code.
#
# mad-trellis ships NO image (see docs/0006-container-grain-image-contract.md). This is
# a recipe you build LOCALLY. The critical difference from the codex image: Claude Code
# REFUSES `--dangerously-skip-permissions` when running as root (uid 0), and that flag
# is what a governed unattended session relies on. So this image runs as a NON-ROOT
# user. node:20 already ships a `node` user with HOME=/home/node — we use it.
#
# Pair this image with:  MAD_CONTAINER_USER=node  MAD_CONTAINER_HOME=/home/node
# so the launcher execs claude as `node` and mounts its credentials under /home/node.

FROM node:20

# git: the agent commits inside the container against the /work clone.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# The agent binary on PATH. `npm i -g` picks up the latest claude-code release at build
# time — re-run the build to upgrade (the agent cadence is decoupled from mad-trellis).
RUN npm install -g @anthropic-ai/claude-code

# Use the NON-ROOT `node` user (uid 1000) that ships in node:20, so claude's
# --dangerously-skip-permissions works. Its HOME is /home/node; the grain mounts the
# live Keychain-sourced .credentials.json under $MAD_CONTAINER_HOME/.claude there.
ENV HOME=/home/node
USER node
WORKDIR /work

# No ENTRYPOINT/CMD is needed: the substrate holds the container alive with
# `sleep infinity` (via --entrypoint) and `container exec`s claude into /work as `node`.
