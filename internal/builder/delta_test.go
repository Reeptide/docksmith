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
