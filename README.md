# Docksmith

A Docker-like image builder and container runtime written from scratch in Go, with **no external dependencies** — not Docker, not runc, not even a netlink library. Docksmith implements content-addressed image layers, a deterministic build cache, Linux namespace isolation via `pivot_root`, and container networking driven by hand-rolled rtnetlink messages.

---

## Table of Contents

1. [Requirements](#requirements)
2. [Quick Start](#quick-start)
3. [Project Structure](#project-structure)
4. [Building Images](#building-images)
5. [Running Containers](#running-containers)
6. [Networking](#networking)
7. [All CLI Commands](#all-cli-commands)
8. [The Docksmithfile Language](#the-docksmithfile-language)
9. [How the Cache Works](#how-the-cache-works)
10. [State Directory Layout](#state-directory-layout)
11. [Architecture Notes](#architecture-notes)
12. [Testing](#testing)

---

## Requirements

- **OS:** Linux. macOS and Windows are not supported — kernel namespaces are required.
- **Go:** 1.22 or later.
- **busybox-static:** for the base image (`sudo apt-get install -y busybox-static`).
- **iptables:** only for `--net=bridge` (the default). `--net=none` and `--net=host` work without it.
- **Root:** `docksmith build` and `docksmith run` need `CLONE_NEWNS`/`NEWPID`/`NEWUTS`/`NEWIPC`/`NEWNET` and `pivot_root`. Read-only commands (`images`, `ps`, `logs`, `prune`) do not.

Under `sudo`, docksmith resolves its state directory from `SUDO_USER`'s home rather than `$HOME`, so `sudo docksmith run` and an unprivileged `docksmith ps` see the same store. `DOCKSMITH_ROOT` overrides this.

---

## Quick Start

```bash
make build                          # builds a static binary
sudo ./setup/import-base-images.sh  # one-time: creates busybox:latest

sudo ./docksmith build -t myapp:latest ./sample-app
sudo ./docksmith run myapp:latest

make test                           # unit tests, no privileges needed
sudo make demo                      # full end-to-end verification
```

---

## Project Structure

```
docksmith/
├── main.go                     # CLI dispatch + three re-exec sentinels
├── Makefile
│
├── cmd/                        # one file per subcommand
│   ├── build.go  images.go  import.go  rmi.go  run.go
│   ├── ps.go  logs.go  stop.go  rm.go       # container lifecycle
│   ├── save.go  load.go  prune.go           # image transfer, GC
│   ├── supervisor.go                        # detached-container supervisor
│   ├── netsetup.go                          # --net / -p wiring
│   └── state.go                             # state-root resolution
│
├── internal/
│   ├── builder/    parser.go  build.go  ignore.go
│   ├── cache/      cache.go             # keys, index, flock
│   ├── container/  container.go  process.go  name.go
│   ├── image/      manifest.go  export.go
│   ├── netlink/    socket.go  message.go  link.go  addr.go  route.go
│   ├── network/    bridge.go  ipam.go  nat.go  resolv.go
│   ├── runtime/    isolate.go  mount.go  init.go
│   └── store/      store.go             # layers, tars, whiteouts
│
├── sample-app/                 # demo application
└── setup/import-base-images.sh
```

---

## Building Images

```bash
sudo ./docksmith build -t myapp:latest ./sample-app
sudo ./docksmith build --no-cache -t myapp:latest ./sample-app
```

`RUN` steps execute inside the image filesystem with **no network access** (`--net=none`). This is deliberate: network access during a build would make layers depend on what a remote server returned at the time, which the cache cannot detect — the same Docksmithfile would hit the cache and produce a different image.

### `.docksmithignore`

A gitignore-style file in the build context root excludes paths from `COPY`:

```
# comments and blank lines are ignored
*.log
.git
build/
node_modules
!important.log      # re-include
```

Rules: bare names match by basename at any depth, a trailing `/` covers the whole subtree, `**` crosses directory separators, `!` re-includes, and later rules win. This matters beyond build speed — `COPY` cache keys hash every matched file, so a stray `.git` object silently invalidates downstream steps for a reason you cannot see.

---

## Running Containers

```bash
sudo ./docksmith run myapp:latest                        # image's CMD
sudo ./docksmith run myapp:latest /bin/sh -c 'echo hi'   # override CMD
sudo ./docksmith run -e GREETING=Howdy myapp:latest      # env override
sudo ./docksmith run -v /host/data:/data myapp:latest    # bind mount
sudo ./docksmith run -v /host/cfg:/cfg:ro myapp:latest   # read-only mount
sudo ./docksmith run -d --name web -p 8080:80 myapp:latest
```

Containers **persist after they exit**, like Docker: they stay in `ps -a` with their exit code and logs until removed. Use `--rm` to clean up automatically, or `docksmith prune` to reclaim in bulk.

A container runs with:

- Namespaces: `CLONE_NEWNS`, `NEWPID`, `NEWUTS`, `NEWIPC`, and `NEWNET` unless `--net=host`
- `pivot_root` into the assembled rootfs, with the old root unmounted
- Mount propagation set to private, so container mounts never leak to the host
- Hostname set to the container's short ID
- An init process as PID 1 that forwards signals and reaps orphans

---

## Networking

Three modes, selected with `--net`:

| Mode | Behaviour |
|------|-----------|
| `bridge` (default) | Own netns, veth pair to the `docksmith0` bridge, address from `172.30.0.0/16`, NAT for outbound traffic |
| `none` | Own netns with loopback only |
| `host` | Shares the host's network stack entirely |

On the first bridge container, docksmith creates `docksmith0` (gateway `172.30.0.1`), enables `ip_forward`, and installs MASQUERADE plus FORWARD rules. Each container gets a veth pair; the host end joins the bridge, the other end is moved into the container's netns and renamed `eth0`.

`/etc/resolv.conf`, `/etc/hostname` and `/etc/hosts` are generated into the rootfs. Loopback resolvers are **filtered out** — a host running systemd-resolved advertises `127.0.0.53`, which inside a container points at the container itself where nothing is listening, so copying it across produces silent DNS timeouts.

### Publishing ports

```bash
sudo ./docksmith run -d -p 8080:80 myapp:latest        # host 8080 -> container 80
sudo ./docksmith run -d -p 5353:53/udp myapp:latest    # UDP
sudo ./docksmith run -d -p 8080 myapp:latest           # container port from EXPOSE
```

Published ports are reachable both from the LAN address and from `127.0.0.1`. The localhost path needs more than DNAT: DNAT rewrites only the destination, so the container would see source `127.0.0.1` and send its reply to its own loopback. Docksmith enables `route_localnet` on the bridge and masquerades that traffic so replies can return. (Docker avoids the problem entirely by proxying localhost connections in userspace with `docker-proxy`.)

All rules installed for a container are recorded in its `config.json`, so teardown deletes exactly what was added rather than pattern-matching a ruleset other tools are also editing.

---

## All CLI Commands

### Images

| Command | Description |
|---------|-------------|
| `build -t <name:tag> [--no-cache] <context>` | Build an image from a Docksmithfile |
| `images` | List images |
| `rmi <name:tag>` | Remove an image; deletes only layers no other image references |
| `import <dir-or-tar> <name:tag>` | Import a base image |
| `save [-o out.tar] <name:tag>...` | Export images to a portable archive |
| `load [-i in.tar]` | Import an archive, verifying every digest |

### Containers

| Command | Description |
|---------|-------------|
| `run [opts] <name:tag> [cmd]` | Run a container |
| `ps [-a]` | List containers (`-a` includes exited) |
| `logs [-f] <container>` | Show output (`-f` follows) |
| `stop [-t secs] <container>...` | SIGTERM, then SIGKILL after the timeout (default 10s) |
| `rm [-f] <container>...` | Remove a container (`-f` kills it first) |

`run` options: `-d` detached, `--rm` auto-remove, `--name`, `--net`, `-e KEY=VALUE`, `-v host:ctr[:ro]`, `-p host:ctr[/proto]`. The repeatable flags may be given more than once.

Containers can be referred to by full ID, any unambiguous ID prefix, or name.

### Maintenance

| Command | Description |
|---------|-------------|
| `prune [-f] [--all]` | Reclaim exited containers, unreferenced layers, stale cache entries and IP leases |

`prune` prints what it will remove and how much it will reclaim, then asks for confirmation unless `-f`. `--all` also drops build-cache entries whose layers still exist.

---

## The Docksmithfile Language

Seven instructions.

### `FROM <image>[:<tag>]`
The base image, which must already be in the local store. Must come first. Produces no layer; its manifest digest anchors every subsequent cache key. Environment, working directory and exposed ports are inherited; `CMD` deliberately is not.

### `ENV <key>=<value>`
Sets an environment variable for later `RUN` steps and for the container at runtime. No layer. Only the first `=` separates, so values may contain more.

### `WORKDIR <path>`
Sets the working directory for subsequent instructions and at runtime. No layer. Created if absent.

### `EXPOSE <port>[/tcp|udp] ...`
Records the ports the image listens on. No layer, and deliberately **not** part of any cache key — it cannot change a layer's bytes, so including it would invalidate every existing cache entry on upgrade and buy nothing. Supplies the container port for a bare `-p <hostPort>`.

### `COPY <src> <dest>`
Copies from the build context into the image. Supports globs and `**`. Honours `.docksmithignore`. Produces one delta layer.

### `RUN <command>`
Runs `/bin/sh -c <command>` inside the image filesystem, with the same isolation `docksmith run` uses and no network. Produces a delta layer containing files created, modified **or deleted**.

### `CMD ["exec", "arg"]`
The default command, JSON array form only. No layer. Overridable at runtime.

### Example

```dockerfile
FROM busybox:latest

ENV APP_NAME=docksmith-sample
ENV GREETING=Hello

WORKDIR /app
EXPOSE 8080

COPY src/main.sh /app/main.sh
COPY config/settings.txt /app/settings.txt

RUN echo "build complete" > /app/message.txt

CMD ["/bin/sh", "/app/main.sh"]
```

---

## How the Cache Works

Before each `COPY` or `RUN`, a cache key is the SHA-256 of:

1. A **layer format version** — see below
2. The previous layer's digest (or the base manifest digest)
3. The full instruction text
4. The current `WORKDIR`
5. All accumulated `ENV` pairs, in sorted key order
6. **`COPY` only:** the SHA-256 of each source file, in sorted path order

A hit requires both a matching key *and* the layer file still being present on disk.

**Cascade rule.** Once a step misses, every later step misses regardless of its own key.

**The format version matters.** Key inputs describe the *instruction*, never the layer encoding. Without a version salt, changing how layers are built would silently reuse layers produced by the old code — a `RUN rm` cached before whiteout support existed would recompute the same key, hit, and serve a layer that deletes nothing. Bump `keyFormatVersion` in `internal/cache/cache.go` whenever `BuildTar`'s output changes for the same inputs.

**Concurrency.** The index is a read-modify-write of one file, guarded by a `flock`, and written atomically. Two builds racing cannot lose each other's entries.

---

## State Directory Layout

```
~/.docksmith/
├── images/       <name>_<tag>.json          image manifests
├── layers/       <sha256>.tar               content-addressed layers
├── cache/        index.json  .lock          build cache
├── containers/
│   └── <id>/
│       ├── config.json                      container record
│       ├── rootfs/                          assembled layers
│       └── container.log                    stdout + stderr
└── network/      ipam.json  .lock           address leases
```

---

## Architecture Notes

### Re-exec and the three sentinels

There is no separate runtime binary and no daemon. `main.go` checks `os.Args[1]` before any CLI parsing for three sentinels:

- **`__child__`** — the container itself. The parent sets `Cloneflags` and passes a JSON `ChildConfig`; the child configures its network, mounts, enters the rootfs and runs the command.
- **`__init__` (in-process)** — the container's PID 1. Rather than exec'ing a separate init binary, `ChildMain` *is* already PID 1 in the namespace, so it forks the real command as PID 2 and stays as init.
- **`__supervisor__`** — for `run -d`. Docksmith re-execs itself with `Setsid`, and that copy waits on the container and records its exit status. Without it a detached container would be orphaned with nothing to report its result.

### The FD protocol

The parent hands the child two extra file descriptors:

- **FD 3, error pipe (child → parent).** Set `CLOEXEC`, so a successful `exec` closes it and the parent reads EOF — that is how "setup succeeded" is signalled. A setup failure is written as text instead.
- **FD 4, sync barrier (parent → child).** The child blocks reading one byte before doing anything. This gives the parent a window to do work that requires the child to *already exist* — moving a veth endpoint into its network namespace, which is addressed by pid. The byte is written in **every** mode; a mode that forgot would hang forever.

The parent drains FD 3 in a goroutine. Reading it inline would deadlock: that read blocks until the child execs, and the child is blocked on FD 4 waiting for the parent.

### Isolation

`pivot_root`, not `chroot`. A `chroot` only changes path resolution — a process holding a directory descriptor from outside can walk back out. `pivot_root` swaps the mount itself and the old root is then unmounted, leaving nothing to escape to.

Before any of that, mount propagation on `/` is set to `MS_REC|MS_PRIVATE`. On a systemd host `/` is shared, so without this the container's `/proc` and `/dev` mounts propagate into the host's namespace and outlive the container. It is also required for `pivot_root`, which returns `EINVAL` when the new root's parent is `MS_SHARED`.

If `pivot_root` fails *before* taking effect, docksmith warns and falls back to `chroot`. A failure *after* it is fatal: the process is already pivoted, and a failed unmount would leave the entire host filesystem readable at `/.oldroot`, which is strictly worse than the chroot it replaced.

### PID 1

Container PID 1 is an init loop, not the user's command. Per `pid_namespaces(7)`, a namespace's init receives only signals it has installed a handler for — and that restriction applies to signals from an ancestor namespace too. A shell running as PID 1 therefore *discards* SIGTERM, so `docksmith stop` would fall through to SIGKILL after the full timeout every time. The init also reaps orphans, which would otherwise accumulate as zombies for the container's lifetime.

### Content-addressed layers and whiteouts

Each layer is a tar of only what a step changed, named by the SHA-256 of its own bytes, so identical steps share one file on disk. `RUN` deltas are computed by re-extracting the base layers into a reference directory and diffing.

Deletions need explicit encoding, because a tar can only say "here is a file", never "the file that used to be here is gone". Removing `/app/config.txt` is recorded as a zero-byte entry named `app/.wh.config.txt`, which `ExtractTar` turns back into a removal. This follows the OCI layer spec. The opaque-directory marker (`.wh..wh..opq`) is not needed here, because layers are extracted sequentially into one real directory rather than stacked with overlayfs.

Whiteout entries are written **ahead of all content** rather than relying on sort order. Byte-wise, `.cache` sorts *before* `.wh..cache`, which would break for most real dotfiles.

### Deterministic tars

Entries sorted by path, timestamps zeroed, uid/gid/uname cleared, and a fixed umask during `RUN`. Identical inputs produce byte-identical layers on any machine — which is what makes the content-addressed cache correct rather than merely fast.

### Netlink

`internal/netlink` speaks rtnetlink over an `AF_NETLINK` socket directly: message framing, sequence numbering, `NLMSG_ERROR` decoding, and `rtattr` TLV encoding including nested attributes. Creating a veth pair means `IFLA_LINKINFO` → `IFLA_INFO_DATA` → `VETH_INFO_PEER`, whose payload is a raw `ifinfomsg` followed by attributes. Struct layouts and constants come from the standard library's `syscall` package; only `IFLA_INFO_KIND`, `IFLA_INFO_DATA` and `VETH_INFO_PEER` are defined locally.

The one exception is packet filtering: netfilter rules use a different netlink family with a much larger wire format, so NAT shells out to `iptables`. That is the only place docksmith depends on an external binary, and it is confined to `internal/network/nat.go`.

### Reference counting

`rmi` deletes only layers no surviving image references, and `prune` reuses the same live-set computation. Every image built `FROM` a common base shares that base's layer files, so deleting them unconditionally would destroy the base image and every sibling built on it.

---

## Testing

```bash
make test       # unit tests, no privileges
sudo make demo  # end-to-end, requires root
```

The unit suite exists mainly to pin the invariants everything else rests on: `BuildTar` must produce byte-identical output regardless of input order, host umask or wall-clock time, and `ComputeKey` must be stable across map iteration order while remaining sensitive to every input that can change a layer. Netlink message encoding is asserted at the byte level, including attribute padding.

`internal/runtime` is uncovered by unit tests by design — it needs namespaces and root, and is verified by `make demo` instead.
