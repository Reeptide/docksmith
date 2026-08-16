package builder

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"docksmith/internal/cache"
	"docksmith/internal/image"
	"docksmith/internal/store"
)

// newTestState builds a store rooted in a temp dir. snapshotDelta needs no
// privileges — it only walks directories, hashes bytes, and builds a tar — so
// these run under plain `go test`.
func newTestState(t *testing.T) *store.State {
	t.Helper()
	st, err := store.NewState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// writeBaseLayer stores files as a layer and returns the manifest entry.
func writeBaseLayer(t *testing.T, st *store.State, files []store.TarFile) image.LayerEntry {
	t.Helper()
	data, err := store.BuildTar(files)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := st.WriteLayer(data)
	if err != nil {
		t.Fatal(err)
	}
	return image.LayerEntry{Digest: digest, Size: int64(len(data)), CreatedBy: "test"}
}

// extractDelta applies a delta on top of the base and returns the result dir,
// which is what the runtime would actually see.
func extractDelta(t *testing.T, st *store.State, base []image.LayerEntry, delta []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := extractLayers(base, st, dir); err != nil {
		t.Fatal(err)
	}
	if err := store.ExtractTar(delta, dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The bug this step fixes: a RUN that deletes a file used to produce a delta
// that deleted nothing, so the file came back when layers were reassembled.
func TestSnapshotDeltaRecordsDeletions(t *testing.T) {
	st := newTestState(t)
	base := []image.LayerEntry{writeBaseLayer(t, st, []store.TarFile{
		{Path: "app/", Mode: 0755, IsDir: true},
		{Path: "app/keep.txt", Mode: 0644, Content: []byte("keep")},
		{Path: "app/gone.txt", Mode: 0644, Content: []byte("gone")},
	})}

	// Simulate the post-RUN rootfs: same as base, minus gone.txt.
	rootfs := t.TempDir()
	if err := extractLayers(base, st, rootfs); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(rootfs, "app/gone.txt")); err != nil {
		t.Fatal(err)
	}

	delta, err := snapshotDelta(rootfs, base, st)
	if err != nil {
		t.Fatal(err)
	}

	got := extractDelta(t, st, base, delta)
	if _, err := os.Lstat(filepath.Join(got, "app/gone.txt")); !os.IsNotExist(err) {
		t.Errorf("deleted file reappeared after reassembly (err=%v)", err)
	}
	if _, err := os.Lstat(filepath.Join(got, "app/keep.txt")); err != nil {
		t.Errorf("untouched file went missing: %v", err)
	}
}

func TestSnapshotDeltaRecordsDirectoryDeletion(t *testing.T) {
	st := newTestState(t)
	base := []image.LayerEntry{writeBaseLayer(t, st, []store.TarFile{
		{Path: "data/", Mode: 0755, IsDir: true},
		{Path: "data/a.txt", Mode: 0644, Content: []byte("a")},
		{Path: "data/nested/", Mode: 0755, IsDir: true},
		{Path: "data/nested/b.txt", Mode: 0644, Content: []byte("b")},
		{Path: "other.txt", Mode: 0644, Content: []byte("other")},
	})}

	rootfs := t.TempDir()
	if err := extractLayers(base, st, rootfs); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(rootfs, "data")); err != nil {
		t.Fatal(err)
	}

	delta, err := snapshotDelta(rootfs, base, st)
	if err != nil {
		t.Fatal(err)
	}

	got := extractDelta(t, st, base, delta)
	if _, err := os.Lstat(filepath.Join(got, "data")); !os.IsNotExist(err) {
		t.Errorf("deleted directory reappeared after reassembly (err=%v)", err)
	}
	if _, err := os.Lstat(filepath.Join(got, "other.txt")); err != nil {
		t.Errorf("unrelated file went missing: %v", err)
	}
}

// Additions and modifications must keep working, and an unchanged tree must
// still produce an empty delta — otherwise every RUN would churn its layer.
func TestSnapshotDeltaAddsModifiesAndIgnoresUnchanged(t *testing.T) {
	st := newTestState(t)
	base := []image.LayerEntry{writeBaseLayer(t, st, []store.TarFile{
		{Path: "app/", Mode: 0755, IsDir: true},
		{Path: "app/same.txt", Mode: 0644, Content: []byte("same")},
		{Path: "app/edit.txt", Mode: 0644, Content: []byte("before")},
	})}

	rootfs := t.TempDir()
	if err := extractLayers(base, st, rootfs); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "app/edit.txt"), []byte("after"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "app/new.txt"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}

	delta, err := snapshotDelta(rootfs, base, st)
	if err != nil {
		t.Fatal(err)
	}
	got := extractDelta(t, st, base, delta)

	for path, want := range map[string]string{
		"app/edit.txt": "after",
		"app/new.txt":  "new",
		"app/same.txt": "same",
	} {
		data, err := os.ReadFile(filepath.Join(got, path))
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if string(data) != want {
			t.Errorf("%s = %q, want %q", path, data, want)
		}
	}
}

func TestSnapshotDeltaEmptyWhenNothingChanged(t *testing.T) {
	st := newTestState(t)
	base := []image.LayerEntry{writeBaseLayer(t, st, []store.TarFile{
		{Path: "app/", Mode: 0755, IsDir: true},
		{Path: "app/a.txt", Mode: 0644, Content: []byte("a")},
	})}

	rootfs := t.TempDir()
	if err := extractLayers(base, st, rootfs); err != nil {
		t.Fatal(err)
	}

	delta, err := snapshotDelta(rootfs, base, st)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := store.BuildTar(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta) != len(empty) {
		t.Errorf("unchanged rootfs produced a %d-byte delta, want the %d-byte empty tar", len(delta), len(empty))
	}
}

// A symlink whose target does not exist must not be mistaken for a deletion.
// os.Stat would follow it and report ENOENT; os.Lstat is required. A busybox
// rootfs is almost entirely symlinks, so getting this wrong would whiteout
// most of the base image.
func TestSnapshotDeltaDoesNotWhiteoutDanglingSymlinks(t *testing.T) {
	st := newTestState(t)
	base := []image.LayerEntry{writeBaseLayer(t, st, []store.TarFile{
		{Path: "bin/", Mode: 0755, IsDir: true},
		{Path: "bin/busybox", Mode: 0755, Content: []byte("binary")},
		{Path: "bin/ls", Mode: 0777, IsSymlink: true, Linkname: "/bin/busybox"},
	})}

	rootfs := t.TempDir()
	if err := extractLayers(base, st, rootfs); err != nil {
		t.Fatal(err)
	}

	delta, err := snapshotDelta(rootfs, base, st)
	if err != nil {
		t.Fatal(err)
	}
	got := extractDelta(t, st, base, delta)

	// The absolute symlink target resolves inside the host, not the rootfs, so
	// it dangles here — exactly the case that trips os.Stat.
	if _, err := os.Lstat(filepath.Join(got, "bin/ls")); err != nil {
		t.Errorf("dangling symlink was wrongly whited out: %v", err)
	}
}

// A RUN that creates a symlink must produce a symlink in the layer.
//
// The delta walk used to read every entry with os.ReadFile, which follows
// links, so `ln -s /bin/busybox /bin/ls` was recorded as a full copy of
// busybox's bytes: the layer grew by the target's size per link and the
// indirection was lost entirely, which for a busybox image means every applet
// becomes an independent megabyte-sized binary.
func TestSnapshotDeltaPreservesSymlinksCreatedByRun(t *testing.T) {
	st := newTestState(t)
	base := []image.LayerEntry{writeBaseLayer(t, st, []store.TarFile{
		{Path: "bin/", Mode: 0755, IsDir: true},
		{Path: "bin/busybox", Mode: 0755, Content: []byte("ELF-ish payload")},
	})}

	rootfs := t.TempDir()
	if err := extractLayers(base, st, rootfs); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/busybox", filepath.Join(rootfs, "bin/ls")); err != nil {
		t.Fatal(err)
	}

	delta, err := snapshotDelta(rootfs, base, st)
	if err != nil {
		t.Fatal(err)
	}

	got := extractDelta(t, st, base, delta)
	link := filepath.Join(got, "bin/ls")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("bin/ls came back as a %v, not a symlink", info.Mode().Type())
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if target != "/bin/busybox" {
		t.Errorf("symlink target = %q, want /bin/busybox", target)
	}
}

// A symlink inherited unchanged from the base must not appear in the delta at
// all. Following it would hash the target's bytes, so any change to the target
// would also rewrite every link pointing at it.
func TestSnapshotDeltaIgnoresUnchangedSymlinks(t *testing.T) {
	st := newTestState(t)
	base := []image.LayerEntry{writeBaseLayer(t, st, []store.TarFile{
		{Path: "bin/", Mode: 0755, IsDir: true},
		{Path: "bin/busybox", Mode: 0755, Content: []byte("payload")},
		{Path: "bin/sh", Mode: 0777, IsSymlink: true, Linkname: "/bin/busybox"},
		{Path: "marker.txt", Mode: 0644, Content: []byte("x")},
	})}

	rootfs := t.TempDir()
	if err := extractLayers(base, st, rootfs); err != nil {
		t.Fatal(err)
	}
	// Touch something unrelated so the delta is non-empty for a real reason.
	if err := os.WriteFile(filepath.Join(rootfs, "new.txt"), []byte("n"), 0644); err != nil {
		t.Fatal(err)
	}

	delta, err := snapshotDelta(rootfs, base, st)
	if err != nil {
		t.Fatal(err)
	}
	names := tarEntryNames(t, delta)
	for _, n := range names {
		if n == "bin/sh" {
			t.Errorf("unchanged symlink was re-recorded in the delta: %v", names)
		}
	}
	if !containsName(names, "new.txt") {
		t.Errorf("the genuinely new file is missing from the delta: %v", names)
	}
}

// A FIFO left behind by a RUN step used to hang the build forever: os.ReadFile
// on a FIFO blocks until a writer appears, which inside a finished build is
// never, and there is no timeout anywhere in the pipeline.
//
// The test would hang rather than fail against the old code, so it runs the
// snapshot on a goroutine and fails on a deadline instead.
func TestSnapshotDeltaSkipsFIFOsInsteadOfBlocking(t *testing.T) {
	st := newTestState(t)
	base := []image.LayerEntry{writeBaseLayer(t, st, []store.TarFile{
		{Path: "var/", Mode: 0755, IsDir: true},
	})}

	rootfs := t.TempDir()
	if err := extractLayers(base, st, rootfs); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(rootfs, "var/pipe")
	if err := syscall.Mkfifo(fifo, 0644); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}

	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := snapshotDelta(rootfs, base, st)
		done <- result{data, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if containsName(tarEntryNames(t, r.data), "var/pipe") {
			t.Error("a FIFO was recorded in the layer; it is not representable")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("snapshotDelta blocked on a FIFO")
	}
}

// A RUN that replaces a file with a directory (or the reverse) must whiteout
// the old entry first. Without it the delta carries a directory entry for a
// path that a lower layer still holds as a regular file, and assembly fails —
// the build succeeds and the image is unusable.
func TestSnapshotDeltaWhiteoutsTypeChanges(t *testing.T) {
	cases := []struct {
		name     string
		base     []store.TarFile
		mutate   func(t *testing.T, rootfs string)
		checkDir bool // expect a directory at conf after reassembly
	}{
		{
			name: "file becomes directory",
			base: []store.TarFile{{Path: "conf", Mode: 0644, Content: []byte("old")}},
			mutate: func(t *testing.T, rootfs string) {
				p := filepath.Join(rootfs, "conf")
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(p, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(p, "a.txt"), []byte("a"), 0644); err != nil {
					t.Fatal(err)
				}
			},
			checkDir: true,
		},
		{
			name: "directory becomes file",
			base: []store.TarFile{
				{Path: "conf/", Mode: 0755, IsDir: true},
				{Path: "conf/a.txt", Mode: 0644, Content: []byte("a")},
			},
			mutate: func(t *testing.T, rootfs string) {
				p := filepath.Join(rootfs, "conf")
				if err := os.RemoveAll(p); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("now a file"), 0644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newTestState(t)
			base := []image.LayerEntry{writeBaseLayer(t, st, c.base)}

			rootfs := t.TempDir()
			if err := extractLayers(base, st, rootfs); err != nil {
				t.Fatal(err)
			}
			c.mutate(t, rootfs)

			delta, err := snapshotDelta(rootfs, base, st)
			if err != nil {
				t.Fatal(err)
			}

			got := extractDelta(t, st, base, delta)
			info, err := os.Lstat(filepath.Join(got, "conf"))
			if err != nil {
				t.Fatalf("conf is missing after reassembly: %v", err)
			}
			if info.IsDir() != c.checkDir {
				t.Fatalf("conf came back as IsDir=%v, want %v", info.IsDir(), c.checkDir)
			}
			if c.checkDir {
				if _, err := os.Stat(filepath.Join(got, "conf/a.txt")); err != nil {
					t.Errorf("new directory contents missing: %v", err)
				}
			} else {
				data, err := os.ReadFile(filepath.Join(got, "conf"))
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != "now a file" {
					t.Errorf("conf = %q", data)
				}
			}
		})
	}
}

// tarEntryNames lists the entry names in a layer, whiteout markers included.
func tarEntryNames(t *testing.T, data []byte) []string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, strings.TrimSuffix(hdr.Name, "/"))
	}
	return names
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// A RUN that changes only permissions must still produce a layer.
//
// `RUN chmod +x /app/entrypoint.sh` changes no bytes, so a delta that compared
// content alone came out empty: the build reported a cache miss, wrote a layer
// that did nothing, and the container failed at runtime with "permission
// denied" and no diagnostic anywhere in the build output.
func TestSnapshotDeltaRecordsPermissionOnlyChanges(t *testing.T) {
	st := newTestState(t)
	base := []image.LayerEntry{writeBaseLayer(t, st, []store.TarFile{
		{Path: "app/", Mode: 0755, IsDir: true},
		{Path: "app/entrypoint.sh", Mode: 0644, Content: []byte("#!/bin/sh\necho hi\n")},
	})}

	rootfs := t.TempDir()
	if err := extractLayers(base, st, rootfs); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(rootfs, "app/entrypoint.sh"), 0755); err != nil {
		t.Fatal(err)
	}

	delta, err := snapshotDelta(rootfs, base, st)
	if err != nil {
		t.Fatal(err)
	}
	if !containsName(tarEntryNames(t, delta), "app/entrypoint.sh") {
		t.Fatal("a chmod-only RUN produced a layer that does not mention the file")
	}

	got := extractDelta(t, st, base, delta)
	info, err := os.Stat(filepath.Join(got, "app/entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("mode after reassembly = %v, want -rwxr-xr-x", info.Mode())
	}
}

// A directory's mode is part of its identity too: a RUN that locks down a
// directory it inherited must not have that silently reverted.
func TestSnapshotDeltaRecordsDirectoryPermissionChanges(t *testing.T) {
	st := newTestState(t)
	base := []image.LayerEntry{writeBaseLayer(t, st, []store.TarFile{
		{Path: "secrets/", Mode: 0755, IsDir: true},
	})}

	rootfs := t.TempDir()
	if err := extractLayers(base, st, rootfs); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(rootfs, "secrets"), 0700); err != nil {
		t.Fatal(err)
	}

	delta, err := snapshotDelta(rootfs, base, st)
	if err != nil {
		t.Fatal(err)
	}
	got := extractDelta(t, st, base, delta)
	info, err := os.Stat(filepath.Join(got, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Errorf("directory mode after reassembly = %v, want drwx------", info.Mode())
	}
}

// COPY's trailing slash is the only thing distinguishing "put it in this
// directory" from "rename it to this". filepath.Join strips it, so joining
// WORKDIR before reading it silently turned the former into the latter.
func TestBuildCopyTarKeepsDirectorySemanticsUnderWorkdir(t *testing.T) {
	files := []srcFile{{HostPath: writeTemp(t, "x"), RelPath: "main.sh"}}

	cases := []struct {
		workDir, dest, want string
	}{
		{"", "bin/", "bin/main.sh"},
		{"/work", "bin/", "work/bin/main.sh"},
		{"/work", "bin", "work/bin"},
		{"/work", "/opt/bin/", "opt/bin/main.sh"},
	}
	for _, c := range cases {
		tf, err := buildCopyTar(files, c.dest, c.workDir)
		if err != nil {
			t.Fatal(err)
		}
		if !copyTarHas(tf, c.want) {
			t.Errorf("WORKDIR %q, COPY main.sh %s -> %v, want %s",
				c.workDir, c.dest, copyTarPaths(tf), c.want)
		}
	}
}

// Adding a second file to a source directory must not move where the first one
// lands. Deciding on len(files) alone meant it did.
func TestBuildCopyTarPlacementDoesNotDependOnSourceCount(t *testing.T) {
	one := []srcFile{{HostPath: writeTemp(t, "a"), RelPath: "src/a.txt", FromDir: true}}
	two := append([]srcFile{}, one...)
	two = append(two, srcFile{HostPath: writeTemp(t, "b"), RelPath: "src/b.txt", FromDir: true})

	tfOne, err := buildCopyTar(one, "/dest", "")
	if err != nil {
		t.Fatal(err)
	}
	tfTwo, err := buildCopyTar(two, "/dest", "")
	if err != nil {
		t.Fatal(err)
	}
	if !copyTarHas(tfOne, "dest/src/a.txt") || !copyTarHas(tfTwo, "dest/src/a.txt") {
		t.Errorf("a.txt landed at %v with one source and %v with two",
			copyTarPaths(tfOne), copyTarPaths(tfTwo))
	}
}

// A named pipe in the build context used to wedge COPY forever: os.ReadFile
// blocks on a FIFO until a writer appears, and inside a build that is never.
// The build hung before it even consulted the cache, so neither --no-cache nor
// re-running helped.
func TestCollectGlobSkipsNamedPipes(t *testing.T) {
	ctx := t.TempDir()
	if err := os.WriteFile(filepath.Join(ctx, "real.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(ctx, "pipe"), 0644); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}

	done := make(chan []srcFile, 1)
	go func() {
		files, err := collectGlob(ctx, "*", &IgnoreList{})
		if err != nil {
			done <- nil
			return
		}
		if _, err := buildCopyTar(files, "/app/", ""); err != nil {
			done <- nil
			return
		}
		done <- files
	}()

	select {
	case files := <-done:
		for _, f := range files {
			if f.RelPath == "pipe" {
				t.Error("a FIFO was collected as a COPY source")
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("COPY blocked on a FIFO in the build context")
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func copyTarPaths(files []store.TarFile) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func copyTarHas(files []store.TarFile, want string) bool {
	for _, f := range files {
		if f.Path == want {
			return true
		}
	}
	return false
}

// COPY placement, driven through the real collectGlob rather than hand-built
// srcFiles. Constructing the inputs by hand is how a wrong assumption about
// what the collector produces gets baked into both the code and its test: the
// "is the source a directory" decision cannot be recovered from RelPath, since
// `COPY config/settings.txt` and `COPY config` both yield paths containing a
// separator while meaning opposite things for the destination.
func TestCopyPlacementThroughCollectGlob(t *testing.T) {
	ctx := t.TempDir()
	mustMkdir(t, filepath.Join(ctx, "config"))
	mustWrite(t, filepath.Join(ctx, "config/settings.txt"), "setting=1")
	mustMkdir(t, filepath.Join(ctx, "solo"))
	mustWrite(t, filepath.Join(ctx, "solo/only.txt"), "x")
	mustWrite(t, filepath.Join(ctx, "top.txt"), "y")

	cases := []struct {
		name          string
		pattern, dest string
		workDir       string
		wantFile      string
		wantNotADirAt string
	}{
		{
			name:          "named file to an explicit path is a rename",
			pattern:       "config/settings.txt",
			dest:          "/app/settings.txt",
			wantFile:      "app/settings.txt",
			wantNotADirAt: "app/settings.txt",
		},
		{
			name:     "named file into a directory keeps its base name",
			pattern:  "config/settings.txt",
			dest:     "/app/",
			wantFile: "app/config/settings.txt",
		},
		{
			name:     "matched directory with one file is directory-style",
			pattern:  "solo",
			dest:     "/app",
			wantFile: "app/solo/only.txt",
		},
		{
			name:     "named file under WORKDIR with a trailing slash",
			pattern:  "top.txt",
			dest:     "bin/",
			workDir:  "/work",
			wantFile: "work/bin/top.txt",
		},
		{
			name:          "named file under WORKDIR without a trailing slash",
			pattern:       "top.txt",
			dest:          "bin",
			workDir:       "/work",
			wantFile:      "work/bin",
			wantNotADirAt: "work/bin",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files, err := collectGlob(ctx, c.pattern, &IgnoreList{})
			if err != nil {
				t.Fatal(err)
			}
			if len(files) == 0 {
				t.Fatalf("pattern %q matched nothing", c.pattern)
			}
			tf, err := buildCopyTar(files, c.dest, c.workDir)
			if err != nil {
				t.Fatal(err)
			}
			if !copyTarHas(tf, c.wantFile) {
				t.Errorf("COPY %s %s (WORKDIR %q) -> %v, want a file at %s",
					c.pattern, c.dest, c.workDir, copyTarPaths(tf), c.wantFile)
			}
			if c.wantNotADirAt != "" {
				// The regression that broke every build: treating a named file
				// as a directory source turned /app/settings.txt into a
				// directory, so the next `RUN rm /app/settings.txt` failed with
				// "is a directory" and the build died.
				for _, f := range tf {
					if f.IsDir && strings.TrimSuffix(f.Path, "/") == c.wantNotADirAt {
						t.Errorf("%s was emitted as a directory: %v",
							c.wantNotADirAt, copyTarPaths(tf))
					}
				}
			}
		})
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// A chmod in the build context must invalidate the COPY step that reads it.
//
// COPY layers carry permission bits, so `chmod +x deploy.sh` changes the layer
// — but it changes no bytes, so a content-only cache key was identical, the
// step reported a CACHE HIT and the old layer with the old mode was served
// forever. The same failure that made chmod-only RUN steps vanish, one
// instruction over.
func TestCopyCacheKeyChangesWhenSourceModeChanges(t *testing.T) {
	ctx := t.TempDir()
	src := filepath.Join(ctx, "deploy.sh")
	mustWrite(t, src, "#!/bin/sh\n")

	// Through the production helper, not a reimplementation of it: recomputing
	// the expression here and comparing it to itself would pass whether or not
	// execCOPY still folds the mode in.
	key := func() string {
		files, err := collectGlob(ctx, "deploy.sh", &IgnoreList{})
		if err != nil {
			t.Fatal(err)
		}
		sums, err := copyFileSums(files)
		if err != nil {
			t.Fatal(err)
		}
		return cache.ComputeKey(cache.KeyParams{
			Instruction: "COPY deploy.sh /app/deploy.sh",
			FileSums:    sums,
		})
	}

	before := key()
	if err := os.Chmod(src, 0755); err != nil {
		t.Fatal(err)
	}
	after := key()
	if before == after {
		t.Error("chmod +x on a COPY source did not change the cache key; " +
			"the step would report a CACHE HIT and serve the old mode")
	}

	// And the layer really does differ, which is why the key has to.
	files, err := collectGlob(ctx, "deploy.sh", &IgnoreList{})
	if err != nil {
		t.Fatal(err)
	}
	tf, err := buildCopyTar(files, "/app/deploy.sh", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range tf {
		if f.Path == "app/deploy.sh" && f.Mode&0111 == 0 {
			t.Errorf("COPY layer mode = %04o, want the executable bit", f.Mode)
		}
	}
}

// The other route out of the build context: a symlink inside it whose target is
// not. COPY reads through symlinks, so without a containment check a context
// shipping "key -> /etc/shadow" would have a plain `COPY . /app` pull the host's
// file into the image. The parser's "../" guard cannot see this one.
func TestCollectGlobRefusesSymlinksLeavingTheContext(t *testing.T) {
	ctx := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	mustWrite(t, outside, "host secret")
	mustWrite(t, filepath.Join(ctx, "ok.txt"), "fine")
	if err := os.Symlink(outside, filepath.Join(ctx, "leak.txt")); err != nil {
		t.Fatal(err)
	}

	files, err := collectGlob(ctx, "*", &IgnoreList{})
	if err != nil {
		return // refused outright, which is the intended outcome
	}
	for _, f := range files {
		data, _, rerr := readCopySource(f.HostPath)
		if rerr == nil && strings.Contains(string(data), "host secret") {
			t.Errorf("%s pulled a file from outside the build context", f.RelPath)
		}
	}
}

// The build context is whatever the user typed, and `docksmith build -t x .`
// is the ordinary invocation, so collectGlob must accept a relative context
// directory. It did not: safepath.Resolve left a relative root relative, and
// the containment re-check then rejected every legitimate source with
// `path "payload.txt" escapes .`. scripts/demo.sh always passes an absolute
// context, so 59 end-to-end checks all missed it.
func TestCollectGlobAcceptsARelativeContextDir(t *testing.T) {
	ctx := t.TempDir()
	if err := os.WriteFile(filepath.Join(ctx, "payload.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ctx, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctx, "src", "main.sh"), []byte("echo hi"), 0644); err != nil {
		t.Fatal(err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if err := os.Chdir(ctx); err != nil {
		t.Fatal(err)
	}

	for _, pattern := range []string{"payload.txt", "src/main.sh", "*"} {
		rel, err := collectGlob(".", pattern, &IgnoreList{})
		if err != nil {
			t.Errorf("collectGlob(\".\", %q) = error %v, want the files", pattern, err)
			continue
		}
		abs, err := collectGlob(ctx, pattern, &IgnoreList{})
		if err != nil {
			t.Fatalf("collectGlob(absolute, %q): %v", pattern, err)
		}
		if len(rel) != len(abs) {
			t.Errorf("pattern %q: relative context found %d files, absolute found %d",
				pattern, len(rel), len(abs))
			continue
		}
		for i := range rel {
			if rel[i].RelPath != abs[i].RelPath {
				t.Errorf("pattern %q entry %d: relative context gave RelPath %q, absolute gave %q",
					pattern, i, rel[i].RelPath, abs[i].RelPath)
			}
		}
	}

	// Containment is unchanged, at each of the two layers that enforce it.
	// The parser refuses a lexical escape up front...
	if err := validCopySource("../escape.txt"); err == nil {
		t.Error("validCopySource(\"../escape.txt\") accepted it, want it refused")
	}
	// ...and collectGlob re-checks each match through safepath, which is what
	// catches a symlink inside the context whose target is not. That check must
	// still fire with a relative context dir.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(ctx, "link.txt")); err != nil {
		t.Fatal(err)
	}
	got, err := collectGlob(".", "link.txt", &IgnoreList{})
	if err == nil && len(got) > 0 {
		t.Errorf("collectGlob(\".\", \"link.txt\") returned %d files through a symlink escaping the context, want it refused", len(got))
	}
}
