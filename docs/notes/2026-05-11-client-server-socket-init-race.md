---
title: "`CRUSH_CLIENT_SERVER=1` socket init race"
date: 2026-05-11
author: Christian Rocha
tags:
  - investigation
status: resolved
---

# `CRUSH_CLIENT_SERVER=1` socket init race

## Context

Running `CRUSH_CLIENT_SERVER=1 crush` exits ~50% of the time on cold start with:

```
Failed to initialize crush server: stat /tmp/crush-501.sock: no such file or directory
```

Branch: `client-server-race`. Reproduces on macOS (darwin), almost always when
no prior `crush server` is running.

## Findings

The error string is produced in exactly one place: `internal/cmd/root.go:426`,
inside `ensureServer`'s post-spawn polling loop.

### Bug A — readiness poll is too short, and uses the wrong signal

`ensureServer` (`internal/cmd/root.go:394`):

1. `os.Stat(hostURL.Host)` returns `ErrNotExist` ⇒ `needsStart = true`.
2. `startDetachedServer(cmd)` forks a detached `crush server` (`root.go:471`).
3. Polls the socket path with `os.Stat`: `for range 10` × `100ms` = **1s total**
   (`root.go:413‑424`).
4. On timeout it returns the user-visible error.

The detached server (`internal/cmd/server.go:30`) does substantial work _before_
binding the socket: `config.Load(...)`, `crushlog.Setup`, `server.NewServer`
(which calls `backend.New` — pubsub, project registry, etc.), then finally
`srv.ListenAndServe()` which is what actually creates the unix socket via
`net.Listen("unix", …)` (`internal/server/net_other.go:7`).

On a true cold start (no warm fs cache, no prior server) 1s is frequently not
enough to reach `net.Listen`. That matches the ~50% rate.

A second symptom of the same shape lives a few lines down in `connectToServer`
(`root.go:357‑374`), where `CreateWorkspace` is retried 5× with the comment
_"The server socket may exist before the HTTP handler is ready"_ — i.e.
`os.Stat` succeeding does not imply the HTTP handler is serving. We need a real
readiness probe, not a stat.

### Bug B — `exec.CommandContext` kills the child we just spawned

`startDetachedServer` builds the child with:

```go
c := exec.CommandContext(cmd.Context(), exe, cmdArgs...)
...
detachProcess(c)         // Setsid on !windows
...
_ = c.Start()
_ = c.Process.Release()
```

`Process.Release()` does **not** disarm the `CommandContext` watcher goroutine —
the watcher still calls `os.Process.Kill` on the child once `cmd.Context()` is
`Done()`. When the parent's 1s poll loop times out and `crush` exits, the
watcher kills the server we just started. The next invocation finds no socket
again and re-races.

Evidence shows up in the per-host server log dir:

```
$XDG_CACHE_HOME/crush/server-<safeHost>/stderr.log
```

after a failed run — a half-initialized server cut off mid-init.

### Bug C — `restartIfStale` can leave the system with no server

`restartIfStale` (`root.go:436`) on version mismatch:

1. `c.ShutdownServer(...)`,
2. waits for the socket to disappear,
3. `os.Remove(hostURL.Host)` (force-cleanup),
4. `return nil`.

In `ensureServer` the call site is:

```go
} else if err == nil {
    if err := restartIfStale(cmd, hostURL); err != nil {
        slog.Warn("Failed to check server version, restarting", "error", err)
        needsStart = true
    }
}
```

`needsStart` is only set on **error**. On a successful version-mismatch shutdown
(`return nil`), we kill the old server, never start a new one, fall through to
the poll loop, and emit the same "no such file" error deterministically.

### Bug D — non-`ErrNotExist` stat errors are swallowed

The first stat in `ensureServer`:

```go
if _, err := os.Stat(hostURL.Host); err != nil && errors.Is(err, fs.ErrNotExist) {
    needsStart = true
} else if err == nil {
    ...
}
```

Any other error (e.g. a stray non-socket file at that path, EACCES, or a
half-cleaned-up directory) hits neither branch. `needsStart` stays `false`, no
server is spawned, and we go straight to the timing-out poll loop.

### Bug E — concurrent clients double-spawn

Two `crush` invocations launched at roughly the same time both stat → miss →
spawn a detached server. Whichever loses the race for `net.Listen` exits with a
bind error visible only in the per-host log. Today this is rare because there's
no other reason to launch two clients simultaneously, but once we add editor
integrations or any tooling that may invoke `crush` opportunistically it becomes
a real failure mode.

## Implications

The current flow conflates three different things — _socket file exists_, _HTTP
listener is up_, _child process is alive past parent exit_ — and treats them as
if they were the same signal. Fixing this means:

- using a real readiness probe over the socket;
- truly detaching the spawned child from the parent's lifetime;
- making the version-mismatch and unexpected-stat-error paths converge on "needs
  (re)start" instead of silently leaving the system half-broken;
- serializing concurrent spawns.

This is the right time to do it because we're about to lean harder on the
client/server split (the branch name is `client-server-race`), and every caller
that ships in that mode will hit these races.

## Proposed fix

Implementation guide. None of this is wired up yet.

### 1. Replace `os.Stat` polling with an HTTP readiness probe

In `ensureServer`, after `startDetachedServer` returns, build a `*client.Client`
(we already do this in `restartIfStale`) and poll `GET /v1/health` (or
`/v1/version`) with a short per-attempt timeout. Success criterion is a 2xx
response, not a stat.

Bonus: this can subsume the 5×200ms retry loop around `CreateWorkspace` in
`connectToServer` (`root.go:357‑374`), since by the time `ensureServer` returns
the HTTP handler is known to be live.

Suggested budget: total **10s**, per-attempt **50–100ms**, with the loop exiting
early on context cancellation. Tunable via an env var
(`CRUSH_SERVER_READY_TIMEOUT`) for very slow disks / CI.

### 2. Detach the child from the parent's context

In `startDetachedServer`:

- Use `exec.Command(exe, cmdArgs...)` instead of
  `exec.CommandContext(cmd.Context(), ...)`. The existing `Setsid` in
  `detachProcess` (`internal/cmd/root_other.go:10`) is what actually detaches
  the process group; `CommandContext`'s only job here is to install a
  kill-on-cancel watcher we explicitly do **not** want.
- Keep `Process.Release()`. On Windows, the equivalent path (`root_windows.go`)
  needs the same audit — `CommandContext` on Windows also kills on context
  cancel.

### 3. Fix `restartIfStale`'s contract

Two acceptable shapes:

- **Return value:** change to `restartIfStale(...) (restarted bool, err error)`.
  Caller sets `needsStart = restarted || err != nil`. Easiest to read.
- **Sentinel error:** define `errVersionMismatchRestarted` and let the caller
  flip `needsStart` when it sees that error. Slightly more idiomatic-Go but more
  action-at-a-distance.

Either way, the goal is: after a successful stale-server shutdown,
`ensureServer` _must_ spawn a fresh one before polling.

### 4. Treat unexpected stat errors as "needs (re)start"

Restructure the leading stat in `ensureServer` to:

- `ErrNotExist` → `needsStart = true`.
- `nil` → call `restartIfStale` (see #3).
- anything else → log, attempt `os.Remove(hostURL.Host)`, set
  `needsStart = true`. If `Remove` fails with anything but `ErrNotExist`, bubble
  up — we don't want to spawn into a path we can't bind.

### 5. Single-flight server start

Add a `flock`-based sentinel under the existing per-host server dir:

```
$XDG_CACHE_HOME/crush/server-<safeHost>/start.lock
```

`startDetachedServer` (or a wrapper) acquires an exclusive lock for the duration
of "spawn child + wait for `/v1/health`". Other clients block on the lock; once
it's released they run the readiness probe directly and skip the spawn. Use
`golang.org/x/sys/unix.Flock` on `!windows` and `LockFileEx` on Windows (or
`github.com/gofrs/flock` if we want a single cross-platform import — already
common in our stack).

### 6. Out of scope but worth noting

- The `for range 5 { CreateWorkspace }` retry in `connectToServer` should be
  deleted once #1 lands — it's compensating for the same race.
- `slog.Warn("Failed to check server version, restarting", ...)` in
  `ensureServer` is misleading: today, on `restartIfStale` error we don't always
  actually restart; with #3 in place this message becomes truthful.
- Consider emitting a single structured error type for "server failed to become
  ready" that includes `stderrPath` so the user can see the real reason instead
  of `stat: no such file or directory`.

Each numbered item above is independently landable and independently revertable,
in any order; (1)+(2) alone should drop the cold-start failure rate to
effectively zero.

## Follow-ups

Tick boxes as work lands and link to whatever resolved each item (commit SHA,
PR, RFC, issue). Flip **Status** to `resolved` once all are checked.

- [x] **F1** — Readiness probe over `/v1/health` + delete the `CreateWorkspace`
      retry in `connectToServer` (covers Bug A and the §6 retry). Landed in
      `9b366c36`.
- [x] **F2** — Detach the spawned `crush server` from the parent's context
      (covers Bug B; audit Windows path in `internal/cmd/root_windows.go`).
      Landed in `8021114b`.
- [x] **F3** — `restartIfStale` always (re)starts after shutdown +
      unexpected-stat-error handling (covers Bugs C and D). Landed in
      `a5f71d4e`.
- [x] **F4** — Single-flight spawn via `flock` on
      `$XDG_CACHE_HOME/crush/server-<safeHost>/start.lock` (covers Bug E).
      Landed in `767d401b`.
- [x] **F5** — Regression test: spawn N `crush` clients in parallel against a
      fresh `XDG_CACHE_HOME` and assert none emits the readiness error, plus a
      positive `/v1/health` check on the resulting socket. See
      `internal/cmd/clientserverrace/race_test.go` and `95c3b444`.
