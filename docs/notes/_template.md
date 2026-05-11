---
title: Short title
date: YYYY-MM-DD
author: Your Name
tags:
  - post-mortem # or investigation | design-note | spike
status: open # open | in-progress | resolved
---

# Short title

## Context

What triggered this note. Link to session/issue/PR/log.

## Findings

Bullet the evidence. Include `file:line` references and raw data snippets.

## Implications

What this changes about how we think or build.

## Follow-ups

Track work that came out of this note. Use GitHub-flavored task list syntax so
progress is visible at a glance, and give each item a stable ID (`F1`, `F2`, …)
so it can be referenced from commits, other notes, and conversations:

- [ ] **F1** — Open work item.
- [x] **F2** — Done work item; link to whatever resolved it (commit SHA, PR
      number, RFC, issue).

IDs are append-only: once assigned, never renumber. If a follow-up gets dropped
rather than done, strike it through and say why:
`- [ ] **F3** — ~~Foo~~ — superseded by #5678`.

When every box is checked, flip **Status** to `resolved` at the top of the note.
