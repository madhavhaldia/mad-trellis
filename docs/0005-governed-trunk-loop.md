# 0005 — The governed trunk loop (self-hosting topology)

> Status: ADOPTED 2026-06-23. mad-substrate now develops itself through its OWN mediated
> integrator (project 6), not the bootstrap `integrate`. This is the C15 topology
> flip / backlog #12 ("true dogfood"): the canonical trunk is a mad-substrate-mediated
> bare repo; the working checkout is a CONSUMER of it; every trunk advance goes
> through the validated, lease-gated, atomic-CAS promote and is observed in `watch`.

## Topology

```
  feature branch (nm/<slug>)                 mediated trunk (bare, authoritative)
  in a boundary / the checkout   --push-->   ~/.mad-substrate/trunk.git  ref: trunk
        |                          (origin = med; hook: nm/* only, Inv 7)   ^
        |                                                                   | atomic
        | mad-substrate trunk submit <ref>  -->  integration record (received) | update-ref
        | mad-substrate trunk promote <id>  -->  validate (merge-tree) +        | CAS
        |                                    trunk lease (single-writer) ---+
        v
  git fetch med && merge --ff-only med/trunk   (the checkout follows trunk)
```

- **Mediated trunk** = a bare repo mad-substrate owns (`<runtime>/trunk.git`, default
  `~/.mad-substrate/trunk.git`). Its `trunk` ref is the authority. A receive `update`
  hook lets a boundary push ONLY `refs/heads/nm/*` and REJECTS any push to `trunk`
  (Inv 7) — agents never advance trunk by pushing. The integrator advances it via a
  LOCAL `update-ref` (hooks do not fire), which is the sole trunk-write path.
- **The integrator** (project 6) is the single promoter: `submit` records a branch
  against the current trunk tip (the base); `promote` validates the merge
  deterministically (`git merge-tree --write-tree`, no LLM), acquires the trunk
  lease (CAS-fail-fast single-writer), then advances trunk in ONE atomic update-ref
  CAS. Idempotent; a crash at any earlier point leaves trunk byte-identical.
- **`watch`** is the read-only mirror: trunk tip (`integrate.trunk`), pending/
  in-flight + promoted integrations (`integrate.list`), lease holders
  (`lease.list`), and the decision-audit stream (`audit.tail`). Non-load-bearing.

## One-time setup (the flip)

```sh
mad-substrate daemon &                 # standing daemon; auto-creates trunk.git + hook
# seed the mediated trunk from the current history (local ref write, not an agent push):
git -C ~/.mad-substrate/trunk.git fetch <repo> +<branch>:refs/heads/trunk
git -C <repo> remote add med ~/.mad-substrate/trunk.git
```

## The loop (every change)

```sh
# 1. isolate the work
git checkout -b nm/<slug>          # or: mad-substrate spawn (boundary; origin = med)
# ...edit, commit on nm/<slug>...

# 2. publish the branch to the mediated repo (hook allows nm/*)
git push med nm/<slug>

# 3. govern the trunk advance (validated + lease + atomic CAS)
id=$(mad-substrate trunk submit refs/heads/nm/<slug>)   # -> received
mad-substrate trunk promote "$id"                        # -> promoted, trunk advances
#   watch shows: received -> validating -> promoted; trunk tip moves; audit records.

# 4. follow trunk in the working checkout
git fetch med && git merge --ff-only med/trunk
```

A merge conflict / stale base makes `promote` abort cleanly (trunk untouched);
resubmit against the new tip. A mid-promote death is reconciled by ancestry on the
next `promote`/`status`/recover.

## Relationship to the bootstrap `integrate`

`mad-substrate integrate <branch>` (a plain lease-serialized `git merge` into the
checkout) was the BOOTSTRAP that let mad-substrate build itself before the integrator
loop was wired. It is retained for emergencies but is NO LONGER the way trunk
advances — it bypasses the validated gate, the mediated trunk, and `watch`. The
governed path above is canonical.
