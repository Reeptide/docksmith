package store

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
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

func (s *State) LayerPath(digest string) string {
	h := digest[len("sha256:"):]
	return filepath.Join(s.LayersDir, h+".tar")
}

func (s *State) LayerExists(digest string) bool {
	_, err := os.Stat(s.LayerPath(digest))
	return err == nil
}

func (s *State) WriteLayer(data []byte) (string, error) {
	h := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(h[:])
	path := s.LayerPath(digest)
	if _, err := os.Stat(path); err == nil {
		return digest, nil
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", err
	}
	return digest, nil
}

func (s *State) ReadLayer(digest string) ([]byte, error) {
	return os.ReadFile(s.LayerPath(digest))
}

func (s *State) DeleteLayer(digest string) error {
	return os.Remove(s.LayerPath(digest))
}

type TarFile struct {
	Path     string
	Mode     int64
	IsDir    bool
	Content  []byte
	Linkname  string // symlink target (if IsSymlink)
	IsSymlink bool
}

func BuildTar(files []TarFile) ([]byte, error) {
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		hdr := &tar.Header{
			Name:    f.Path,
			Mode:    f.Mode,
			ModTime: time.Time{},
			Uid:     0,
			Gid:     0,
			Uname:   "",
			Gname:   "",
			Format:  tar.FormatGNU,
		}
		if f.IsSymlink {
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
		if !f.IsDir && !f.IsSymlink && len(f.Content) > 0 {
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
		target := filepath.Join(destDir, filepath.Clean("/"+hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)|0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)|0644)
			if err != nil {
				return err
			}
			_, cpErr := io.Copy(f, tr)
			f.Close()
			if cpErr != nil {
				return cpErr
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				fmt.Fprintf(os.Stderr, "warning: symlink %s -> %s: %v\n", target, hdr.Linkname, err)
			}
		case tar.TypeLink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			linkTarget := filepath.Join(destDir, filepath.Clean("/"+hdr.Linkname))
			os.Remove(target)
			if err := os.Link(linkTarget, target); err != nil {
				srcData, rerr := os.ReadFile(linkTarget)
				if rerr != nil {
					fmt.Fprintf(os.Stderr, "warning: hard link %s: %v\n", target, err)
					continue
				}
				if werr := os.WriteFile(target, srcData, 0644); werr != nil {
					fmt.Fprintf(os.Stderr, "warning: hard link copy fallback %s: %v\n", target, werr)
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
