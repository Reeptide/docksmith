package cmd

import (
	"archive/tar"
	"compress/gzip"
	"docksmith/internal/image"
	"docksmith/internal/store"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RunImport imports a directory or tar/tar.gz as a named base image.
func RunImport(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: docksmith import <dir-or-tar> <name:tag>")
	}
	src := args[0]
	nameTag := args[1]
	name, tag := image.ParseNameTag(nameTag)

	st, err := store.NewState(stateRoot())
	if err != nil {
		return err
	}

	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}

	var tarFiles []store.TarFile
	if info.IsDir() {
		tarFiles, err = collectDir(src)
	} else {
		tarFiles, err = readTarArchive(src)
	}
	if err != nil {
		return err
	}

	sort.Slice(tarFiles, func(i, j int) bool {
		return tarFiles[i].Path < tarFiles[j].Path
	})

	tarData, err := store.BuildTar(tarFiles)
	if err != nil {
		return err
	}

	digest, err := st.WriteLayer(tarData)
	if err != nil {
		return err
	}

	m := &image.Manifest{
		Name:    name,
		Tag:     tag,
		Created: time.Now().UTC().Format(time.RFC3339),
		Config: image.Config{
			Env:        []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
			Cmd:        []string{"/bin/sh"},
			WorkingDir: "/",
		},
		Layers: []image.LayerEntry{
			{
				Digest:    digest,
				Size:      int64(len(tarData)),
				CreatedBy: "import " + src,
			},
		},
	}

	if err := image.Save(m, st.ImagesDir); err != nil {
		return err
	}
	saved, err := image.Load(st.ImagesDir, name, tag)
	if err != nil {
		return err
	}
	fmt.Printf("Imported %s:%s (digest: %s)\n", name, tag, saved.Digest[:19])
	return nil
}

func collectDir(dir string) ([]store.TarFile, error) {
	var files []store.TarFile
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." {
			return nil
		}

		// Check if this entry is a symlink (WalkDir does not follow symlinks).
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			files = append(files, store.TarFile{
				Path:      rel,
				Mode:      0777,
				IsSymlink: true,
				Linkname:  target,
			})
			return nil
		}

		if d.IsDir() {
			files = append(files, store.TarFile{
				Path:  rel + "/",
				Mode:  0755,
				IsDir: true,
			})
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := int64(0644)
		if info != nil {
			mode = int64(info.Mode())
		}
		files = append(files, store.TarFile{
			Path:    rel,
			Mode:    mode,
			Content: data,
		})
		return nil
	})
	return files, err
}

func readTarArchive(path string) ([]store.TarFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var reader io.Reader = f
	if strings.HasSuffix(path, ".gz") || strings.HasSuffix(path, ".tgz") {
		gr, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("gzip open: %w", err)
		}
		defer gr.Close()
		reader = gr
	}

	var files []store.TarFile
	tr := tar.NewReader(reader)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			p := hdr.Name
			if !strings.HasSuffix(p, "/") {
				p += "/"
			}
			files = append(files, store.TarFile{
				Path:  p,
				Mode:  hdr.Mode,
				IsDir: true,
			})
		case tar.TypeReg, tar.TypeRegA:
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			files = append(files, store.TarFile{
				Path:    hdr.Name,
				Mode:    hdr.Mode,
				Content: data,
			})
		case tar.TypeSymlink:
			files = append(files, store.TarFile{
				Path:      hdr.Name,
				Mode:      0777,
				IsSymlink: true,
				Linkname:  hdr.Linkname,
			})
		}
	}
	return files, nil
}
