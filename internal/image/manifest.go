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
	// ExposedPorts documents the ports a container listens on, as "80/tcp".
	// Advisory: it publishes nothing on its own, but supplies the default
	// container-port set for `run -p`.
	ExposedPorts []string `json:"ExposedPorts,omitempty"`
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
//
// Both components are sanitised, not just the name: manifests arrive from
// archives during `docksmith load`, so an unsanitised tag of "../../etc/x"
// would make filepath.Join escape the images directory entirely and write
// wherever the archive asked. Anything outside the safe set becomes "_".
func ManifestFileName(name, tag string) string {
	return fmt.Sprintf("%s_%s.json", sanitiseRef(name), sanitiseRef(tag))
}

// sanitiseRef reduces a name or tag to characters that cannot traverse or
// escape a path.
func sanitiseRef(s string) string {
	if s == "" {
		return "_"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	// A component consisting only of dots would still be a traversal.
	if strings.Trim(out, ".") == "" {
		return "_"
	}
	return out
}

// ValidRef reports whether a name and tag are storable without mangling. Used
// to reject hostile references outright rather than silently rewriting them.
func ValidRef(name, tag string) error {
	if name == "" || tag == "" {
		return fmt.Errorf("image name and tag must not be empty")
	}
	for _, part := range []string{name, tag} {
		if strings.Contains(part, "..") || strings.ContainsAny(part, `\:`+"\x00") {
			return fmt.Errorf("invalid image reference %q:%q", name, tag)
		}
	}
	// Names may contain "/" (registry-style); tags may not.
	if strings.Contains(tag, "/") {
		return fmt.Errorf("invalid tag %q", tag)
	}
	return nil
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
	// Write atomically. A plain os.WriteFile truncates in place, and any reader
	// that catches the window sees an unparseable manifest — which for
	// referencedLayers means an image's layers drop out of the live set and
	// prune deletes them.
	path := filepath.Join(imagesDir, ManifestFileName(m.Name, m.Tag))
	tmp, err := os.CreateTemp(imagesDir, ".tmp-manifest-*")
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
		// Temp files from an in-flight Save are not manifests yet.
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(imagesDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			// Deliberately not skipped. Callers include referencedLayers, which
			// decides what garbage collection may delete — treating a manifest
			// it cannot read as "references nothing" is how prune ends up
			// deleting layers a live image still needs.
			return nil, fmt.Errorf("reading manifest %s: %w", e.Name(), err)
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("manifest %s is corrupt: %w", e.Name(), err)
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
