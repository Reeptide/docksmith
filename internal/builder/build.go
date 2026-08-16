package builder

import (
	"crypto/sha256"
	"docksmith/internal/cache"
	"docksmith/internal/image"
	"docksmith/internal/runtime"
	"docksmith/internal/store"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BuildOptions configures a build.
type BuildOptions struct {
	ContextDir string
	Tag        string
	NoCache    bool
	State      *store.State
}

// buildContext holds all mutable state for a single build execution.
type buildContext struct {
	opts             BuildOptions
	name, tag        string
	totalSteps       int
	currentLayers    []image.LayerEntry
	envMap           map[string]string
	workDir          string
	cmd              []string
	cmdSet           bool
	prevDigest       string
	cacheInvalidated bool
	allCacheHit      bool
	existingManifest *image.Manifest
	startTime        time.Time
	ignore           *IgnoreList
	exposed          []string
}

// Build executes a Docksmithfile build.
func Build(opts BuildOptions) error {
	docksmithfile := filepath.Join(opts.ContextDir, "Docksmithfile")
	instrs, err := ParseFile(docksmithfile)
	if err != nil {
		return err
	}

	name, tag := image.ParseNameTag(opts.Tag)
	existing, _ := image.Load(opts.State.ImagesDir, name, tag)

	ignore, err := LoadIgnoreList(opts.ContextDir)
	if err != nil {
		return fmt.Errorf("reading %s: %w", IgnoreFileName, err)
	}

	bc := &buildContext{
		opts:             opts,
		name:             name,
		tag:              tag,
		totalSteps:       len(instrs),
		envMap:           make(map[string]string),
		allCacheHit:      true,
		existingManifest: existing,
		startTime:        time.Now(),
		ignore:           ignore,
	}

	for i, instr := range instrs {
		if err := bc.executeStep(i+1, instr); err != nil {
			return err
		}
	}

	return bc.assemble()
}

// executeStep dispatches a single instruction to the appropriate handler.
func (bc *buildContext) executeStep(stepNum int, instr Instruction) error {
	switch instr.Type {
	case InstrFROM:
		return bc.execFROM(stepNum, instr)
	case InstrWORKDIR:
		return bc.execWORKDIR(stepNum, instr)
	case InstrENV:
		return bc.execENV(stepNum, instr)
	case InstrCMD:
		return bc.execCMD(stepNum, instr)
	case InstrEXPOSE:
		return bc.execEXPOSE(stepNum, instr)
	case InstrCOPY:
		return bc.execCOPY(stepNum, instr)
	case InstrRUN:
		return bc.execRUN(stepNum, instr)
	default:
		return fmt.Errorf("line %d: unknown instruction %s", instr.LineNum, instr.Type)
	}
}

func (bc *buildContext) execFROM(stepNum int, instr Instruction) error {
	fmt.Printf("Step %d/%d : FROM %s\n", stepNum, bc.totalSteps, instr.Args)
	parsed, err := instr.AsFROM()
	if err != nil {
		return err
	}
	base, err := image.Load(bc.opts.State.ImagesDir, parsed.Name, parsed.Tag)
	if err != nil {
		return fmt.Errorf("line %d: %w", instr.LineNum, err)
	}
	bc.currentLayers = make([]image.LayerEntry, len(base.Layers))
	copy(bc.currentLayers, base.Layers)
	// Inherit base env.
	for _, e := range base.Config.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			bc.envMap[parts[0]] = parts[1]
		}
	}
	if bc.workDir == "" && base.Config.WorkingDir != "" {
		bc.workDir = base.Config.WorkingDir
	}
	bc.exposed = append(bc.exposed, base.Config.ExposedPorts...)
	// Do NOT inherit base image CMD — spec requires explicit CMD.
	bc.prevDigest = base.Digest
	return nil
}

func (bc *buildContext) execWORKDIR(stepNum int, instr Instruction) error {
	fmt.Printf("Step %d/%d : WORKDIR %s\n", stepNum, bc.totalSteps, instr.Args)
	bc.workDir = instr.Args
	return nil
}

func (bc *buildContext) execENV(stepNum int, instr Instruction) error {
	fmt.Printf("Step %d/%d : ENV %s\n", stepNum, bc.totalSteps, instr.Args)
	parsed, err := instr.AsENV()
	if err != nil {
		return err
	}
	bc.envMap[parsed.Key] = parsed.Value
	return nil
}

func (bc *buildContext) execCMD(stepNum int, instr Instruction) error {
	fmt.Printf("Step %d/%d : CMD %s\n", stepNum, bc.totalSteps, instr.Args)
	parsed, err := instr.AsCMD()
	if err != nil {
		return err
	}
	bc.cmd = parsed
	bc.cmdSet = true
	return nil
}

func (bc *buildContext) execEXPOSE(stepNum int, instr Instruction) error {
	fmt.Printf("Step %d/%d : EXPOSE %s\n", stepNum, bc.totalSteps, instr.Args)
	ports, err := instr.AsEXPOSE()
	if err != nil {
		return err
	}
	for _, p := range ports {
		if !contains(bc.exposed, p) {
			bc.exposed = append(bc.exposed, p)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (bc *buildContext) execCOPY(stepNum int, instr Instruction) error {
	t0 := time.Now()
	instrText := "COPY " + instr.Args
	parsed, err := instr.AsCOPY()
	if err != nil {
		return err
	}

	// Collect sources.
	srcFiles, err := collectGlob(bc.opts.ContextDir, parsed.Src, bc.ignore)
	if err != nil {
		return fmt.Errorf("line %d: COPY glob error: %w", instr.LineNum, err)
	}
	if len(srcFiles) == 0 {
		return fmt.Errorf("line %d: COPY: no files matched %q", instr.LineNum, parsed.Src)
	}
	sort.Slice(srcFiles, func(i, j int) bool {
		return srcFiles[i].RelPath < srcFiles[j].RelPath
	})

	// File digests for cache key (sorted by rel path).
	fileSums := make(map[string]string)
	for _, sf := range srcFiles {
		data, err := os.ReadFile(sf.HostPath)
		if err != nil {
			return err
		}
		h := sha256.Sum256(data)
		fileSums[sf.RelPath] = hex.EncodeToString(h[:])
	}

	cacheKey := cache.ComputeKey(cache.KeyParams{
		PrevDigest:  bc.prevDigest,
		Instruction: instrText,
		WorkDir:     bc.workDir,
		Env:         copyEnvMap(bc.envMap),
		FileSums:    fileSums,
	})

	if !bc.opts.NoCache && !bc.cacheInvalidated {
		if digest, ok := cache.Lookup(bc.opts.State.CacheDir, cacheKey); ok && bc.opts.State.LayerExists(digest) {
			elapsed := time.Since(t0)
			layerData, _ := bc.opts.State.ReadLayer(digest)
			fmt.Printf("Step %d/%d : %s [CACHE HIT] %.2fs\n", stepNum, bc.totalSteps, instrText, elapsed.Seconds())
			bc.currentLayers = append(bc.currentLayers, image.LayerEntry{
				Digest:    digest,
				Size:      int64(len(layerData)),
				CreatedBy: instrText,
			})
			bc.prevDigest = digest
			return nil
		}
	}

	// Cache miss.
	bc.cacheInvalidated = true
	bc.allCacheHit = false
	tarFiles, err := buildCopyTar(srcFiles, parsed.Dest, bc.workDir)
	if err != nil {
		return err
	}
	tarData, err := store.BuildTar(tarFiles)
	if err != nil {
		return err
	}
	digest, err := bc.opts.State.WriteLayer(tarData)
	if err != nil {
		return err
	}
	if !bc.opts.NoCache {
		_ = cache.Store(bc.opts.State.CacheDir, cacheKey, digest)
	}
	elapsed := time.Since(t0)
	fmt.Printf("Step %d/%d : %s [CACHE MISS] %.2fs\n", stepNum, bc.totalSteps, instrText, elapsed.Seconds())
	bc.currentLayers = append(bc.currentLayers, image.LayerEntry{
		Digest:    digest,
		Size:      int64(len(tarData)),
		CreatedBy: instrText,
	})
	bc.prevDigest = digest
	return nil
}

func (bc *buildContext) execRUN(stepNum int, instr Instruction) error {
	t0 := time.Now()
	instrText := "RUN " + instr.Args

	cacheKey := cache.ComputeKey(cache.KeyParams{
		PrevDigest:  bc.prevDigest,
		Instruction: instrText,
		WorkDir:     bc.workDir,
		Env:         copyEnvMap(bc.envMap),
	})

	if !bc.opts.NoCache && !bc.cacheInvalidated {
		if digest, ok := cache.Lookup(bc.opts.State.CacheDir, cacheKey); ok && bc.opts.State.LayerExists(digest) {
			elapsed := time.Since(t0)
			layerData, _ := bc.opts.State.ReadLayer(digest)
			fmt.Printf("Step %d/%d : %s [CACHE HIT] %.2fs\n", stepNum, bc.totalSteps, instrText, elapsed.Seconds())
			bc.currentLayers = append(bc.currentLayers, image.LayerEntry{
				Digest:    digest,
				Size:      int64(len(layerData)),
				CreatedBy: instrText,
			})
			bc.prevDigest = digest
			return nil
		}
	}

	// Cache miss — assemble rootfs and run.
	bc.cacheInvalidated = true
	bc.allCacheHit = false

	rootfs, err := os.MkdirTemp("", "docksmith-build-*")
	if err != nil {
		return err
	}

	if err := extractLayers(bc.currentLayers, bc.opts.State, rootfs); err != nil {
		os.RemoveAll(rootfs)
		return fmt.Errorf("RUN: extracting layers: %w", err)
	}

	// Ensure workDir exists.
	if bc.workDir != "" {
		os.MkdirAll(filepath.Join(rootfs, bc.workDir), 0755)
	}

	exitCode, runErr := runtime.IsolatedRun(runtime.RunOptions{
		RootFS:       rootfs,
		Command:      []string{"/bin/sh", "-c", instr.Args},
		WorkingDir:   bc.workDir,
		Env:          copyEnvMap(bc.envMap),
		EnvOverrides: nil,
		// An empty network namespace: loopback only, no route out. Network
		// access during a build would make layers depend on whatever a remote
		// server returned at the time, which the cache has no way to detect —
		// the same Docksmithfile would hit the cache and produce a different
		// image. Everything a build needs must come from the context or a
		// previous layer.
		Network: &runtime.NetworkConfig{},
	})

	if runErr != nil {
		os.RemoveAll(rootfs)
		return fmt.Errorf("line %d: RUN failed: %w", instr.LineNum, runErr)
	}
	if exitCode != 0 {
		os.RemoveAll(rootfs)
		return fmt.Errorf("line %d: RUN exited with code %d", instr.LineNum, exitCode)
	}

	// Compute delta layer.
	tarData, err := snapshotDelta(rootfs, bc.currentLayers, bc.opts.State)
	os.RemoveAll(rootfs)
	if err != nil {
		return fmt.Errorf("RUN: snapshot delta: %w", err)
	}

	digest, err := bc.opts.State.WriteLayer(tarData)
	if err != nil {
		return err
	}
	if !bc.opts.NoCache {
		_ = cache.Store(bc.opts.State.CacheDir, cacheKey, digest)
	}
	elapsed := time.Since(t0)
	fmt.Printf("Step %d/%d : %s [CACHE MISS] %.2fs\n", stepNum, bc.totalSteps, instrText, elapsed.Seconds())
	bc.currentLayers = append(bc.currentLayers, image.LayerEntry{
		Digest:    digest,
		Size:      int64(len(tarData)),
		CreatedBy: instrText,
	})
	bc.prevDigest = digest
	return nil
}

// assemble creates the final image manifest and writes it to disk.
func (bc *buildContext) assemble() error {
	// Build final env slice (sorted for reproducibility).
	envSlice := envMapToSlice(bc.envMap)

	// Determine created timestamp.
	createdTime := image.NowISO()
	if bc.allCacheHit && bc.existingManifest != nil {
		createdTime = bc.existingManifest.Created
	}

	// Only store CMD if it was explicitly set via a CMD instruction.
	var finalCmd []string
	if bc.cmdSet {
		finalCmd = bc.cmd
	}

	m := &image.Manifest{
		Name:    bc.name,
		Tag:     bc.tag,
		Created: createdTime,
		Config: image.Config{
			Env:          envSlice,
			Cmd:          finalCmd,
			WorkingDir:   bc.workDir,
			ExposedPorts: bc.exposed,
		},
		Layers: bc.currentLayers,
	}

	if err := image.Save(m, bc.opts.State.ImagesDir); err != nil {
		return fmt.Errorf("saving manifest: %w", err)
	}

	saved, err := image.Load(bc.opts.State.ImagesDir, bc.name, bc.tag)
	if err != nil {
		return err
	}
	shortID := shortDigest(saved.Digest)
	fmt.Printf("Successfully built %s %s:%s (%.2fs)\n", shortID, bc.name, bc.tag, time.Since(bc.startTime).Seconds())
	return nil
}

// AssembleRootFS extracts all image layers into a fresh temp dir.
func AssembleRootFS(m *image.Manifest, st *store.State) (string, error) {
	rootfs, err := os.MkdirTemp("", "docksmith-rootfs-*")
	if err != nil {
		return "", err
	}
	if err := extractLayers(m.Layers, st, rootfs); err != nil {
		os.RemoveAll(rootfs)
		return "", err
	}
	return rootfs, nil
}

// AssembleRootFSInto extracts an image's layers into a caller-supplied
// directory, for containers whose rootfs outlives the process that created it.
func AssembleRootFSInto(m *image.Manifest, st *store.State, dest string) error {
	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}
	return extractLayers(m.Layers, st, dest)
}

// ─── helpers ────────────────────────────────────────────────────────────────

type srcFile struct {
	HostPath string
	RelPath  string
}

func collectGlob(contextDir, pattern string, ignore *IgnoreList) ([]srcFile, error) {
	if strings.Contains(pattern, "**") {
		return collectDoubleGlob(contextDir, pattern, ignore)
	}
	matches, err := filepath.Glob(filepath.Join(contextDir, pattern))
	if err != nil {
		return nil, err
	}
	var out []srcFile
	for _, m := range matches {
		rel, _ := filepath.Rel(contextDir, m)
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if info.IsDir() {
			err := filepath.WalkDir(m, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				r, _ := filepath.Rel(contextDir, path)
				if ignore.Match(r) {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if d.IsDir() {
					return nil
				}
				out = append(out, srcFile{HostPath: path, RelPath: r})
				return nil
			})
			if err != nil {
				return nil, err
			}
		} else if !ignore.Match(rel) {
			out = append(out, srcFile{HostPath: m, RelPath: rel})
		}
	}
	return out, nil
}

func collectDoubleGlob(contextDir, pattern string, ignore *IgnoreList) ([]srcFile, error) {
	var out []srcFile
	err := filepath.WalkDir(contextDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(contextDir, path)
		if rel != "." && ignore.Match(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		matched, _ := filepath.Match(strings.ReplaceAll(pattern, "**", "*"), rel)
		if matched {
			out = append(out, srcFile{HostPath: path, RelPath: rel})
		}
		return nil
	})
	return out, err
}

func buildCopyTar(files []srcFile, dest, workDir string) ([]store.TarFile, error) {
	if !filepath.IsAbs(dest) && workDir != "" {
		dest = filepath.Join(workDir, dest)
	}

	var tarFiles []store.TarFile
	destDirs := make(map[string]bool)

	// Determine if dest is a directory-style path (ends in /) or a rename.
	destIsDir := strings.HasSuffix(dest, "/") || len(files) > 1

	for _, sf := range files {
		data, err := os.ReadFile(sf.HostPath)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(sf.HostPath)
		if err != nil {
			return nil, err
		}

		var archivePath string
		if destIsDir {
			archivePath = filepath.Join(dest, sf.RelPath)
		} else {
			archivePath = dest
		}
		archivePath = strings.TrimPrefix(filepath.Clean(archivePath), "/")

		// Ensure parent directories exist in tar.
		addParentDirs(archivePath, &tarFiles, destDirs, nil)

		tarFiles = append(tarFiles, store.TarFile{
			Path:    archivePath,
			Mode:    int64(info.Mode()),
			IsDir:   false,
			Content: data,
		})
	}
	return tarFiles, nil
}

// addParentDirs emits directory entries for every ancestor of archivePath that
// has not been emitted yet.
//
// existing, when non-nil, maps paths already present in the base layers to
// their signature; an ancestor that is already an unchanged directory there is
// skipped. Re-emitting it would be harmless for content but not for metadata:
// the entry is written with mode 0755, so a delta layer would silently strip
// the sticky bit from an inherited /tmp or downgrade any other directory mode
// it merely happens to sit above.
func addParentDirs(archivePath string, tarFiles *[]store.TarFile, seen map[string]bool, existing map[string]string) {
	dir := filepath.Dir(archivePath)
	if dir == "." || dir == "/" || dir == "" {
		return
	}
	parts := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	for i := range parts {
		d := strings.Join(parts[:i+1], "/")
		if existing[d] == dirSig {
			continue
		}
		if !seen[d] {
			*tarFiles = append(*tarFiles, store.TarFile{
				Path:  d + "/",
				Mode:  0755,
				IsDir: true,
			})
			seen[d] = true
		}
	}
}

func extractLayers(layers []image.LayerEntry, st *store.State, rootfs string) error {
	for _, l := range layers {
		data, err := st.ReadLayer(l.Digest)
		if err != nil {
			return fmt.Errorf("layer %s: %w", shortDigest(l.Digest), err)
		}
		if err := store.ExtractTar(data, rootfs); err != nil {
			return fmt.Errorf("layer %s: %w", shortDigest(l.Digest), err)
		}
	}
	return nil
}

// Signature prefixes. Every signature starts with one of these two-byte tags,
// so comparing the first two bytes of two signatures compares their kind.
const (
	dirSig     = "d:"
	symlinkSig = "l:"
	regularSig = "f:"
	otherSig   = "o:"
)

// entrySignature summarises a filesystem entry as a comparable string encoding
// both its kind and its content, without ever following a symlink.
//
// Kind has to be part of it. The delta is computed by comparing the
// post-execution rootfs against a re-extraction of the base layers, and a
// content-only comparison cannot see a RUN that replaced a file with a
// directory, or a directory with a symlink — cases where writing the new entry
// over the old one at assembly time does not work.
//
// Symlinks are read with Readlink rather than followed. Reading through them,
// which is what this used to do, turned every symlink a RUN step created into a
// full copy of its target's bytes: a busybox image doing `ln -s /bin/busybox
// /bin/ls` gained a megabyte per link and lost the indirection entirely.
//
// Anything that is neither a directory, a symlink, nor a regular file is
// reported as otherSig and never opened. A FIFO is the reason: opening one for
// reading blocks until a writer appears, which inside a build is never, so the
// build hangs with no timeout and no diagnostic.
func entrySignature(path string, d fs.DirEntry) (string, error) {
	switch {
	case d.IsDir():
		return dirSig, nil
	case d.Type()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		return symlinkSig + target, nil
	case d.Type().IsRegular():
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		h := sha256.Sum256(data)
		return regularSig + hex.EncodeToString(h[:]), nil
	default:
		return otherSig, nil
	}
}

// snapshotDelta builds a tar of files in rootfs that differ from the base layers.
func snapshotDelta(rootfs string, baseLayers []image.LayerEntry, st *store.State) ([]byte, error) {
	// Build a reference snapshot from base layers.
	refDir, err := os.MkdirTemp("", "docksmith-ref-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(refDir)

	if err := extractLayers(baseLayers, st, refDir); err != nil {
		return nil, err
	}

	// Signature of every entry in the reference, keyed by rootfs-relative path.
	refSigs := make(map[string]string)
	_ = filepath.WalkDir(refDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(refDir, path)
		if rel == "." {
			return nil
		}
		sig, err := entrySignature(path, d)
		if err != nil {
			return nil
		}
		refSigs[rel] = sig
		return nil
	})

	var tarFiles []store.TarFile
	dirsSeen := make(map[string]bool)

	err = filepath.WalkDir(rootfs, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(rootfs, path)
		if rel == "." {
			return nil
		}
		if isVirtualPath(rel) {
			return filepath.SkipDir
		}

		sig, err := entrySignature(path, d)
		if err != nil {
			return nil
		}
		refSig, existed := refSigs[rel]
		if existed && refSig == sig {
			return nil // unchanged
		}

		// A change of *kind* cannot be expressed by simply writing the new
		// entry on top. Extraction would try to create a regular file where a
		// directory from a lower layer already sits, or leave a stale directory
		// shadowing a new symlink — either way the layer builds fine and then
		// fails, or silently misbehaves, at assembly time. Whiteout first;
		// BuildTar sorts every whiteout ahead of all content, so the removal is
		// guaranteed to happen before the replacement is written.
		if existed && refSig[:2] != sig[:2] {
			tarFiles = append(tarFiles, store.TarFile{
				Path:       rel,
				Mode:       0644,
				IsWhiteout: true,
			})
		}

		info, _ := d.Info()
		mode := int64(0644)
		if info != nil {
			mode = int64(info.Mode().Perm())
		}

		switch {
		case d.IsDir():
			if !dirsSeen[rel] {
				addParentDirs(rel, &tarFiles, dirsSeen, refSigs)
				tarFiles = append(tarFiles, store.TarFile{
					Path:  rel + "/",
					Mode:  mode,
					IsDir: true,
				})
				dirsSeen[rel] = true
			}
		case d.Type()&fs.ModeSymlink != 0:
			addParentDirs(rel, &tarFiles, dirsSeen, refSigs)
			tarFiles = append(tarFiles, store.TarFile{
				Path:      rel,
				Mode:      mode,
				IsSymlink: true,
				Linkname:  strings.TrimPrefix(sig, symlinkSig),
			})
		case d.Type().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			addParentDirs(rel, &tarFiles, dirsSeen, refSigs)
			tarFiles = append(tarFiles, store.TarFile{
				Path:    rel,
				Mode:    mode,
				Content: data,
			})
		default:
			// FIFO, socket or device node. Not representable in a docksmith
			// layer, and never read — see entrySignature.
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Second walk, over the reference: anything present in the base layers but
	// absent from the post-execution rootfs was deleted by this step, and must
	// be recorded as a whiteout. Without this the delta only ever adds, so a
	// RUN that removes a file produces a layer that deletes nothing and the
	// file reappears when layers are reassembled.
	err = filepath.WalkDir(refDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(refDir, p)
		if rel == "." {
			return nil
		}
		if isVirtualPath(rel) {
			return filepath.SkipDir
		}
		// Lstat, not Stat: a busybox rootfs is mostly symlinks, and Stat would
		// follow a link whose target is missing (or lives under a skipped
		// prefix) and wrongly report the link itself as deleted.
		if _, err := os.Lstat(filepath.Join(rootfs, rel)); err == nil {
			return nil // still present
		} else if !os.IsNotExist(err) {
			return nil // unreadable for some other reason — do not guess
		}
		tarFiles = append(tarFiles, store.TarFile{
			Path:       rel,
			Mode:       0644,
			IsWhiteout: true,
		})
		if d.IsDir() {
			// One marker deletes the whole subtree on extraction, so there is
			// no point enumerating what is underneath it.
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return store.BuildTar(tarFiles)
}

// isVirtualPath reports whether a rootfs-relative path belongs to a kernel
// filesystem mounted into the container rather than to image content. These
// are never captured in a layer.
func isVirtualPath(rel string) bool {
	switch rel {
	case "proc", "sys", "dev":
		return true
	case ".oldroot":
		// Scratch mount point used by pivot_root. It is unmounted and removed
		// before the command runs, but if a teardown ever fails it must not be
		// captured into a layer.
		return true
	}
	return strings.HasPrefix(rel, "proc/") ||
		strings.HasPrefix(rel, "sys/") ||
		strings.HasPrefix(rel, "dev/")
}

func copyEnvMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func envMapToSlice(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(m))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

func shortDigest(d string) string {
	if len(d) >= 19 {
		return d[:19] // "sha256:" + 12 chars
	}
	return d
}
