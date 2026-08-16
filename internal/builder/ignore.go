package builder

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// IgnoreFileName is read from the build context root.
const IgnoreFileName = ".docksmithignore"

// ignorePattern is one parsed rule.
type ignorePattern struct {
	pattern string // cleaned, slash-separated, no leading "./" or trailing "/"
	negate  bool   // "!" prefix — re-includes a path an earlier rule excluded
	dirOnly bool   // trailing "/" — matches a directory and everything under it
}

// IgnoreList decides whether a context-relative path is excluded from a build.
//
// This matters for more than build speed. COPY cache keys hash the SHA-256 of
// every matched source file, so without an ignore list an unrelated file
// landing in the context — a .git object, an editor swap file, a previous
// build's output — silently changes the cache key and invalidates every
// downstream step for a reason the user cannot see.
type IgnoreList struct {
	patterns []ignorePattern
}

// LoadIgnoreList reads .docksmithignore from contextDir. A missing file is not
// an error: it yields an empty list that excludes nothing.
func LoadIgnoreList(contextDir string) (*IgnoreList, error) {
	f, err := os.Open(filepath.Join(contextDir, IgnoreFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return &IgnoreList{}, nil
		}
		return nil, err
	}
	defer f.Close()

	il := &IgnoreList{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := ignorePattern{}
		if strings.HasPrefix(line, "!") {
			p.negate = true
			line = strings.TrimSpace(line[1:])
			if line == "" {
				continue
			}
		}
		if strings.HasSuffix(line, "/") {
			p.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		p.pattern = strings.TrimPrefix(filepath.ToSlash(line), "./")
		if p.pattern == "" {
			continue
		}
		il.patterns = append(il.patterns, p)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return il, nil
}

// Match reports whether a context-relative path is excluded. Later rules win,
// so a "!" rule can re-include something an earlier rule excluded.
//
// isDir is needed for trailing-slash rules: "build/" names a directory and must
// not exclude a regular file that happens to be called "build". Without it the
// dirOnly flag was parsed and then never consulted, so the slash meant nothing.
func (il *IgnoreList) Match(rel string, isDir bool) bool {
	rel = filepath.ToSlash(rel)

	// The ignore file never enters an image, and neither does docksmith's own
	// state directory if someone builds with their home as the context.
	if rel == IgnoreFileName {
		return true
	}

	excluded := false
	for _, p := range il.patterns {
		if p.matches(rel, isDir) {
			excluded = !p.negate
		}
	}
	return excluded
}

// matches reports whether rel is covered by this rule, either directly or by
// sitting underneath a matched directory.
func (p ignorePattern) matches(rel string, isDir bool) bool {
	if matchPath(p.pattern, rel) {
		// A trailing-slash rule names a directory, so it does not exclude a
		// file of the same name.
		return !p.dirOnly || isDir
	}
	// A rule matches everything beneath a directory it names: "build/" covers
	// "build/out/app". Walk the ancestors rather than string-prefixing, so
	// "build" does not accidentally match "buildkit/x". Every ancestor is a
	// directory by definition, so dirOnly is satisfied here regardless.
	for dir := pathDir(rel); dir != ""; dir = pathDir(dir) {
		if matchPath(p.pattern, dir) {
			return true
		}
	}
	return false
}

// matchPath applies one pattern to one path. "**" matches across separators;
// a single "*" does not. A pattern with no separator matches by basename
// anywhere in the tree, which is what makes "*.log" and ".git" behave the way
// people expect.
func matchPath(pattern, rel string) bool {
	if strings.Contains(pattern, "**") {
		return matchDoubleStar(pattern, rel)
	}
	if !strings.Contains(pattern, "/") {
		if ok, _ := filepath.Match(pattern, filepath.Base(rel)); ok {
			return true
		}
	}
	ok, _ := filepath.Match(pattern, rel)
	return ok
}

// matchDoubleStar handles patterns containing "**" by splitting on it and
// walking the segments in order, anchored at each end.
func matchDoubleStar(pattern, rel string) bool {
	parts := strings.Split(pattern, "**")
	pos := 0
	for i, part := range parts {
		part = strings.Trim(part, "/")
		if part == "" {
			continue
		}
		idx := indexSegment(rel[pos:], part, i == 0)
		if idx < 0 {
			return false
		}
		pos += idx
	}
	return true
}

// indexSegment finds a glob segment within rel, optionally anchored at the
// start. Returns the offset just past the match, or -1.
func indexSegment(rel, part string, anchored bool) int {
	segs := strings.Split(rel, "/")
	offset := 0
	for i, seg := range segs {
		if anchored && i > 0 {
			return -1
		}
		if ok, _ := filepath.Match(part, seg); ok {
			return offset + len(seg)
		}
		offset += len(seg) + 1
	}
	return -1
}

// pathDir returns the parent of a slash-separated path, or "" at the root.
func pathDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return ""
	}
	return p[:i]
}
