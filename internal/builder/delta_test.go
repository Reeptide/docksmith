package builder

import (
	"os"
	"path/filepath"
	"testing"

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
