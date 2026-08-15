package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Index is the cache index file stored in cache/.
type Index struct {
	Entries map[string]string `json:"entries"` // cacheKey -> layerDigest
}

const (
	indexFile = "index.json"
	lockFile  = ".lock"
)

func loadIndex(cacheDir string) (*Index, error) {
	path := filepath.Join(cacheDir, indexFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Index{Entries: make(map[string]string)}, nil
	}
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return &Index{Entries: make(map[string]string)}, nil
	}
	if idx.Entries == nil {
		idx.Entries = make(map[string]string)
	}
	return &idx, nil
}

func saveIndex(cacheDir string, idx *Index) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	// Write to a temp file and rename, so an interrupted write cannot leave a
	// truncated index behind — loadIndex would silently treat that as an empty
	// cache and every subsequent build would miss.
	path := filepath.Join(cacheDir, indexFile)
	tmp, err := os.CreateTemp(cacheDir, indexFile+".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// lockIndex takes an exclusive flock on the cache directory, returning a
// release function. Store is a read-modify-write of a single JSON file, so two
// concurrent builds would otherwise each load the index, add their own entry,
// and write back — with the second overwriting the first's entry. The lock
// lives in its own file so it is never truncated by the index write itself.
func lockIndex(cacheDir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(cacheDir, lockFile), os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Lookup checks the cache for a key. Returns layerDigest and true if found.
func Lookup(cacheDir, key string) (string, bool) {
	idx, err := loadIndex(cacheDir)
	if err != nil {
		return "", false
	}
	digest, ok := idx.Entries[key]
	return digest, ok
}

// Store writes a cache entry, serialised against concurrent builds.
func Store(cacheDir, key, layerDigest string) error {
	unlock, err := lockIndex(cacheDir)
	if err != nil {
		return err
	}
	defer unlock()

	// Load inside the lock: another build may have written entries since this
	// process last read the index.
	idx, err := loadIndex(cacheDir)
	if err != nil {
		return err
	}
	idx.Entries[key] = layerDigest
	return saveIndex(cacheDir, idx)
}

// Keys returns the whole index as a copy: cache key -> layer digest. Used by
// prune to find entries whose layer no longer exists.
func Keys(cacheDir string) (map[string]string, error) {
	idx, err := loadIndex(cacheDir)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(idx.Entries))
	for k, v := range idx.Entries {
		out[k] = v
	}
	return out, nil
}

// Delete removes cache entries, serialised against concurrent builds the same
// way Store is.
func Delete(cacheDir string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	unlock, err := lockIndex(cacheDir)
	if err != nil {
		return err
	}
	defer unlock()

	idx, err := loadIndex(cacheDir)
	if err != nil {
		return err
	}
	for _, k := range keys {
		delete(idx.Entries, k)
	}
	return saveIndex(cacheDir, idx)
}

// KeyParams holds all inputs for computing a cache key.
type KeyParams struct {
	PrevDigest  string            // digest of previous layer or base manifest digest
	Instruction string            // full instruction text
	WorkDir     string            // current WORKDIR value
	Env         map[string]string // accumulated ENV vars
	FileSums    map[string]string // COPY only: path -> sha256
}

// keyFormatVersion salts every cache key with the layer format that produced
// it. The key inputs describe the *instruction*, never the layer encoding, so
// without this salt a change to how layers are built silently reuses layers
// built by the old code: a RUN that deletes a file, cached before whiteout
// support existed, recomputes an identical key afterwards, hits, and serves
// the deletion-free layer forever behind a [CACHE HIT].
//
// Bump this whenever the bytes BuildTar emits for the same inputs change.
//
//	v2: whiteout entries added for deleted paths.
const keyFormatVersion = "v2"

// ComputeKey produces a deterministic cache key from KeyParams.
func ComputeKey(p KeyParams) string {
	var sb strings.Builder
	sb.WriteString(keyFormatVersion)
	sb.WriteByte('\n')
	sb.WriteString(p.PrevDigest)
	sb.WriteByte('\n')
	sb.WriteString(p.Instruction)
	sb.WriteByte('\n')
	sb.WriteString(p.WorkDir)
	sb.WriteByte('\n')

	// ENV: sorted by key
	envKeys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(p.Env[k])
		sb.WriteByte('\n')
	}

	// COPY file sums: sorted by path
	if len(p.FileSums) > 0 {
		paths := make([]string, 0, len(p.FileSums))
		for path := range p.FileSums {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			sb.WriteString(path)
			sb.WriteByte(':')
			sb.WriteString(p.FileSums[path])
			sb.WriteByte('\n')
		}
	}

	h := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(h[:])
}
