package cache

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// writeFile clobbers the on-disk index, simulating corruption.
func writeFile(cacheDir, body string) error {
	return os.WriteFile(filepath.Join(cacheDir, indexFile), []byte(body), 0644)
}

func baseParams() KeyParams {
	return KeyParams{
		PrevDigest:  "sha256:abc",
		Instruction: "RUN echo hi",
		WorkDir:     "/app",
		Env:         map[string]string{"A": "1", "B": "2"},
	}
}

// Map iteration order is randomised in Go, so a key that folded in map order
// would be unstable across runs — and the content-addressed cache would then
// miss at random.
func TestComputeKeyStableAcrossMapOrder(t *testing.T) {
	want := ComputeKey(baseParams())
	for i := 0; i < 100; i++ {
		p := baseParams()
		p.Env = map[string]string{"B": "2", "A": "1"}
		p.FileSums = map[string]string{"z": "1", "a": "2", "m": "3"}
		q := baseParams()
		q.Env = map[string]string{"A": "1", "B": "2"}
		q.FileSums = map[string]string{"a": "2", "m": "3", "z": "1"}
		if ComputeKey(p) != ComputeKey(q) {
			t.Fatal("key depends on map iteration order")
		}
	}
	if ComputeKey(baseParams()) != want {
		t.Error("key is not reproducible for identical input")
	}
}

// Every input must be able to invalidate the cache on its own.
func TestComputeKeySensitiveToEveryField(t *testing.T) {
	base := ComputeKey(baseParams())

	mutations := map[string]func(*KeyParams){
		"PrevDigest":  func(p *KeyParams) { p.PrevDigest = "sha256:different" },
		"Instruction": func(p *KeyParams) { p.Instruction = "RUN echo bye" },
		"WorkDir":     func(p *KeyParams) { p.WorkDir = "/other" },
		"Env value":   func(p *KeyParams) { p.Env["A"] = "changed" },
		"Env key":     func(p *KeyParams) { p.Env["C"] = "3" },
		"FileSums":    func(p *KeyParams) { p.FileSums = map[string]string{"f": "deadbeef"} },
	}
	for name, mutate := range mutations {
		p := baseParams()
		mutate(&p)
		if ComputeKey(p) == base {
			t.Errorf("changing %s did not change the cache key", name)
		}
	}
}

// The salt is what stops layers built by older code from being served after
// the layer format changes. Without it, the whiteout fix would be invisible on
// any pre-existing cache.
// The whole point of keyFormatVersion is that bumping it invalidates every
// key. That cannot be checked by comparing two live ComputeKey calls — the
// constant is the same on both sides of any such comparison, so a version that
// was never wired into the hash at all still passes. The previous version of
// this test did exactly that, asserting only that a different PrevDigest gives
// a different key, which is true whether or not the salt exists.
//
// A golden value is the only formulation that actually fails. This digest was
// produced under keyFormatVersion "v4"; if the version changes and this value
// does not, the salt is not reaching the hash and every stale layer built by
// older code is still being served behind a [CACHE HIT].
//
// Bumping keyFormatVersion is therefore expected to break this test. Recompute
// the constant below — deliberately, as part of the bump — rather than deleting
// the assertion.
const goldenKeyV4 = "ad9915adb4a113d3706a299116b86d947b539def9240be959db82585365368a9"

func TestComputeKeyIncludesFormatVersion(t *testing.T) {
	if keyFormatVersion == "" {
		t.Fatal("keyFormatVersion must not be empty")
	}
	if keyFormatVersion != "v4" {
		t.Fatalf("keyFormatVersion is now %q: recompute goldenKeyV4 (and rename it) "+
			"so the new format's keys are pinned", keyFormatVersion)
	}
	if got := ComputeKey(baseParams()); got != goldenKeyV4 {
		t.Errorf("ComputeKey(baseParams()) = %s\nwant %s\n"+
			"Either the key inputs changed without a keyFormatVersion bump, or "+
			"the version is not reaching the hash.", got, goldenKeyV4)
	}
}

func TestStoreLookupRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok := Lookup(dir, "missing"); ok {
		t.Error("empty cache should not report a hit")
	}
	if err := Store(dir, "k1", "sha256:layer1"); err != nil {
		t.Fatal(err)
	}
	got, ok := Lookup(dir, "k1")
	if !ok || got != "sha256:layer1" {
		t.Errorf("Lookup = (%q, %v), want (sha256:layer1, true)", got, ok)
	}
}

func TestStoreOverwritesExistingKey(t *testing.T) {
	dir := t.TempDir()
	if err := Store(dir, "k", "sha256:old"); err != nil {
		t.Fatal(err)
	}
	if err := Store(dir, "k", "sha256:new"); err != nil {
		t.Fatal(err)
	}
	if got, _ := Lookup(dir, "k"); got != "sha256:new" {
		t.Errorf("Lookup = %q, want sha256:new", got)
	}
}

// The reason Store takes a flock: without it, concurrent builds each load the
// index, add one entry, and write back — and all but the last entry is lost.
func TestStoreConcurrentWritesAllSurvive(t *testing.T) {
	dir := t.TempDir()
	const n = 50

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := Store(dir, fmt.Sprintf("key-%d", i), fmt.Sprintf("sha256:layer-%d", i)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Store failed: %v", err)
	}

	for i := 0; i < n; i++ {
		want := fmt.Sprintf("sha256:layer-%d", i)
		got, ok := Lookup(dir, fmt.Sprintf("key-%d", i))
		if !ok {
			t.Errorf("key-%d was lost", i)
			continue
		}
		if got != want {
			t.Errorf("key-%d = %q, want %q", i, got, want)
		}
	}
}

// A corrupt index must degrade to a cold cache, never fail the build.
func TestLookupToleratesCorruptIndex(t *testing.T) {
	dir := t.TempDir()
	if err := Store(dir, "k", "sha256:layer"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(dir, "not json at all"); err != nil {
		t.Fatal(err)
	}
	if _, ok := Lookup(dir, "k"); ok {
		t.Error("corrupt index should report a miss, not a stale hit")
	}
	if err := Store(dir, "k2", "sha256:layer2"); err != nil {
		t.Errorf("Store should recover from a corrupt index: %v", err)
	}
	if got, ok := Lookup(dir, "k2"); !ok || got != "sha256:layer2" {
		t.Error("cache should be usable again after recovering from corruption")
	}
}
