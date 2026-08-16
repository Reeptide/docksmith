# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Docksmith is a simplified Docker-like build and container runtime built from scratch in Go — content-addressed image layers, a deterministic build cache, Linux process isolation via kernel namespaces + `pivot_root`, and container networking over hand-rolled rtnetlink. No dependency on Docker or runc, and **no external Go modules at all** (`go.mod` has no requires — keep it that way). Linux-only.

## Commands

```bash
make build   # CGO_ENABLED=0 go build -o docksmith .
make test    # go test ./... — needs no privileges
make setup   # build + import busybox base image (one-time)
make clean   # remove binary and state dir
make demo    # full end-to-end verification (requires root)
```

`build`, `run`, and `RUN` steps require root (namespaces + `pivot_root`). Read-only commands (`images`, `ps`, `logs`, `prune`) do not.

**`go build ./...` type-checks but does not write the executable.** If a CLI change appears to do nothing, `./docksmith` is stale — use `make build`.

`CGO_ENABLED=0` is deliberate: `archive/tar` pulls in `os/user` → `runtime/cgo`, and a static binary is wanted for a container runtime.

## Testing

Unit tests cover everything that does not need privileges. They exist mainly to pin the determinism invariants the content-addressed store depends on: `store.BuildTar` must produce byte-identical output regardless of input order, host umask, or wall-clock time, and `cache.ComputeKey` must be stable across map iteration order while remaining sensitive to every input that can change a layer. Netlink message encoding is asserted at the byte level, including 4-byte attribute padding.

`internal/runtime`'s namespace work needs root and is verified by `make demo`; only the root-free parts (child-config derivation, exit-status mapping, mount-spec parsing) have unit tests.

**Tests here are held to non-vacuity.** Several early ones asserted the buggy behaviour they were written to prevent, or rebuilt a production expression in the test and compared it to itself. When adding a test for a fix, break the fix and confirm the test fails before keeping it.

## Architecture

### Re-exec: three sentinels

There is no separate runtime binary and no daemon. `main.go` checks `os.Args[1]` **before any CLI parsing** for three sentinels — that ordering is load-bearing:

- **`__child__`** — the container. Parent sets `SysProcAttr.Cloneflags` and passes a single JSON `ChildConfig` argument (it replaced positional args, which could not carry mounts or network settings). The child configures its network, sets up mounts, enters the rootfs, and runs the command.
- **`__init__`** — container PID 1, *in-process*. `ChildMain` is already PID 1 inside the namespace, so it forks the real command as PID 2 and stays as init rather than exec'ing a separate binary. This is why no init binary is bind-mounted into the rootfs — and why it is unconditional, build `RUN` steps included: it costs nothing and leaves no trace in the delta.
- **`__supervisor__`** — `run -d`. Docksmith re-execs itself with `Setsid`, and that copy blocks in `IsolatedRun`, then writes the container's final state to `config.json`.

### The FD protocol (`internal/runtime/isolate.go`)

- **FD 3** — error pipe, child → parent. `CLOEXEC`, so a successful `exec` closes it and the parent reads EOF, signalling "setup succeeded". Setup failures are written as text.
- **FD 4** — sync barrier, parent → child. The child blocks on one byte before doing anything, giving the parent a window for work needing the child to already exist (moving a veth endpoint by pid). **The byte is written in every mode** — a path that forgets hangs forever with no timeout.

The parent must drain FD 3 **in a goroutine**. Reading it inline deadlocks: the read blocks until the child execs, and the child is blocked on FD 4 waiting for the parent.

### Build pipeline (`internal/builder`)

`parser.go` parses 7 instructions (`FROM`, `ENV`, `WORKDIR`, `EXPOSE`, `COPY`, `RUN`, `CMD`). `build.go` executes them against an assembled rootfs, producing one content-addressed delta layer per `COPY`/`RUN`. `RUN` deltas diff the post-execution rootfs against a re-extraction of the base layers, and `RUN` runs with `--net=none` — network access during a build would break cache determinism invisibly.

The diff compares `entrySignature`, which encodes an entry's **kind and permissions as well as its bytes** and never follows a symlink. Mode is in there because `RUN chmod +x` changes no bytes: a content-only comparison produced an empty layer and the container failed at runtime with "permission denied" and nothing in the build output. Kind matters because a path whose type changed cannot be fixed by writing the new entry over the old one, so a type change emits a whiteout first. Symlinks are read with `Readlink`, not through: following them turned every link a `RUN` created into a full copy of its target, which on a busybox rootfs is a megabyte per applet. FIFOs, sockets and device nodes are skipped without ever being opened — `os.ReadFile` on a FIFO blocks until a writer appears, so a stray FIFO hangs the build forever with no timeout.

`ignore.go` implements `.docksmithignore`, applied inside `collectGlob` so ignored files affect neither layer contents nor `COPY` cache keys. `Match` takes `isDir` because a trailing-slash rule (`build/`) names a directory and must not exclude a file of the same name.

`COPY` sources cannot leave the build context: the parser rejects absolute paths and `../`, and `collectGlob` re-checks each match through `safepath` to catch a symlink inside the context whose target is not. Without both, a build reads the host it happens to run on and stops being reproducible anywhere else.

Cache keys (`internal/cache/cache.go`) are SHA-256 of: **a layer format version**, the previous layer digest, the instruction text, `WORKDIR`, accumulated `ENV` (sorted), and for `COPY` each source file's mode and SHA-256 (sorted). **Bump `keyFormatVersion` whenever `BuildTar` output or `snapshotDelta`'s encoding changes for the same inputs** — without it, layers built by older code are silently reused. The bump is pinned by a golden digest in `cache_test.go`, which is *expected* to fail on a bump; recompute it deliberately rather than deleting the assertion. `EXPOSE` is deliberately *not* in the key. **Cascade rule:** one miss forces all later steps to miss.

### Storage (`internal/store`, `internal/image`)

Layers are deterministic tars named by the SHA-256 of their bytes. Deletions are OCI-style whiteouts (`.wh.<name>`), and `BuildTar` writes whiteouts **ahead of all content** — lexical order is not sufficient, since `.cache` sorts before `.wh..cache`.

`internal/safepath` has two resolvers and the distinction is load-bearing. `Resolve` follows the final path component; `ResolveNoFollow` resolves only the parent and leaves the last component alone. Anything that **creates, replaces or deletes** an entry — layer extraction, whiteouts, `/etc` generation, bind-mount points, `WORKDIR` — must use `ResolveNoFollow` or `MkdirAll`. Nothing writes into a rootfs with a bare `filepath.Join`. A busybox rootfs is almost entirely symlinks into `/bin/busybox`, so following the last component turns a whiteout for `bin/ls` into a deletion of `bin/busybox` and destroys every applet at once. Containment is unaffected: the parent is still resolved, so a symlinked directory mid-path is still caught.

Manifest file names carry a short digest of the exact reference (`team_app_v1_<hash>.json`). Sanitising alone is not injective — `team/app:v1`, `team_app:v1` and `team:app_v1` all flatten to the same string — so without it one image overwrites another's manifest and `rmi` on either collects the other's layers.

`export.go` implements `save`/`load`; import verifies every layer hash and manifest digest before writing anything, so a corrupt archive leaves the store untouched.

`rmi` and `prune` share `referencedLayers` (`cmd/rmi.go`) and delete only layers no surviving image references. Deleting unconditionally destroys shared base layers and every sibling image.

### Runtime (`internal/runtime`)

`mount.go` — `MS_REC|MS_PRIVATE` on `/` before any mount (required for `pivot_root`, and prevents leaking mounts to a systemd host), then `pivot_root`. Pre-pivot failures warn and fall back to `chroot`; **post-pivot failures are fatal**, since a failed unmount leaves the host filesystem readable at `/.oldroot`. Read-only bind mounts need a second `MS_BIND|MS_REMOUNT|MS_RDONLY` call — the kernel ignores flags on the first — and that remount covers only its own mount, so every submount under the target is enumerated from `/proc/self/mountinfo` and remounted too, deepest first. Otherwise `:ro` yields a read-only mount with writable holes in it.

`init.go` — PID 1 must handle signals explicitly: per `pid_namespaces(7)` a namespace's init only receives signals it has a handler for, even from an ancestor namespace, so an unhandled SIGTERM is discarded and `stop` degrades to SIGKILL after the full timeout. Uses `syscall.ForkExec`, never `os/exec`, whose internal reaper races the `Wait4(-1)` loop and eats the exit status.

### Containers (`internal/container`)

`~/.docksmith/containers/<id>/{config.json,rootfs/,container.log}`. Liveness compares **pid and `/proc/<pid>/stat` start time**, and a record with no recorded start time is reported *dead* rather than falling back to a bare pid check — a bare pid check is reuse-unsafe and would let `stop` signal an unrelated process as root. `Reconcile` corrects records left claiming to run by a killed supervisor. Directory modes are `chmod`ed explicitly after `MkdirAll`, whose mode argument is umask-masked — root's umask is 077 on some distributions, which produced state a `sudo run` wrote and an unprivileged `ps` could not read.

`prune` treats `created` as live only within a 30-minute grace period. A `run` killed between `Create` and `MarkStarted` otherwise leaves a record pinned forever, holding a rootfs and an IP lease that nothing will ever release.

A foreground `run` catches SIGINT/SIGTERM/SIGHUP rather than dying on them (`forwardSignals` in `cmd/run.go`). The terminal sends Ctrl-C to the whole foreground process group, so under the default disposition docksmith died before releasing the IP lease or deleting the container's DNAT rules — which then pointed at an address about to be reallocated. The signal is forwarded to the container explicitly, since a `kill` aimed at docksmith alone never reaches it; a second signal escalates to SIGKILL.

### Networking (`internal/netlink`, `internal/network`)

`internal/netlink` is a hand-rolled rtnetlink client: socket, sequence numbers, `NLMSG_ERROR` decoding, `rtattr` TLV encoding with nesting. Structs and constants come from stdlib `syscall`; only `IFLA_INFO_KIND`, `IFLA_INFO_DATA`, `VETH_INFO_PEER` are local. Payloads returned from a dump **must be copied** — they alias the receive buffer that the next `Recvfrom` overwrites.

`internal/network` — `docksmith0` bridge, IPAM under a `flock`, DNS/hosts generation (loopback resolvers are filtered; systemd-resolved's `127.0.0.53` points at nothing inside a container). `nat.go` shells out to `iptables` and is the **only** external-binary dependency; it uses `-I` not `-A` because Docker's rules sit at the head of `FORWARD` with a `DROP` policy. Published ports need MASQUERADE for `127.0.0.0/8` sources plus `route_localnet`, or the container replies to its own loopback. A host port already published by a live container is refused up front — two DNAT rules for the same port silently starve the older container, since the newer rule goes in at the head of the chain.

### State root (`cmd/state.go`)

`DOCKSMITH_ROOT` → `SUDO_USER`'s home → `$HOME`. `sudo` sets `HOME=/root`, so trusting `$HOME` would make `sudo docksmith run` and an unprivileged `docksmith ps` disagree about where state lives.
