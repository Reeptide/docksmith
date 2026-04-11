package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Index is the cache index file stored in cache/.
type Index struct {
	Entries map[string]string `json:"entries"` // cacheKey -> layerDigest
}

const indexFile = "index.json"

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
	return os.WriteFile(filepath.Join(cacheDir, indexFile), data, 0644)
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

// Store writes a cache entry.
func Store(cacheDir, key, layerDigest string) error {
	idx, err := loadIndex(cacheDir)
	if err != nil {
		return err
	}
	idx.Entries[key] = layerDigest
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

// ComputeKey produces a deterministic cache key from KeyParams.
func ComputeKey(p KeyParams) string {
	var sb strings.Builder
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
