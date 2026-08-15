package store

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"docksmith/internal/safepath"
)

type State struct {
	Root      string
	ImagesDir string
	LayersDir string
	CacheDir  string
}

func NewState(root string) (*State, error) {
	s := &State{
		Root:      root,
		ImagesDir: filepath.Join(root, "images"),
		LayersDir: filepath.Join(root, "layers"),
		CacheDir:  filepath.Join(root, "cache"),
	}
	for _, d := range []string{s.ImagesDir, s.LayersDir, s.CacheDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// ValidDigest reports whether d is exactly "sha256:" followed by 64 lowercase
// hex characters.
//
// Digests arrive from image manifests, which for `docksmith load` means they
// arrive from an untrusted archive. Without this check a digest of "abc" makes
// LayerPath slice out of range, and one of "sha256:../../etc/passwd" makes it
// resolve outside the store entirely.
func ValidDigest(d string) bool {
	const prefix = "sha256:"
	if len(d) != len(prefix)+64 || !strings.HasPrefix(d, prefix) {
		return false
	}
	for _, c := range d[len(prefix):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// LayerPath returns the on-disk path for a layer, or "" when the digest is not
// a well-formed one. Callers must treat "" as "no such layer".
func (s *State) LayerPath(digest string) string {
	if !ValidDigest(digest) {
		return ""
	}
	return filepath.Join(s.LayersDir, digest[len("sha256:"):]+".tar")
}

func (s *State) LayerExists(digest string) bool {
	path := s.LayerPath(digest)
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// WriteLayer stores data under its own content hash, atomically. The write goes
// to a temp file first: os.WriteFile truncates in place, so an interrupted
// build would otherwise leave a short file that LayerExists reports as present
// forever.
func (s *State) WriteLayer(data []byte) (string, error) {
	h := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(h[:])
	path := s.LayerPath(digest)
	if _, err := os.Stat(path); err == nil {
		return digest, nil
	}

	tmp, err := os.CreateTemp(s.LayersDir, ".tmp-layer-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0644); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return digest, nil
}

func (s *State) ReadLayer(digest string) ([]byte, error) {
	path := s.LayerPath(digest)
	if path == "" {
		return nil, fmt.Errorf("malformed layer digest %q", digest)
	}
	return os.ReadFile(path)
}

func (s *State) DeleteLayer(digest string) error {
	path := s.LayerPath(digest)
	if path == "" {
		return fmt.Errorf("malformed layer digest %q", digest)
	}
	return os.Remove(path)
}

// WhiteoutPrefix marks a tar entry as a deletion instruction rather than
// content. A tar archive can only express "here is a file" — it has no way to
// say "the file that used to be here is gone". Since layers are stacked, a
// delta layer needs to record deletions of files inherited from lower layers,
// so a removal of /app/config.txt is encoded as a zero-byte entry named
// app/.wh.config.txt. ExtractTar turns that back into an os.RemoveAll.
//
// This follows the OCI image spec, which also defines an opaque-directory
// marker (.wh..wh..opq) meaning "ignore the lower layers' version of this
// directory entirely". Docksmith does not need it: layers are extracted
// sequentially into a single real directory rather than stacked with
// overlayfs, so there is no lower layer still visible underneath at runtime.
//
// The prefix is necessarily reserved — a real file genuinely named .wh.foo in
// a build context would be read as a deletion. Docker carries the same hazard.
const WhiteoutPrefix = ".wh."

// WhiteoutPath returns the marker path that deletes rel when the layer is
// extracted. rel is a slash-separated path relative to the rootfs.
func WhiteoutPath(rel string) string {
	dir, base := path.Split(path.Clean("/" + rel))
	return strings.TrimPrefix(dir+WhiteoutPrefix+base, "/")
}

// IsWhiteout reports whether a tar entry name is a deletion marker, and if so
// returns the path it deletes.
//
// A marker with nothing after the prefix names no path, and must not be treated
// as a whiteout: an entry called exactly ".wh." would otherwise resolve to the
// extraction root itself and delete the entire rootfs.
func IsWhiteout(name string) (target string, ok bool) {
	dir, base := path.Split(path.Clean("/" + name))
	if !strings.HasPrefix(base, WhiteoutPrefix) {
		return "", false
	}
	shadowed := strings.TrimPrefix(base, WhiteoutPrefix)
	if shadowed == "" || shadowed == "." || shadowed == ".." {
		return "", false
	}
	return strings.TrimPrefix(dir+shadowed, "/"), true
}

type TarFile struct {
	Path      string
	Mode      int64
	IsDir     bool
	Content   []byte
	Linkname  string // symlink target (if IsSymlink)
	IsSymlink bool

	// IsWhiteout marks this entry as a deletion of Path rather than content at
	// Path. BuildTar writes it out as WhiteoutPath(Path).
	IsWhiteout bool
}

func BuildTar(files []TarFile) ([]byte, error) {
	// Whiteouts sort ahead of all content so a deletion can never clobber an
	// entry written earlier in the same layer. Relying on lexical order alone
	// would be wrong: byte-wise, ".cache" sorts before ".wh..cache" (compare at
	// index 1: 'c' 0x63 < 'w' 0x77), which breaks for most real dotfiles.
	// Within each group the sort stays deterministic by path, which is what the
	// content-addressed digests depend on.
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].IsWhiteout != files[j].IsWhiteout {
			return files[i].IsWhiteout
		}
		return files[i].Path < files[j].Path
	})
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		name := f.Path
		if f.IsWhiteout {
			name = WhiteoutPath(f.Path)
		}
		hdr := &tar.Header{
			Name:    name,
			Mode:    f.Mode,
			ModTime: time.Time{},
			Uid:     0,
			Gid:     0,
			Uname:   "",
			Gname:   "",
			Format:  tar.FormatGNU,
		}
		if f.IsWhiteout {
			// Always a zero-byte regular file, even when deleting a directory —
			// the marker is an instruction, not a copy of what it removes.
			hdr.Typeflag = tar.TypeReg
			hdr.Size = 0
		} else if f.IsSymlink {
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = f.Linkname
			hdr.Size = 0
		} else if f.IsDir {
			hdr.Typeflag = tar.TypeDir
			hdr.Size = 0
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(f.Content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if !f.IsDir && !f.IsSymlink && !f.IsWhiteout && len(f.Content) > 0 {
			if _, err := tw.Write(f.Content); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ExtractTar(data []byte, destDir string) error {
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// A whiteout entry deletes the path it shadows and is never itself
		// written to disk.
		if wh, ok := IsWhiteout(hdr.Name); ok {
			victim, err := safepath.Resolve(destDir, wh)
			if err != nil {
				return fmt.Errorf("whiteout %s: %w", hdr.Name, err)
			}
			if err := os.RemoveAll(victim); err != nil {
				return fmt.Errorf("whiteout %s: %w", hdr.Name, err)
			}
			continue
		}

		target, err := safepath.Resolve(destDir, hdr.Name)
		if err != nil {
			return err
		}
		mode := os.FileMode(hdr.Mode).Perm()

		switch hdr.Typeflag {
		case tar.TypeDir:
			if _, err := safepath.MkdirAll(destDir, hdr.Name); err != nil {
				return err
			}
			if err := os.Chmod(target, mode); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if _, err := safepath.MkdirAll(destDir, path.Dir(filepath.ToSlash(hdr.Name))); err != nil {
				return err
			}
			// Remove first: O_TRUNC on an existing file keeps its old mode, so
			// a later layer changing a file's permissions would be ignored.
			// It also stops a write landing on a symlink left by an earlier
			// entry, which is the escape this function exists to prevent.
			os.Remove(target)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, cpErr := io.Copy(f, tr)
			f.Close()
			if cpErr != nil {
				return cpErr
			}
			// Explicit chmod: the mode passed to OpenFile is masked by umask.
			if err := os.Chmod(target, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if _, err := safepath.MkdirAll(destDir, path.Dir(filepath.ToSlash(hdr.Name))); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("symlink %s: %w", hdr.Name, err)
			}
		case tar.TypeLink:
			if _, err := safepath.MkdirAll(destDir, path.Dir(filepath.ToSlash(hdr.Name))); err != nil {
				return err
			}
			linkTarget, err := safepath.Resolve(destDir, hdr.Linkname)
			if err != nil {
				return fmt.Errorf("hard link %s: %w", hdr.Name, err)
			}
			os.Remove(target)
			if err := os.Link(linkTarget, target); err != nil {
				srcData, rerr := os.ReadFile(linkTarget)
				if rerr != nil {
					return fmt.Errorf("hard link %s -> %s: %w", hdr.Name, hdr.Linkname, err)
				}
				if werr := os.WriteFile(target, srcData, mode); werr != nil {
					return fmt.Errorf("hard link copy fallback %s: %w", hdr.Name, werr)
				}
			}
		}
	}
	return nil
}

func DigestBytes(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

func DigestFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return DigestBytes(data), nil
}
