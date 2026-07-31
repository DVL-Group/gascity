---
title: Overview
description: Internals-grade reference for operators and contributors who need to see under the hood.
---

These pages document internal mechanics — useful when operating, debugging,
or contributing, but not required for everyday use. They describe how Gas
City works underneath the surfaces the rest of the reference documents.

| Page | Covers |
|---|---|
| [Beads Storage Topology](/reference/internal/beads-topology) | How a city and its rigs share one Dolt server while keeping each rig's beads logically isolated by prefix |
| [Per-Scope bd Resolver](/reference/internal/bd-resolver) | How each scope pins the exact `bd` CLI version and store schema it will accept, and why Gas City refuses to run `bd` rather than risk corrupting a store |
