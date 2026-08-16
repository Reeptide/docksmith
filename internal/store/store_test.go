package store

import (
	"archive/tar"
	"bytes"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func sampleFiles() []TarFile {
	return []TarFile{
		{Path: "app/", Mode: 0755, IsDir: true},
		{Path: "app/main.sh", Mode: 0755, Content: []byte("#!/bin/sh\necho hi\n")},
		{Path: "app/config.txt", Mode: 0644, Content: []byte("key=value")},
		{Path: "bin/", Mode: 0755, IsDir: true},
		{Path: "bin/tool", Mode: 0755, Content: []byte("binary-ish")},
		{Path: "bin/link", Mode: 0777, IsSymlink: true, Linkname: "tool"},
	}
}

// Layer digests are the SHA-256 of the tar bytes, and the build cache assumes
// identical inputs always produce identical bytes. If input ordering leaked
// into the output, cache hits would depend on filesystem walk order.
func TestBuildTarDeterministicUnderShuffledInput(t *testing.T) {
	want, err := BuildTar(sampleFiles())
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		files := sampleFiles()
		rng.Shuffle(len(files), func(a, b int) { files[a], files[b] = files[b], files[a] })
		got, err := BuildTar(files)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("shuffle %d produced different tar bytes (%d vs %d)", i, len(got), len(want))
		}
	}
}

func TestBuildTarStableAcrossCalls(t *testing.T) {
	a, err := BuildTar(sampleFiles())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // any wall-clock dependence would show here
	b, err := BuildTar(sampleFiles())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("two BuildTar calls with identical input produced different bytes")
	}
	if DigestBytes(a) != DigestBytes(b) {
		t.Error("digests differ for identical input")
	}
}

// Host metadata must never reach a layer: mtimes, uid/gid, and user names all
// vary between machines and would make digests unreproducible.
func TestBuildTarZeroesHostMetadata(t *testing.T) {
	data, err := BuildTar(sampleFiles())
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(data))
	seen := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen++
		// BuildTar writes time.Time{}, but the GNU format stores seconds since
		// the epoch, so it reads back as Unix 0 rather than Go's zero time.
		// What matters is that it is a fixed constant, never the host clock.
		if hdr.ModTime.Unix() != 0 {
			t.Errorf("%s: ModTime = %v (unix %d), want the epoch", hdr.Name, hdr.ModTime, hdr.ModTime.Unix())
		}
		if hdr.Uid != 0 || hdr.Gid != 0 {
			t.Errorf("%s: uid/gid = %d/%d, want 0/0", hdr.Name, hdr.Uid, hdr.Gid)
		}
		if hdr.Uname != "" || hdr.Gname != "" {
			t.Errorf("%s: uname/gname = %q/%q, want empty", hdr.Name, hdr.Uname, hdr.Gname)
		}
	}
	if seen != len(sampleFiles()) {
		t.Errorf("wrote %d entries, want %d", seen, len(sampleFiles()))
	}
}

// The host umask must not change layer bytes — the same source tree built on
// two differently-configured machines has to yield the same digest.
//
// BuildTar is a pure function of its argument and never touches the
// filesystem, so this half can only ever pass; it stands as documentation.
// TestExtractTarIgnoresHostUmask below is the half that can fail, since
// extraction does create files and OpenFile's mode argument *is* umask-masked.
func TestBuildTarIgnoresHostUmask(t *testing.T) {
	old := syscall.Umask(0)
	a, err := BuildTar(sampleFiles())
	syscall.Umask(0077)
	b, err2 := BuildTar(sampleFiles())
	syscall.Umask(old)
	if err != nil || err2 != nil {
		t.Fatal(err, err2)
	}
	if !bytes.Equal(a, b) {
		t.Error("host umask leaked into layer bytes")
	}
}

func TestTarRoundTripPreservesContentModesAndSymlinks(t *testing.T) {
	data, err := BuildTar(sampleFiles())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := ExtractTar(data, dir); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "app/main.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "#!/bin/sh\necho hi\n" {
		t.Errorf("app/main.sh = %q", body)
	}

	info, err := os.Stat(filepath.Join(dir, "app/main.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0100 == 0 {
		t.Errorf("app/main.sh lost its executable bit: %v", info.Mode())
	}

	target, err := os.Readlink(filepath.Join(dir, "bin/link"))
	if err != nil {
		t.Fatalf("bin/link is not a symlink: %v", err)
	}
	if target != "tool" {
		t.Errorf("bin/link -> %q, want tool", target)
	}
}

// Layers are untrusted input once `load` exists, so a crafted entry must not
// be able to write outside the destination directory.
func TestExtractTarCannotEscapeDestDir(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{
		"../escaped.txt",
		"../../escaped2.txt",
		"a/../../escaped3.txt",
		"/absolute.txt",
	} {
		body := []byte("pwned")
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	parent := t.TempDir()
	dest := filepath.Join(parent, "rootfs")
	if err := os.Mkdir(dest, 0755); err != nil {
		t.Fatal(err)
	}
	if err := ExtractTar(buf.Bytes(), dest); err != nil {
		t.Fatal(err)
	}

	for _, escaped := range []string{
		filepath.Join(parent, "escaped.txt"),
		filepath.Join(parent, "escaped2.txt"),
		filepath.Join(parent, "escaped3.txt"),
		filepath.Dir(parent) + "/escaped2.txt",
	} {
		if _, err := os.Lstat(escaped); err == nil {
			t.Errorf("tar entry escaped the destination: %s", escaped)
		}
	}
	// Everything should have been clamped inside dest instead.
	if _, err := os.Lstat(filepath.Join(dest, "escaped.txt")); err != nil {
		t.Errorf("expected the entry to be clamped into dest: %v", err)
	}
}

// A whiteout must not be able to delete outside the destination either.
func TestExtractTarWhiteoutCannotEscapeDestDir(t *testing.T) {
	parent := t.TempDir()
	victim := filepath.Join(parent, "victim.txt")
	if err := os.WriteFile(victim, []byte("important"), 0644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(parent, "rootfs")
	if err := os.Mkdir(dest, 0755); err != nil {
		t.Fatal(err)
	}

	data, err := BuildTar([]TarFile{{Path: "../victim.txt", Mode: 0644, IsWhiteout: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ExtractTar(data, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Errorf("whiteout escaped the destination and deleted %s", victim)
	}
}

func TestWriteLayerIsContentAddressedAndIdempotent(t *testing.T) {
	st, err := NewState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := BuildTar(sampleFiles())
	if err != nil {
		t.Fatal(err)
	}

	d1, err := st.WriteLayer(data)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := st.WriteLayer(data)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Errorf("same bytes produced different digests: %s vs %s", d1, d2)
	}
	if d1 != DigestBytes(data) {
		t.Errorf("layer digest %s does not match DigestBytes %s", d1, DigestBytes(data))
	}
	if !st.LayerExists(d1) {
		t.Error("LayerExists false for a layer just written")
	}

	got, err := st.ReadLayer(d1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Error("ReadLayer returned different bytes than were written")
	}

	if err := st.DeleteLayer(d1); err != nil {
		t.Fatal(err)
	}
	if st.LayerExists(d1) {
		t.Error("LayerExists true after DeleteLayer")
	}
}

func TestBuildTarEmptyInput(t *testing.T) {
	data, err := BuildTar(nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := ExtractTar(data, dir); err != nil {
		t.Fatalf("extracting an empty layer should succeed: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("empty layer produced %d entries", len(entries))
	}
}

// A whiteout must delete the entry at its path, never what that entry points
// at. Layer extraction resolved the final component through symlinks, so on a
// busybox rootfs — where /bin is almost entirely links into /bin/busybox — a
// `RUN rm /bin/ls` produced a whiteout that deleted /bin/busybox and took the
// shell, and every other applet, with it. The layer itself was correct; the
// damage happened at extraction, so every image built on busybox was affected.
func TestWhiteoutRemovesTheSymlinkNotItsTarget(t *testing.T) {
	dir := t.TempDir()
	base, err := BuildTar([]TarFile{
		{Path: "bin/", Mode: 0755, IsDir: true},
		{Path: "bin/busybox", Mode: 0755, Content: []byte("the only real binary")},
		{Path: "bin/sh", Mode: 0777, IsSymlink: true, Linkname: "busybox"},
		{Path: "bin/ls", Mode: 0777, IsSymlink: true, Linkname: "busybox"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ExtractTar(base, dir); err != nil {
		t.Fatal(err)
	}

	delta, err := BuildTar([]TarFile{{Path: "bin/ls", Mode: 0644, IsWhiteout: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ExtractTar(delta, dir); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(filepath.Join(dir, "bin/ls")); !os.IsNotExist(err) {
		t.Errorf("bin/ls should be gone: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "bin/busybox"))
	if err != nil {
		t.Fatalf("the whiteout deleted the symlink's target: %v", err)
	}
	if string(data) != "the only real binary" {
		t.Errorf("bin/busybox = %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin/sh")); err != nil {
		t.Errorf("an unrelated link into the same target broke: %v", err)
	}
}

// The same hazard for content: a later layer writing a regular file at a path
// held by a symlink must replace the link, not write through it and silently
// corrupt whatever it pointed at.
func TestEntryReplacingASymlinkDoesNotWriteThroughIt(t *testing.T) {
	dir := t.TempDir()
	base, err := BuildTar([]TarFile{
		{Path: "bin/", Mode: 0755, IsDir: true},
		{Path: "bin/busybox", Mode: 0755, Content: []byte("original")},
		{Path: "bin/ls", Mode: 0777, IsSymlink: true, Linkname: "busybox"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ExtractTar(base, dir); err != nil {
		t.Fatal(err)
	}

	delta, err := BuildTar([]TarFile{
		{Path: "bin/ls", Mode: 0755, Content: []byte("a real ls now")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ExtractTar(delta, dir); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(filepath.Join(dir, "bin/ls"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("bin/ls is still a symlink; the new content went to its target")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "bin/ls")); string(data) != "a real ls now" {
		t.Errorf("bin/ls = %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "bin/busybox")); string(data) != "original" {
		t.Errorf("bin/busybox was overwritten through the link: %q", data)
	}
}

// os.FileMode keeps setuid, setgid and sticky outside Perm(), in bits that do
// not match the Unix layout, so a plain Perm() or int64 conversion drops them.
// Both matter in a real base image: busybox is frequently setuid, and /tmp is
// sticky. A layer that silently clears either changes what the image can do.
func TestTarRoundTripPreservesSpecialModeBits(t *testing.T) {
	cases := []struct {
		name string
		mode os.FileMode
	}{
		{"setuid binary", 0755 | os.ModeSetuid},
		{"setgid binary", 0755 | os.ModeSetgid},
		{"sticky directory", 0777 | os.ModeSticky},
		{"plain", 0644},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FileMode(TarMode(c.mode)); got != c.mode {
				t.Errorf("FileMode(TarMode(%v)) = %v", c.mode, got)
			}
		})
	}

	dir := t.TempDir()
	data, err := BuildTar([]TarFile{
		{Path: "tmp/", Mode: TarMode(0777 | os.ModeSticky), IsDir: true},
		{Path: "bin/busybox", Mode: TarMode(0755 | os.ModeSetuid), Content: []byte("x")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ExtractTar(data, dir); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, "tmp")); err != nil {
		t.Fatal(err)
	} else if info.Mode()&os.ModeSticky == 0 {
		t.Errorf("sticky bit lost on /tmp: %v", info.Mode())
	}
	if info, err := os.Stat(filepath.Join(dir, "bin/busybox")); err != nil {
		t.Fatal(err)
	} else if info.Mode()&os.ModeSetuid == 0 {
		t.Errorf("setuid bit lost on busybox: %v", info.Mode())
	}
}

// The umask assertion that can actually fail. os.OpenFile and os.Mkdir mask
// their mode argument against the process umask, so extraction under umask 077
// would produce 0600 files and 0700 directories unless every entry is chmodded
// explicitly afterwards. A container built on a hardened host would then ship
// files its own non-root processes cannot read.
func TestExtractTarIgnoresHostUmask(t *testing.T) {
	data, err := BuildTar([]TarFile{
		{Path: "app/", Mode: 0755, IsDir: true},
		{Path: "app/run.sh", Mode: 0755, Content: []byte("#!/bin/sh\n")},
		{Path: "app/data.txt", Mode: 0644, Content: []byte("x")},
	})
	if err != nil {
		t.Fatal(err)
	}

	old := syscall.Umask(0077)
	defer syscall.Umask(old)

	dir := t.TempDir()
	if err := ExtractTar(data, dir); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path string
		want os.FileMode
	}{
		{"app", 0755},
		{"app/run.sh", 0755},
		{"app/data.txt", 0644},
	} {
		info, err := os.Stat(filepath.Join(dir, c.path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != c.want {
			t.Errorf("%s = %v under umask 077, want %v", c.path, info.Mode().Perm(), c.want)
		}
	}
}
