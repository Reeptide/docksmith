package cmd

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"docksmith/internal/cache"
	"docksmith/internal/container"
	"docksmith/internal/network"
	"docksmith/internal/store"
)

// pruneReport accumulates what a prune reclaimed.
type pruneReport struct {
	containers int
	layers     int
	cacheKeys  int
	leases     int
	bytes      int64
}

func RunPrune(args []string) error {
	var force, all bool
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.BoolVar(&force, "f", false, "do not prompt for confirmation")
	fs.BoolVar(&all, "all", false, "also remove build cache entries for layers that still exist")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: docksmith prune [-f] [--all]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	root := stateRoot()
	st, err := store.NewState(root)
	if err != nil {
		return err
	}

	// Work out what would go before touching anything, so the confirmation
	// prompt can describe the actual damage rather than a vague warning.
	plan, err := planPrune(root, st, all)
	if err != nil {
		return err
	}
	if plan.empty() {
		fmt.Println("Nothing to prune.")
		return nil
	}

	fmt.Println(plan.describe())
	if !force && !confirm() {
		fmt.Println("Aborted.")
		return nil
	}

	report, err := applyPrune(root, st, plan)
	if err != nil {
		return err
	}
	fmt.Printf("Reclaimed %s (%d container(s), %d layer(s), %d cache entr(ies), %d IP lease(s))\n",
		humanBytes(report.bytes), report.containers, report.layers, report.cacheKeys, report.leases)
	return nil
}

// prunePlan is what prune intends to remove.
type prunePlan struct {
	containers []*container.Record
	layers     []string // digests
	cacheKeys  []string
	leases     []string // container ids holding an address
	bytes      int64
}

func (p prunePlan) empty() bool {
	return len(p.containers) == 0 && len(p.layers) == 0 &&
		len(p.cacheKeys) == 0 && len(p.leases) == 0
}

func (p prunePlan) describe() string {
	var b strings.Builder
	b.WriteString("This will remove:\n")
	if n := len(p.containers); n > 0 {
		fmt.Fprintf(&b, "  - %d exited container(s) and their filesystems\n", n)
	}
	if n := len(p.layers); n > 0 {
		fmt.Fprintf(&b, "  - %d layer(s) no image references\n", n)
	}
	if n := len(p.cacheKeys); n > 0 {
		fmt.Fprintf(&b, "  - %d build cache entr(ies)\n", n)
	}
	if n := len(p.leases); n > 0 {
		fmt.Fprintf(&b, "  - %d stale IP lease(s)\n", n)
	}
	fmt.Fprintf(&b, "Total reclaimable: %s", humanBytes(p.bytes))
	return b.String()
}

// planPrune decides what is safe to remove. Nothing running is ever touched.
func planPrune(root string, st *store.State, all bool) (prunePlan, error) {
	var plan prunePlan

	records, err := container.List(root)
	if err != nil {
		return plan, err
	}
	liveContainers := make(map[string]bool)
	for _, r := range records {
		// Reconcile first: a record claiming to run whose process is gone must
		// not protect its resources from being reclaimed.
		if r.Reconcile() {
			container.Save(r)
		}
		// StateCreated means a `run` is still assembling this container: the
		// rootfs is being written, an address is already leased, and
		// MarkStarted has not happened yet. Reclaiming it would delete the
		// directory out from under a container about to pivot_root into it
		// and hand its address to someone else.
		if r.State == container.StateRunning || r.State == container.StateCreated {
			liveContainers[r.ID] = true
			continue
		}
		plan.containers = append(plan.containers, r)
		plan.bytes += dirSize(r.Dir)
	}

	// Layers are shared, so only those no surviving manifest names can go.
	// This is the same live-set computation rmi uses.
	inUse, err := referencedLayers(st)
	if err != nil {
		return plan, err
	}
	entries, err := os.ReadDir(st.LayersDir)
	if err != nil && !os.IsNotExist(err) {
		return plan, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar") {
			continue
		}
		digest := "sha256:" + strings.TrimSuffix(e.Name(), ".tar")
		if inUse[digest] {
			continue
		}
		plan.layers = append(plan.layers, digest)
		if info, err := e.Info(); err == nil {
			plan.bytes += info.Size()
		}
	}

	// Cache entries pointing at layers that are gone can never hit again; with
	// --all, drop the rest of the build cache too.
	keys, err := cache.Keys(st.CacheDir)
	if err != nil {
		return plan, err
	}
	doomed := make(map[string]bool, len(plan.layers))
	for _, d := range plan.layers {
		doomed[d] = true
	}
	for key, digest := range keys {
		if all || doomed[digest] || !st.LayerExists(digest) {
			plan.cacheKeys = append(plan.cacheKeys, key)
		}
	}

	// IP leases held by containers that no longer exist.
	for id := range allLeases(root) {
		if liveContainers[id] {
			continue
		}
		if _, err := container.Load(root, id); err == nil {
			continue // container still exists, just not running
		}
		plan.leases = append(plan.leases, id)
	}

	return plan, nil
}

func applyPrune(root string, st *store.State, plan prunePlan) (pruneReport, error) {
	var report pruneReport
	report.bytes = plan.bytes

	for _, r := range plan.containers {
		// Release the address and iptables rules before the record naming
		// them disappears.
		teardownNetwork(root, r)
		if err := container.Remove(r); err != nil {
			fmt.Fprintf(os.Stderr, "warning: container %s: %v\n", container.ShortID(r.ID), err)
			continue
		}
		report.containers++
	}
	for _, digest := range plan.layers {
		if err := st.DeleteLayer(digest); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warning: layer %s: %v\n", digest, err)
			continue
		}
		report.layers++
	}
	if len(plan.cacheKeys) > 0 {
		if err := cache.Delete(st.CacheDir, plan.cacheKeys); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cache: %v\n", err)
		} else {
			report.cacheKeys = len(plan.cacheKeys)
		}
	}
	for _, id := range plan.leases {
		if err := network.Release(root, id); err != nil {
			fmt.Fprintf(os.Stderr, "warning: lease %s: %v\n", container.ShortID(id), err)
			continue
		}
		report.leases++
	}
	return report, nil
}

// allLeases returns every container id currently holding an address.
func allLeases(root string) map[string]bool {
	out := make(map[string]bool)
	for _, id := range network.LeaseHolders(root) {
		out[id] = true
	}
	return out
}

// dirSize sums the bytes a directory occupies.
func dirSize(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func confirm() bool {
	fmt.Print("Continue? [y/N] ")
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return answer == "y" || answer == "yes"
}
