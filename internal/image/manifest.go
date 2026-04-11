package image

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LayerEntry describes one layer in an image manifest.
type LayerEntry struct {
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	CreatedBy string `json:"createdBy"`
}

// Config holds the image runtime config.
type Config struct {
	Env        []string `json:"Env"`
	Cmd        []string `json:"Cmd"`
	WorkingDir string   `json:"WorkingDir"`
}

// Manifest is the JSON file stored under images/.
type Manifest struct {
	Name    string       `json:"name"`
	Tag     string       `json:"tag"`
	Digest  string       `json:"digest"`
	Created string       `json:"created"`
	Config  Config       `json:"config"`
	Layers  []LayerEntry `json:"layers"`
}

// ComputeDigest serializes the manifest with digest="" and SHA-256s the bytes.
func ComputeDigest(m *Manifest) (string, error) {
	orig := m.Digest
	m.Digest = ""
	data, err := json.Marshal(m)
	m.Digest = orig
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", h), nil
}

// Finalize sets the digest field on the manifest.
func Finalize(m *Manifest) error {
	d, err := ComputeDigest(m)
	if err != nil {
		return err
	}
	m.Digest = d
	return nil
}

// ManifestFileName returns the file name used to store this manifest.
func ManifestFileName(name, tag string) string {
	safe := strings.ReplaceAll(name, "/", "_")
	return fmt.Sprintf("%s_%s.json", safe, tag)
}

// Save writes the manifest to imagesDir.
func Save(m *Manifest, imagesDir string) error {
	if err := Finalize(m); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(imagesDir, ManifestFileName(m.Name, m.Tag))
	return os.WriteFile(path, data, 0644)
}

// Load reads a manifest from imagesDir by name:tag.
func Load(imagesDir, name, tag string) (*Manifest, error) {
	path := filepath.Join(imagesDir, ManifestFileName(name, tag))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("image %s:%s not found in local store", name, tag)
		}
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ListAll returns all manifests in imagesDir.
func ListAll(imagesDir string) ([]*Manifest, error) {
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		return nil, err
	}
	var out []*Manifest
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(imagesDir, e.Name()))
		if err != nil {
			continue
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		out = append(out, &m)
	}
	return out, nil
}

// NowISO returns current time as ISO-8601.
func NowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// ParseNameTag splits "name:tag" into parts. Defaults tag to "latest".
func ParseNameTag(s string) (name, tag string) {
	parts := strings.SplitN(s, ":", 2)
	name = parts[0]
	tag = "latest"
	if len(parts) == 2 {
		tag = parts[1]
	}
	return
}
