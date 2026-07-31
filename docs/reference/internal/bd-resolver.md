---
title: Per-Scope bd Resolver
description: How each scope pins the exact bd CLI version and store schema it will accept, and why Gas City refuses to run bd rather than risk corrupting a store.
---

Every Gas City scope — the city root and each rig — talks to its beads store by
running the `bd` CLI. Which `bd` gets run used to be a question with no reliable
answer: Gas City handed the bare name `bd` to the operating system, which
resolved it against whatever `PATH` the calling process happened to carry.

On a host with more than one `bd` installed, that is a coin flip. `bd` itself
notices:

```console
$ bd version
bd version 1.1.0 (8e4e59d39)

Warning: multiple 'bd' binaries found in PATH:
  /home/you/.local/bin/bd
  /home/linuxbrew/.linuxbrew/bin/bd
```

The coin flip matters because a `bd` whose version disagrees with a store's
schema does not refuse the write. It performs it, against a schema it
misunderstands. The damage is silent and it is to the store.

This page describes the resolver that closes that gap.

## What the resolver does

Before **every** `bd` invocation, for the scope that invocation belongs to,
Gas City:

1. resolves an exact **absolute** `bd` binary,
2. checks that binary's version and the store's schema against the scope's pin,
   and
3. **refuses to run `bd` at all** when either disagrees.

A refusal happens before the process is spawned. A mismatched `bd` does not run
once and then get reported — it does not run.

## Pinning a scope

The pin lives in the scope's `.beads/identity.toml`, the one file inside
`.beads/` that is git-tracked rather than ignored:

```toml
# .beads/identity.toml — canonical, git-tracked.
# Edited only at scope creation or by deliberate human/`gc` migration.

[project]
id = "my-project"

[bd]
expected_version = "1.1.0"
schema_version = 53
```

Both keys are optional and enforced independently:

| Key | Meaning |
|---|---|
| `expected_version` | The exact `bd` CLI version this scope accepts. A leading `v` is ignored, so `1.1.0` and `v1.1.0` are the same pin. Build metadata in `bd`'s output is ignored; only the semver core is compared. |
| `schema_version` | The store schema version `bd` must report for this scope. |

Verification costs at most one probe. A scope pinning only a version is checked
with `bd version`, which does not touch the store. A scope pinning a schema is
checked with `bd context --json`, which reports the version and the schema
together.

Results are cached per scope. The cache key includes the binary's path, size,
and modification time, so replacing `bd` in place — or changing which one is
found first — re-runs verification without any cache-clearing step. Editing
`identity.toml` re-verifies too.

## The fail-closed contract

The **pin** is what turns enforcement on, not the presence of the file:

| Scope state | Result |
|---|---|
| Pinned, version and schema agree | `bd` runs, via the verified absolute path |
| Pinned, version disagrees | **Refused.** `bd` is never invoked |
| Pinned, schema disagrees | **Refused.** `bd` is never invoked |
| Pinned, version or schema **unprovable** | **Refused.** "Could not check" never reads as "checked and fine" |
| `identity.toml` malformed or unreadable | **Refused.** The file may carry a pin; it cannot be assumed not to |
| **Unpinned** — no file, or no `[bd]` section | **Inert.** Nothing is enforced |

The unpinned default is deliberate. Pinning is opt-in, taken by a scope at
registration. Making an absent pin fatal would brick every already-registered
scope on upgrade — a larger outage than the one being guarded against.
Enforcement is dark until a scope opts in, and total once it has.

A misspelled key is an error, not a silently-ignored line. A typo that quietly
read as "unpinned" would leave you believing a scope was protected while it was
not.

## Why a global PATH swap is not enough

The obvious-looking alternative — install the right `bd` and put it first on
`PATH` — cannot do this job, for three reasons.

**`PATH` is one value; pins are per scope.** A city and its rigs can legitimately
sit on different `bd` versions, mid-migration. One search path cannot express two
answers.

**`PATH` composition is not under Gas City's control.** It depends on the shell,
the login mode, and the launching process. The very reading that motivated this
resolver — "both `bd` are 1.1.0" — was itself a `PATH`-composition artifact; the
two binaries were not the same.

**`PATH` says which binary is *found*, never which is *correct*.** Promoting a
binary to the front makes it reachable. It does not make it match the store's
schema. Enforcement here is keyed on the scope and applied to the resolved
absolute path, so reordering `PATH` cannot satisfy a pin it does not match, and
cannot make a mismatched binary reachable to Gas City.

## What the resolver covers

Because enforcement sits at the Beads command runner — the single point every
Gas City `bd` subprocess passes through — it applies uniformly to `gc bd`, the
controller store, rig adoption, hooks, and every internal store open. There is
no path that reaches `bd` around it.

Agent sessions are the one surface Gas City can influence but not gate: an agent
typing `bd` in its own shell runs whatever its shell finds. For those, the
resolved binary's directory is prepended to the session's `PATH`, so a bare `bd`
lands on the same binary Gas City verified for that scope. When the resolver
refuses, **nothing** is projected — an unverified binary is never handed to an
agent wearing Gas City's endorsement — and the refusal still surfaces loudly on
the first Gas City-mediated `bd` call.

## Reading a refusal

A refusal names the scope, the binary, and both sides of the disagreement:

```
bd resolver: scope /work/repo-a: /usr/local/bin/bd is bd 1.0.5 but
identity.toml pins bd 1.1.0, refusing to invoke it
```

```
bd resolver: scope /work/repo-a: store schema is 42 but identity.toml pins
schema 53; /usr/local/bin/bd would corrupt this store, refusing to invoke it
```

Every refusal wraps a single sentinel error, so tooling can tell "Gas City
refused to run `bd`" apart from "`bd` ran and failed" — a distinction that
matters when deciding whether to retry.

To resolve one, do exactly one of: install the `bd` the scope pins, or change the
pin deliberately and commit that change. `identity.toml` is git-tracked so the
second is a reviewable act, not a local workaround.
