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

	bc := &buildContext{
		opts:             opts,
		name:             name,
		tag:              tag,
		totalSteps:       len(instrs),
		envMap:           make(map[string]string),
		allCacheHit:      true,
		existingManifest: existing,
		startTime:        time.Now(),
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

func (bc *buildContext) execCOPY(stepNum int, instr Instruction) error {
	t0 := time.Now()
	instrText := "COPY " + instr.Args
	parsed, err := instr.AsCOPY()
	if err != nil {
		return err
	}

	// Collect sources.
	srcFiles, err := collectGlob(bc.opts.ContextDir, parsed.Src)
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
			Env:        envSlice,
			Cmd:        finalCmd,
			WorkingDir: bc.workDir,
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

// ─── helpers ────────────────────────────────────────────────────────────────

type srcFile struct {
	HostPath string
	RelPath  string
}

func collectGlob(contextDir, pattern string) ([]srcFile, error) {
	if strings.Contains(pattern, "**") {
		return collectDoubleGlob(contextDir, pattern)
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
				if err != nil || d.IsDir() {
					return err
				}
				r, _ := filepath.Rel(contextDir, path)
				out = append(out, srcFile{HostPath: path, RelPath: r})
				return nil
			})
			if err != nil {
				return nil, err
			}
		} else {
			out = append(out, srcFile{HostPath: m, RelPath: rel})
		}
	}
	return out, nil
}

func collectDoubleGlob(contextDir, pattern string) ([]srcFile, error) {
	var out []srcFile
	err := filepath.WalkDir(contextDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(contextDir, path)
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
		addParentDirs(archivePath, &tarFiles, destDirs)

		tarFiles = append(tarFiles, store.TarFile{
			Path:    archivePath,
			Mode:    int64(info.Mode()),
			IsDir:   false,
			Content: data,
		})
	}
	return tarFiles, nil
}

func addParentDirs(archivePath string, tarFiles *[]store.TarFile, seen map[string]bool) {
	dir := filepath.Dir(archivePath)
	if dir == "." || dir == "/" || dir == "" {
		return
	}
	parts := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	for i := range parts {
		d := strings.Join(parts[:i+1], "/")
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

	// Snapshot refDir file hashes.
	refHashes := make(map[string]string)
	_ = filepath.WalkDir(refDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(refDir, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		h := sha256.Sum256(data)
		refHashes[rel] = hex.EncodeToString(h[:])
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
		// Skip virtual filesystems.
		if rel == "proc" || strings.HasPrefix(rel, "proc/") ||
			rel == "sys" || strings.HasPrefix(rel, "sys/") ||
			rel == "dev" || strings.HasPrefix(rel, "dev/") {
			return filepath.SkipDir
		}

		if d.IsDir() {
			// Include new dirs in the delta.
			if _, err := os.Stat(filepath.Join(refDir, rel)); os.IsNotExist(err) {
				if !dirsSeen[rel] {
					tarFiles = append(tarFiles, store.TarFile{
						Path:  rel + "/",
						Mode:  0755,
						IsDir: true,
					})
					dirsSeen[rel] = true
				}
			}
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		h := sha256.Sum256(data)
		currentHash := hex.EncodeToString(h[:])

		if refHash, exists := refHashes[rel]; exists && refHash == currentHash {
			return nil // unchanged
		}

		// New or modified file — add parent dirs.
		info, _ := d.Info()
		mode := int64(0644)
		if info != nil {
			mode = int64(info.Mode())
		}
		addParentDirs(rel, &tarFiles, dirsSeen)
		tarFiles = append(tarFiles, store.TarFile{
			Path:    rel,
			Mode:    mode,
			Content: data,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	return store.BuildTar(tarFiles)
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
