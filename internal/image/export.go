package image

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"docksmith/internal/store"
)

// Archive layout:
//
//	index.json                  what the archive contains
//	manifests/<name>_<tag>.json one per image
//	layers/<sha256hex>.tar      deduplicated across images
//
// Layers are already content-addressed, so an archive is little more than the
// store's own files with an index. That is also what makes verification on
// import meaningful: every layer's name is a claim about its bytes, and every
// manifest's digest is a claim about its content, so both can be rechecked
// rather than trusted.

const (
	archiveVersion = 1
	indexName      = "index.json"
	manifestsDir   = "manifests"
	layersDir      = "layers"
)

// IndexEntry names one image in an archive.
type IndexEntry struct {
	Name     string `json:"name"`
	Tag      string `json:"tag"`
	Digest   string `json:"digest"`
	Manifest string `json:"manifest"`
}

// Index is the archive's table of contents.
type Index struct {
	Version int          `json:"version"`
	Created string       `json:"created"`
	Images  []IndexEntry `json:"images"`
}

// Export writes the named images and their layers to w as a tar archive.
//
// Layers are streamed from disk rather than read into memory: store.BuildTar
// exists to build layers deterministically and holds whole files, which is the
// wrong tool for copying an arbitrarily large one.
func Export(st *store.State, refs []string, w io.Writer) error {
	if len(refs) == 0 {
		return fmt.Errorf("no images specified")
	}

	manifests := make([]*Manifest, 0, len(refs))
	seenImage := make(map[string]bool)
	for _, ref := range refs {
		name, tag := ParseNameTag(ref)
		key := name + ":" + tag
		if seenImage[key] {
			continue
		}
		seenImage[key] = true

		m, err := Load(st.ImagesDir, name, tag)
		if err != nil {
			return err
		}
		manifests = append(manifests, m)
	}

	tw := tar.NewWriter(w)
	defer tw.Close()

	idx := Index{
		Version: archiveVersion,
		Created: time.Now().UTC().Format(time.RFC3339),
	}
	for _, m := range manifests {
		idx.Images = append(idx.Images, IndexEntry{
			Name:     m.Name,
			Tag:      m.Tag,
			Digest:   m.Digest,
			Manifest: path.Join(manifestsDir, ManifestFileName(m.Name, m.Tag)),
		})
	}
	idxData, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	if err := writeArchiveFile(tw, indexName, idxData); err != nil {
		return err
	}

	for _, m := range manifests {
		data, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		name := path.Join(manifestsDir, ManifestFileName(m.Name, m.Tag))
		if err := writeArchiveFile(tw, name, data); err != nil {
			return err
		}
	}

	// Deduplicate: images sharing a base contribute the same layer once.
	written := make(map[string]bool)
	var digests []string
	for _, m := range manifests {
		for _, l := range m.Layers {
			if !written[l.Digest] {
				written[l.Digest] = true
				digests = append(digests, l.Digest)
			}
		}
	}
	sort.Strings(digests) // deterministic archive ordering

	for _, digest := range digests {
		if err := streamLayer(tw, st, digest); err != nil {
			return err
		}
	}
	return nil
}

// streamLayer copies one layer file into the archive without buffering it.
func streamLayer(tw *tar.Writer, st *store.State, digest string) error {
	path := st.LayerPath(digest)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("layer %s: %w", ShortDigest(digest), err)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("layer %s: %w", ShortDigest(digest), err)
	}
	defer f.Close()

	hdr := &tar.Header{
		Name:     layerArchiveName(digest),
		Mode:     0644,
		Size:     info.Size(),
		Typeflag: tar.TypeReg,
		ModTime:  time.Time{},
		Format:   tar.FormatGNU,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func writeArchiveFile(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{
		Name:     name,
		Mode:     0644,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
		ModTime:  time.Time{},
		Format:   tar.FormatGNU,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func layerArchiveName(digest string) string {
	return path.Join(layersDir, strings.TrimPrefix(digest, "sha256:")+".tar")
}

// ImportResult describes what happened to one image during Import.
type ImportResult struct {
	Ref     string
	Skipped bool // already present with the same digest
}

// Import reads an archive from r into the store, verifying as it goes.
//
// Nothing is written to the store until every layer and manifest has been
// checked, so a corrupt archive leaves the store untouched rather than
// half-updated.
func Import(st *store.State, r io.Reader) ([]ImportResult, error) {
	var idx *Index
	layers := make(map[string][]byte)    // archive path -> bytes
	manifests := make(map[string][]byte) // archive path -> bytes

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", hdr.Name, err)
		}

		name := path.Clean(hdr.Name)
		switch {
		case name == indexName:
			var parsed Index
			if err := json.Unmarshal(data, &parsed); err != nil {
				return nil, fmt.Errorf("archive index is not valid JSON: %w", err)
			}
			idx = &parsed
		case strings.HasPrefix(name, manifestsDir+"/"):
			manifests[name] = data
		case strings.HasPrefix(name, layersDir+"/"):
			layers[name] = data
		}
	}

	if idx == nil {
		return nil, fmt.Errorf("not a docksmith archive: no %s", indexName)
	}
	if idx.Version != archiveVersion {
		return nil, fmt.Errorf("unsupported archive version %d (this build understands %d)",
			idx.Version, archiveVersion)
	}

	// Every layer's filename is a claim about its own bytes. Check it.
	for name, data := range layers {
		want := strings.TrimSuffix(path.Base(name), ".tar")
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if got != want {
			return nil, fmt.Errorf("layer %s is corrupt: content hashes to %s",
				want[:min(12, len(want))], got[:12])
		}
	}

	// Every manifest's digest is a claim about its content. Check that too,
	// and that the layers it names are actually in the archive.
	parsed := make([]*Manifest, 0, len(idx.Images))
	for _, entry := range idx.Images {
		data, ok := manifests[path.Clean(entry.Manifest)]
		if !ok {
			return nil, fmt.Errorf("archive index names %s but it is missing", entry.Manifest)
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("manifest %s is not valid JSON: %w", entry.Manifest, err)
		}
		// The name and tag decide where this manifest is written, so they are
		// validated before anything else trusts them.
		if err := ValidRef(m.Name, m.Tag); err != nil {
			return nil, err
		}
		for _, l := range m.Layers {
			if !store.ValidDigest(l.Digest) {
				return nil, fmt.Errorf("manifest %s:%s names a malformed layer digest %q",
					m.Name, m.Tag, l.Digest)
			}
		}
		recomputed, err := ComputeDigest(&m)
		if err != nil {
			return nil, err
		}
		if recomputed != m.Digest {
			return nil, fmt.Errorf("manifest %s:%s is corrupt: content hashes to %s, not %s",
				m.Name, m.Tag, ShortDigest(recomputed), ShortDigest(m.Digest))
		}
		if entry.Digest != "" && entry.Digest != m.Digest {
			return nil, fmt.Errorf("archive index disagrees with manifest %s:%s about its digest",
				m.Name, m.Tag)
		}
		for _, l := range m.Layers {
			if _, ok := layers[layerArchiveName(l.Digest)]; ok {
				continue
			}
			if st.LayerExists(l.Digest) {
				continue // already in the store from an earlier, verified import
			}
			return nil, fmt.Errorf("image %s:%s needs layer %s, which the archive does not contain",
				m.Name, m.Tag, ShortDigest(l.Digest))
		}
		parsed = append(parsed, &m)
	}

	// Verification passed; commit.
	for _, data := range layers {
		if _, err := st.WriteLayer(data); err != nil {
			return nil, fmt.Errorf("writing layer: %w", err)
		}
	}

	results := make([]ImportResult, 0, len(parsed))
	for _, m := range parsed {
		ref := m.Name + ":" + m.Tag
		if existing, err := Load(st.ImagesDir, m.Name, m.Tag); err == nil && existing.Digest == m.Digest {
			results = append(results, ImportResult{Ref: ref, Skipped: true})
			continue
		}
		if err := Save(m, st.ImagesDir); err != nil {
			return nil, fmt.Errorf("saving %s: %w", ref, err)
		}
		results = append(results, ImportResult{Ref: ref})
	}
	return results, nil
}

// ShortDigest truncates a digest for display.
func ShortDigest(d string) string {
	if len(d) >= 19 {
		return d[:19]
	}
	return d
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
