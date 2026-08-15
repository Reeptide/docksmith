package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"docksmith/internal/cache"
	"docksmith/internal/container"
	"docksmith/internal/image"
	"docksmith/internal/store"
)

// pruneFixture builds a store with one image, one referenced layer, one
// orphaned layer, an exited container and a running one.
func pruneFixture(t *testing.T) (root string, st *store.State, running *container.Record) {
	t.Helper()
	root = t.TempDir()
	st, err := store.NewState(root)
	if err != nil {
		t.Fatal(err)
	}

	keep, err := st.WriteLayer([]byte("referenced layer"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteLayer([]byte("orphaned layer")); err != nil {
		t.Fatal(err)
	}
	if err := image.Save(&image.Manifest{
		Name: "keeper", Tag: "1",
		Layers: []image.LayerEntry{{Digest: keep, Size: 16}},
	}, st.ImagesDir); err != nil {
		t.Fatal(err)
	}

	exited := &container.Record{
		ID: "1111111111111111", Name: "exited_one", Image: "keeper:1",
		State: container.StateExited, ExitCode: 0,
	}
	if err := container.Create(root, exited); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exited.RootFSPath(), "big"), make([]byte, 4096), 0644); err != nil {
		t.Fatal(err)
	}

	// A container whose process really is alive: this one must survive.
	running = &container.Record{
		ID: "2222222222222222", Name: "running_one", Image: "keeper:1",
		State: container.StateRunning, Pid: os.Getpid(),
	}
	if st, err := container.ReadStartTime(os.Getpid()); err == nil {
		running.StartTime = st
	}
	if err := container.Create(root, running); err != nil {
		t.Fatal(err)
	}
	return root, st, running
}

func TestPlanPruneSelectsOnlyReclaimableThings(t *testing.T) {
	root, st, running := pruneFixture(t)

	plan, err := planPrune(root, st, false)
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.containers) != 1 {
		t.Fatalf("planned %d containers, want 1 (the exited one)", len(plan.containers))
	}
	if plan.containers[0].ID == running.ID {
		t.Error("a running container was selected for removal")
	}
	if len(plan.layers) != 1 {
		t.Errorf("planned %d layers, want 1 (the orphan)", len(plan.layers))
	}
	if plan.bytes < 4096 {
		t.Errorf("reclaimable bytes = %d, should include the container rootfs", plan.bytes)
	}
}

// The invariant that matters: a layer any image still names must never be
// selected, however many other images were removed.
func TestPlanPruneNeverTouchesReferencedLayers(t *testing.T) {
	root, st, _ := pruneFixture(t)

	inUse, err := referencedLayers(st)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planPrune(root, st, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range plan.layers {
		if inUse[d] {
			t.Errorf("layer %s is referenced by an image but was planned for deletion", d)
		}
	}
}

func TestApplyPruneRemovesPlannedItemsOnly(t *testing.T) {
	root, st, running := pruneFixture(t)

	plan, err := planPrune(root, st, false)
	if err != nil {
		t.Fatal(err)
	}
	report, err := applyPrune(root, st, plan)
	if err != nil {
		t.Fatal(err)
	}
	if report.containers != 1 || report.layers != 1 {
		t.Errorf("report = %+v, want 1 container and 1 layer", report)
	}

	if _, err := container.Load(root, running.ID); err != nil {
		t.Errorf("the running container was removed: %v", err)
	}
	if _, err := container.Load(root, "1111111111111111"); err == nil {
		t.Error("the exited container survived prune")
	}

	// The image must still be usable afterwards.
	m, err := image.Load(st.ImagesDir, "keeper", "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range m.Layers {
		if !st.LayerExists(l.Digest) {
			t.Errorf("prune deleted layer %s, which keeper:1 still needs", l.Digest)
		}
	}
}

func TestPlanPruneDropsCacheEntriesForMissingLayers(t *testing.T) {
	root, st, _ := pruneFixture(t)

	// An entry pointing at a layer that does not exist can never hit again.
	if err := cache.Store(st.CacheDir, "stale-key", "sha256:doesnotexist"); err != nil {
		t.Fatal(err)
	}
	live, err := st.WriteLayer([]byte("live layer for cache"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Store(st.CacheDir, "live-key", live); err != nil {
		t.Fatal(err)
	}
	// Make the live layer referenced so it is not itself pruned.
	if err := image.Save(&image.Manifest{
		Name: "cached", Tag: "1",
		Layers: []image.LayerEntry{{Digest: live, Size: 1}},
	}, st.ImagesDir); err != nil {
		t.Fatal(err)
	}

	plan, err := planPrune(root, st, false)
	if err != nil {
		t.Fatal(err)
	}
	var sawStale, sawLive bool
	for _, k := range plan.cacheKeys {
		switch k {
		case "stale-key":
			sawStale = true
		case "live-key":
			sawLive = true
		}
	}
	if !sawStale {
		t.Error("a cache entry pointing at a missing layer should be pruned")
	}
	if sawLive {
		t.Error("a cache entry whose layer still exists should be kept without --all")
	}
}

func TestPlanPruneAllDropsEveryCacheEntry(t *testing.T) {
	root, st, _ := pruneFixture(t)
	live, err := st.WriteLayer([]byte("live layer"))
	if err != nil {
		t.Fatal(err)
	}
	if err := image.Save(&image.Manifest{
		Name: "cached", Tag: "1",
		Layers: []image.LayerEntry{{Digest: live, Size: 1}},
	}, st.ImagesDir); err != nil {
		t.Fatal(err)
	}
	if err := cache.Store(st.CacheDir, "live-key", live); err != nil {
		t.Fatal(err)
	}

	plan, err := planPrune(root, st, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range plan.cacheKeys {
		if k == "live-key" {
			found = true
		}
	}
	if !found {
		t.Error("--all should drop cache entries even when their layer exists")
	}
}

func TestEmptyPlanOnCleanStore(t *testing.T) {
	root := t.TempDir()
	st, err := store.NewState(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planPrune(root, st, false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.empty() {
		t.Errorf("a fresh store should have nothing to prune: %+v", plan)
	}
}

// A record left claiming to run by a killed supervisor must not keep its
// filesystem alive forever.
func TestPlanPruneReclaimsStaleRunningRecords(t *testing.T) {
	root := t.TempDir()
	st, err := store.NewState(root)
	if err != nil {
		t.Fatal(err)
	}
	ghost := &container.Record{
		ID: "3333333333333333", Name: "ghost", Image: "x:1",
		State: container.StateRunning, Pid: 0, // no live process
	}
	if err := container.Create(root, ghost); err != nil {
		t.Fatal(err)
	}

	plan, err := planPrune(root, st, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.containers) != 1 {
		t.Fatalf("planned %d containers, want 1 (the stale record)", len(plan.containers))
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1.0 KB",
		1536: "1.5 KB", 1048576: "1.0 MB", 1073741824: "1.0 GB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
