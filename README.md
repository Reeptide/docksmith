# Docksmith

A simplified Docker-like build and container runtime system built from scratch in Go. Docksmith implements content-addressed image layers, a deterministic build cache, and Linux process isolation using kernel namespaces and `chroot` — with no dependency on Docker, runc, or any other container runtime.

---

## Table of Contents

1. [Requirements](#requirements)
2. [Project Structure](#project-structure)
3. [Quick Start](#quick-start)
4. [One-Time Setup](#one-time-setup)
5. [Building Images](#building-images)
6. [Running Containers](#running-containers)
7. [All CLI Commands](#all-cli-commands)
8. [The Docksmithfile Language](#the-docksmithfile-language)
9. [How the Cache Works](#how-the-cache-works)
10. [State Directory Layout](#state-directory-layout)
11. [Full Demo Walkthrough](#full-demo-walkthrough)
12. [Architecture Notes](#architecture-notes)

---

## Requirements

- **OS:** Linux (Ubuntu 24.04 recommended). macOS and Windows are NOT supported — Linux kernel namespaces are required.
- **Go:** 1.22 or later (`sudo apt-get install -y golang-go`)
- **busybox-static:** Required for the base image (`sudo apt-get install -y busybox-static`)
- **Root / sudo:** `docksmith run` and `RUN` during build use `CLONE_NEWNS`, `CLONE_NEWPID`, `CLONE_NEWUTS`, and `chroot`, which require either root or a kernel configured to allow unprivileged user namespaces. On Ubuntu 24 running as root (e.g. in CI), this works out of the box.

---

## Project Structure

```
docksmith/
├── main.go                        # CLI entry point + container child re-exec handler
├── go.mod
├── Makefile
│
├── cmd/                           # CLI subcommands
│   ├── build.go                   # docksmith build
│   ├── images.go                  # docksmith images
│   ├── import.go                  # docksmith import (base image setup)
│   ├── rmi.go                     # docksmith rmi
│   └── run.go                     # docksmith run
│
├── internal/
│   ├── builder/
│   │   ├── parser.go              # Docksmithfile parser
│   │   └── build.go               # Build engine: COPY/RUN/layer assembly/cache
│   ├── cache/
│   │   └── cache.go               # Cache key computation and index
│   ├── image/
│   │   └── manifest.go            # Image manifest type, load/save, digest
│   ├── runtime/
│   │   └── isolate.go             # Linux namespace isolation (chroot + namespaces)
│   └── store/
│       └── store.go               # Content-addressed layer storage, tar build/extract
│
└── sample-app/                    # Demo application
    ├── Docksmithfile              # Uses all 6 instructions
    ├── src/
    │   └── main.sh                # Application entrypoint
    └── config/
        └── settings.txt           # App config file
│
└── setup/
    └── import-base-images.sh      # One-time base image import script
```

---

## Quick Start

```bash
# 1. Clone / unzip the project
cd docksmith

# 2. Install system dependencies (if not already installed)
sudo apt-get update
sudo apt-get install -y golang-go busybox-static

# 3. Build the binary
go build -o docksmith .

# 4. Import the base image (one-time setup, requires no internet during build/run)
./setup/import-base-images.sh

# 5. Build the sample app
./docksmith build -t myapp:latest ./sample-app

# 6. Run the sample app
./docksmith run myapp:latest

# 7. Run with an environment variable override
./docksmith run -e GREETING=Howdy myapp:latest
```

---

## One-Time Setup

All base images must be imported into `~/.docksmith/` **before** any build. Nothing is downloaded during build or run — everything runs fully offline after setup.

```bash
# Build the binary first
go build -o docksmith .

# Import busybox:latest (builds a minimal static rootfs from your system's busybox-static)
./setup/import-base-images.sh
```

The script:
1. Finds `busybox-static` on your system
2. Builds a minimal Linux rootfs (bin, sbin, etc, proc, dev, tmp...)
3. Imports it as `busybox:latest` into `~/.docksmith/images/` and `~/.docksmith/layers/`

You can also import any directory or tar archive manually:

```bash
./docksmith import /path/to/rootfs-dir  mybase:latest
./docksmith import /path/to/image.tar   mybase:v1
./docksmith import /path/to/image.tar.gz mybase:v1
```

---

## Building Images

```bash
# Basic build (context directory = .)
./docksmith build -t myapp:latest .

# Explicit context directory
./docksmith build -t myapp:latest ./sample-app

# Skip cache entirely
./docksmith build --no-cache -t myapp:latest ./sample-app
```

**Build output format:**
```
Step 1/9 : FROM busybox:latest
Step 2/9 : ENV APP_NAME=docksmith-sample
Step 3/9 : ENV APP_ENV=production
Step 4/9 : ENV GREETING=Hello
Step 5/9 : WORKDIR /app
Step 6/9 : COPY src/main.sh /app/main.sh [CACHE MISS] 0.00s
Step 7/9 : COPY config/settings.txt /app/settings.txt [CACHE MISS] 0.00s
Step 8/9 : RUN echo "Built at: $(date)" > /app/message.txt [CACHE MISS] 1.50s
Step 9/9 : CMD ["/bin/sh", "/app/main.sh"]
Successfully built sha256:98bd2f525812 myapp:latest (1.51s)
```

- `FROM` never shows cache status (not a layer-producing step)
- `ENV`, `WORKDIR`, `CMD` never show cache status (not layer-producing)
- `COPY` and `RUN` show `[CACHE HIT]` or `[CACHE MISS]` with timing
- Any `CACHE MISS` cascades all subsequent steps to also be misses

---

## Running Containers

```bash
# Run using the image's CMD
./docksmith run myapp:latest

# Override CMD
./docksmith run myapp:latest /bin/sh -c 'echo hello'

# Override environment variable
./docksmith run -e GREETING=Howdy myapp:latest

# Multiple overrides
./docksmith run -e GREETING=Hi -e APP_ENV=staging myapp:latest
```

The container process runs with:
- Full Linux namespace isolation (`CLONE_NEWNS`, `CLONE_NEWPID`, `CLONE_NEWUTS`, `CLONE_NEWIPC`)
- `chroot` into the assembled image rootfs
- No access to the host filesystem
- Image `ENV` values injected (with `-e` overrides taking precedence)
- `WorkingDir` set as working directory (defaults to `/`)

**Files written inside a container are NOT visible on the host.** This is enforced by the chroot + mount namespace isolation.

---

## All CLI Commands

### `docksmith build`

```
docksmith build -t <name:tag> [--no-cache] <context-dir>
```

Parses the `Docksmithfile` in `<context-dir>`, executes all instructions, writes layers and a manifest to `~/.docksmith/`.

| Flag | Description |
|------|-------------|
| `-t <name:tag>` | **Required.** Name and tag for the resulting image |
| `--no-cache` | Skip all cache lookups. Layers are still written to disk. |

---

### `docksmith images`

```
docksmith images
```

Lists all images in the local store. Columns: `NAME`, `TAG`, `ID` (first 12 chars of digest), `CREATED`.

---

### `docksmith rmi`

```
docksmith rmi <name:tag>
```

Removes the image manifest and **all layer files** referenced by that image. No reference counting is performed — if another image shares a layer, that layer file is gone and that image is broken. This is expected behaviour per spec.

---

### `docksmith run`

```
docksmith run [-e KEY=VALUE] <name:tag> [cmd [args...]]
```

Assembles the image rootfs, starts the container in the foreground (blocking until exit), then cleans up the temp directory.

| Flag | Description |
|------|-------------|
| `-e KEY=VALUE` | Override or add an environment variable. Repeatable. |
| `[cmd]` | Override the image's `CMD`. Entire remainder of args is used. |

---

### `docksmith import`

```
docksmith import <dir-or-tar> <name:tag>
```

Imports a directory tree or tar archive as a base image. Used during initial setup. Supports `.tar` and `.tar.gz` archives.

---

## The Docksmithfile Language

Place a file named `Docksmithfile` in your build context directory. Six instructions are supported:

### `FROM <image>[:<tag>]`

```dockerfile
FROM busybox:latest
```

Loads the named image from the local store as the base filesystem. Fails immediately with a clear error if the image does not exist. Must be the first instruction. Does not produce a layer. Uses the base manifest's digest as the anchor for all subsequent cache keys.

---

### `ENV <key>=<value>`

```dockerfile
ENV APP_NAME=myapp
ENV LOG_LEVEL=info
```

Stores an environment variable in the image config. Injected into every `RUN` command during build and into every container at runtime. Does not produce a layer. Affects cache keys of all subsequent `COPY` and `RUN` steps.

---

### `WORKDIR <path>`

```dockerfile
WORKDIR /app
```

Sets the working directory for subsequent instructions and for the container at runtime. Does not produce a layer. If the path does not exist in any previously extracted layer, the build engine creates it silently before the next layer-producing instruction. Affects cache keys of all subsequent `COPY` and `RUN` steps.

---

### `COPY <src> <dest>`

```dockerfile
COPY src/main.sh /app/main.sh
COPY config/settings.txt /app/settings.txt
```

Copies files from the build context into the image. Supports `*` and `**` globs. Creates missing destination directories automatically. Produces a layer (a tar delta containing only the copied files). Relative `dest` paths are resolved relative to `WORKDIR`.

---

### `RUN <command>`

```dockerfile
RUN echo "hello" > /app/message.txt
RUN chmod +x /app/entrypoint.sh
```

Executes a shell command **inside the assembled image filesystem** — not on the host. Uses `/bin/sh -c` as the shell. Produces a delta layer containing only the files that were created or modified. The exact same Linux namespace isolation used by `docksmith run` is used here.

**Important:** Commands must not require network access. All dependencies must be present in the build context or a prior layer.

---

### `CMD ["exec", "arg1", "arg2"]`

```dockerfile
CMD ["/bin/sh", "/app/main.sh"]
```

Sets the default command run when the container starts. Must be a JSON array. Does not produce a layer. Can be overridden at runtime with `docksmith run <image> <cmd>`.

---

### Complete Example

```dockerfile
FROM busybox:latest

ENV APP_NAME=myapp
ENV APP_ENV=production
ENV GREETING=Hello

WORKDIR /app

COPY src/main.sh /app/main.sh
COPY config/settings.txt /app/settings.txt

RUN echo "Built at: $(date)" > /app/message.txt

CMD ["/bin/sh", "/app/main.sh"]
```

---

## How the Cache Works

Before each `COPY` or `RUN` step, a **cache key** is computed as a SHA-256 hash of:

1. The digest of the previous layer (or the base image manifest digest for the very first layer-producing step)
2. The full instruction text as written in the Docksmithfile
3. The current `WORKDIR` value at the time the instruction is reached
4. All accumulated `ENV` key=value pairs, serialised in **lexicographically sorted key order**
5. **COPY only:** SHA-256 of each source file's bytes, in **lexicographically sorted path order**

A cache **hit** requires both: (a) the key matches an entry in the cache index AND (b) the referenced layer file is present on disk.

**Cascade rule:** Once any step is a cache miss, all subsequent steps are also misses regardless of their individual keys.

**Reproducible builds:** Tar entries are added in sorted path order with all file timestamps zeroed, so identical inputs always produce identical layer digests.

**Preserved `created` timestamp:** When all steps are cache hits, the manifest is rewritten with the original `created` value so the manifest digest is identical across warm rebuilds.

---

## State Directory Layout

```
~/.docksmith/
├── images/
│   ├── busybox_latest.json        # Image manifest for busybox:latest
│   └── myapp_latest.json          # Image manifest for myapp:latest
├── layers/
│   ├── 4f8807adf5c3...tar         # Content-addressed tar (base image layer)
│   ├── a1b2c3d4e5f6...tar         # COPY layer (delta)
│   └── f6e5d4c3b2a1...tar         # RUN layer (delta)
└── cache/
    └── index.json                 # Maps cache keys → layer digests
```

Each layer is a tar archive named by the SHA-256 of its raw bytes. Layers are immutable once written. The manifest digest is computed over the manifest JSON with the `digest` field set to `""`.

---

## Full Demo Walkthrough

This walks through all 8 demo scenarios from the spec.

```bash
# Setup (one time)
go build -o docksmith .
./setup/import-base-images.sh
```

### Demo 1 — Cold build (all CACHE MISS)

```bash
./docksmith build -t myapp:latest ./sample-app
```

Expected: Every `COPY` and `RUN` step shows `[CACHE MISS]`. Total build time printed.

---

### Demo 2 — Warm build (all CACHE HIT)

```bash
./docksmith build -t myapp:latest ./sample-app
```

Expected: Every `COPY` and `RUN` step shows `[CACHE HIT]`. Completes near-instantly.

---

### Demo 3 — Edit a source file, partial cache invalidation

```bash
echo "# changed" >> sample-app/config/settings.txt
./docksmith build -t myapp:latest ./sample-app
```

Expected: The `COPY src/main.sh` step is still `[CACHE HIT]` (unchanged). The `COPY config/settings.txt` step and the `RUN` step below it are `[CACHE MISS]`.

---

### Demo 4 — List images

```bash
./docksmith images
```

Expected output (columns: NAME, TAG, ID, CREATED):
```
NAME                 TAG        ID             CREATED
busybox              latest     4f8807adf5c3   2026-04-11T03:11:37Z
myapp                latest     98bd2f525812   2026-04-11T03:11:38Z
```

---

### Demo 5 — Run container

```bash
./docksmith run myapp:latest
```

Expected: Container starts, prints app output showing ENV vars and working directory, exits cleanly.

---

### Demo 6 — Run with env override

```bash
./docksmith run -e GREETING=Howdy myapp:latest
```

Expected: `GREETING = Howdy` appears in the output instead of `Hello`.

---

### Demo 7 — Isolation test (PASS/FAIL)

```bash
./docksmith run myapp:latest /bin/sh -c 'echo SECRET > /tmp/escape-test.txt'
ls /tmp/escape-test.txt 2>/dev/null && echo "FAIL" || echo "PASS"
```

Expected: `PASS` — the file written inside the container does not appear on the host.

---

### Demo 8 — Remove image

```bash
./docksmith rmi myapp:latest
./docksmith images
```

Expected: `myapp:latest` is gone. Its layer files are removed from `~/.docksmith/layers/`.

---

## Architecture Notes

### Isolation Mechanism

Docksmith uses the **re-exec pattern**: when a container or `RUN` step needs to execute, the docksmith binary re-invokes itself with `os.Args[0]` plus a `__child__` sentinel argument. The parent process starts the child with `syscall.SysProcAttr.Cloneflags` set to:

```
CLONE_NEWNS | CLONE_NEWPID | CLONE_NEWUTS | CLONE_NEWIPC
```

The child detects the `__DOCKSMITH_CHILD__=1` environment variable, performs `chroot` into the assembled rootfs, `chdir` to the working directory, then `exec`s the target binary. This is the same mechanism used for both `RUN` during build and `docksmith run` — one primitive, two places.

### Content-Addressed Layers

Every layer is a tar archive of only the files added or changed by that step (a delta, not a full snapshot). The file is named `<sha256-of-tar-bytes>.tar`. The same content always produces the same digest, so identical steps share one file on disk.

For `RUN` deltas: Docksmith re-extracts the base layers into a reference directory, then walks the post-execution rootfs and includes only files whose content differs from the reference.

### Deterministic Tars

All tar archives are built with:
- Entries sorted by path (lexicographic order)
- All file timestamps set to zero (`time.Time{}`)

This guarantees byte-for-byte identical output given the same inputs, which is required for the cache to be correct across rebuilds.
