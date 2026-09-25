// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package overlay

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// fakeBase is a read-only filesystem the test writes: what is under test is the
// merging, the shadowing and the seal, none of which is an archive format.
type fakeBase struct {
	files  map[string]string
	closed bool
}

func (b *fakeBase) Close() error { b.closed = true; return nil }

func (b *fakeBase) ListDir(dir string) ([]filesystem.DirEntry, error) {
	kinds := map[string]bool{}
	for p := range b.files {
		if dir != "" && !strings.HasPrefix(p, dir+"/") {
			continue
		}
		rest := p
		if dir != "" {
			rest = p[len(dir)+1:]
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			kinds[rest[:i]] = true
		} else {
			kinds[rest] = false
		}
	}
	if len(kinds) == 0 && dir != "" {
		return nil, ErrNotFound
	}
	names := make([]string, 0, len(kinds))
	for n := range kinds {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]filesystem.DirEntry, 0, len(names))
	for i, n := range names {
		var ft uint8
		if kinds[n] {
			ft = fileTypeDir
		}
		out = append(out, filesystem.NewDirEntry(uint64(i+1), n, ft))
	}
	return out, nil
}

func (b *fakeBase) Stat(p string) (filesystem.Stat, error) {
	if body, ok := b.files[p]; ok {
		return filesystem.NewStat(0o100644, uint64(len(body)), 0), nil
	}
	for q := range b.files {
		if strings.HasPrefix(q, p+"/") {
			return filesystem.NewStat(0o040755, 0, 0), nil
		}
	}
	return nil, ErrNotFound
}

func (b *fakeBase) ReadFile(p string) ([]byte, error) {
	body, ok := b.files[p]
	if !ok {
		return nil, ErrNotFound
	}
	return []byte(body), nil
}

func (b *fakeBase) ReadLink(string) (string, error)             { return "", errors.New("no links") }
func (b *fakeBase) WriteFile(string, []byte, os.FileMode) error { return errors.New("read-only") }
func (b *fakeBase) MkDir(string, os.FileMode) error             { return errors.New("read-only") }
func (b *fakeBase) DeleteFile(string) error                     { return errors.New("read-only") }
func (b *fakeBase) DeleteDir(string) error                      { return errors.New("read-only") }
func (b *fakeBase) Rename(string, string) error                 { return errors.New("read-only") }

// recorder is a Builder that remembers what it was handed.
type recorder struct {
	files   map[string]string
	dirs    []string
	failOn  string // the entry to fail at, for the interruption test
	written int
}

func (r *recorder) AddDir(name string, _ os.FileMode) error {
	r.dirs = append(r.dirs, name)
	return nil
}

func (r *recorder) AddFile(name string, _ os.FileMode, src io.Reader) error {
	if r.failOn != "" && name == r.failOn {
		return errors.New("the disk filled up")
	}
	b, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	if r.files == nil {
		r.files = map[string]string{}
	}
	r.files[name] = string(b)
	r.written++
	return nil
}

func newOverlay(t *testing.T, files map[string]string) (*Overlay, *fakeBase) {
	t.Helper()
	base := &fakeBase{files: files}
	o, err := New(base, WithSpool(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	return o, base
}

// TestAnInterruptedSealLeavesTheOriginalAlone is the property the rename exists
// for, and the reason sealing is not done in place.
//
// A rewrite that opened the target and started writing would destroy the archive
// in order to replace it, and the window is the WHOLE rewrite -- minutes, for the
// archives people have. A full disk or a signal in that window is data loss, not
// a failed operation. So the new archive is written beside the target and moved
// over it, and this test fills the disk halfway through on purpose.
func TestAnInterruptedSealLeavesTheOriginalAlone(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "archive.bin")
	const original = "the original archive, every byte of it"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	o, _ := newOverlay(t, map[string]string{"a.txt": "one", "b.txt": "two", "c.txt": "three"})
	defer o.Close()
	if err := o.WriteFile("b.txt", []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := &recorder{failOn: "b.txt"}
	err := o.SealToFile(target, func(io.WriteSeeker) (Builder, error) { return rec, nil })
	if err == nil {
		t.Fatal("the seal reported success though the builder failed")
	}

	// The whole point:
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("the original is gone: %v", readErr)
	}
	if string(got) != original {
		t.Errorf("the original now reads %q, want it untouched", got)
	}

	// And no half-written file was left behind to be mistaken for an archive.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "archive.bin" {
			t.Errorf("a leftover %q was left in the directory", e.Name())
		}
	}
}

// TestOnlyWhatChangedIsCopied is the claim that makes this worth doing: an edit
// to a large archive must cost the edit, not the archive.
func TestOnlyWhatChangedIsCopied(t *testing.T) {
	big := strings.Repeat("x", 4096)
	o, _ := newOverlay(t, map[string]string{
		"big1.bin": big, "big2.bin": big, "big3.bin": big, "small.txt": "hi",
	})
	defer o.Close()

	if err := o.WriteFile("small.txt", []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	spooled, err := os.ReadDir(o.Spool())
	if err != nil {
		t.Fatal(err)
	}
	if len(spooled) != 1 {
		t.Errorf("%d files in the spool after one edit, want 1", len(spooled))
	}
	var bytesSpooled int64
	for _, e := range spooled {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		bytesSpooled += info.Size()
	}
	if bytesSpooled > 64 {
		t.Errorf("%d bytes spooled for a six-byte edit", bytesSpooled)
	}

	// And the untouched entries still arrive at the builder, from the base.
	rec := &recorder{}
	if err := o.Seal(rec); err != nil {
		t.Fatal(err)
	}
	if rec.files["big1.bin"] != big {
		t.Error("an untouched entry did not reach the builder from the base")
	}
	if rec.files["small.txt"] != "edited" {
		t.Errorf("the edited entry sealed as %q", rec.files["small.txt"])
	}
}

// TestTheUpperLayerWinsAndADeletionShadows covers the three answers a merged
// view has to give.
func TestTheUpperLayerWinsAndADeletionShadows(t *testing.T) {
	o, _ := newOverlay(t, map[string]string{
		"keep.txt":       "from the base",
		"replace.txt":    "from the base",
		"gone.txt":       "from the base",
		"dir/inside.txt": "from the base",
	})
	defer o.Close()

	if err := o.WriteFile("replace.txt", []byte("from the overlay"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := o.DeleteFile("gone.txt"); err != nil {
		t.Fatal(err)
	}
	if err := o.DeleteDir("dir"); err != nil {
		t.Fatal(err)
	}
	if err := o.WriteFile("added.txt", []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	for p, want := range map[string]string{
		"keep.txt":    "from the base",
		"replace.txt": "from the overlay",
		"added.txt":   "new",
	} {
		got, err := o.ReadFile(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	if _, err := o.ReadFile("gone.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deleted file answered %v", err)
	}
	// A deleted directory shadows what is under it, which a deletion that only
	// forgot the directory itself would not.
	if _, err := o.ReadFile("dir/inside.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a file under a deleted directory answered %v, want ErrNotFound", err)
	}

	// The listing agrees with the reads.
	entries, err := o.ListDir("")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "added.txt,keep.txt,replace.txt" {
		t.Errorf("the root lists %v", names)
	}

	// And the seal sees the same world the reads did.
	rec := &recorder{}
	if err := o.Seal(rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.files["gone.txt"]; ok {
		t.Error("a deleted entry was sealed into the new archive")
	}
	if _, ok := rec.files["dir/inside.txt"]; ok {
		t.Error("a file under a deleted directory was sealed")
	}
	if rec.files["added.txt"] != "new" {
		t.Error("an added entry was not sealed")
	}
}

// TestCloseDiscardsTheSpoolAndSaysSo: Seal is the commit, and Close is not.
func TestCloseDiscardsTheSpoolAndSaysSo(t *testing.T) {
	base := &fakeBase{files: map[string]string{"a.txt": "one"}}
	o, err := New(base) // its own temporary spool this time
	if err != nil {
		t.Fatal(err)
	}
	spool := o.Spool()
	if err := o.WriteFile("a.txt", []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spool); err != nil {
		t.Fatalf("the spool is not there: %v", err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spool); !os.IsNotExist(err) {
		t.Errorf("the spool survived Close: %v", err)
	}
	if !base.closed {
		t.Error("Close did not close the base")
	}
	if err := o.Close(); !errors.Is(err, ErrClosed) {
		t.Errorf("a second Close gave %v, want ErrClosed", err)
	}
	if err := o.WriteFile("b.txt", nil, 0); !errors.Is(err, ErrClosed) {
		t.Errorf("writing after Close gave %v, want ErrClosed", err)
	}
}

// TestASpoolTheCallerOwnsIsLeftAlone: WithSpool hands the directory over, so
// Close must not delete somebody else's.
func TestASpoolTheCallerOwnsIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	o, _ := newOverlay(t, map[string]string{"a.txt": "one"})
	_ = o.Close()

	base := &fakeBase{files: map[string]string{"a.txt": "one"}}
	o2, err := New(base, WithSpool(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := o2.WriteFile("a.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := o2.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("Close removed a spool the caller owns: %v", err)
	}
	if _, err := os.Stat(path.Join(dir, "e1")); err != nil {
		t.Errorf("the working copy was removed: %v", err)
	}
}

// TestTheRestOfTheContractAnswers covers the methods a Filesystem promises and
// that the seal tests never touch. An untested contract method is the kind that
// breaks without anything noticing: nothing in this package calls Stat, so only
// a caller would find it wrong.
func TestTheRestOfTheContractAnswers(t *testing.T) {
	o, _ := newOverlay(t, map[string]string{
		"base.txt":       "from the base",
		"dir/inside.txt": "deeper",
	})
	defer o.Close()

	if err := o.WriteFile("added.txt", []byte("six!!!"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := o.MkDir("newdir", 0o750); err != nil {
		t.Fatal(err)
	}

	// Stat: the type lives in the TOP bits of the mode, which is where a uint16
	// keeps it. Narrowing an os.FileMode into one loses every type bit, so a
	// directory that cannot say it is a directory is the failure to watch for.
	for _, c := range []struct {
		path string
		typ  uint16
		perm uint16
	}{
		{"added.txt", 0o100000, 0o640},
		{"newdir", 0o040000, 0o750},
		{"base.txt", 0o100000, 0o644},
	} {
		st, err := o.Stat(c.path)
		if err != nil {
			t.Errorf("Stat(%q): %v", c.path, err)
			continue
		}
		if st.Mode()&0o170000 != c.typ {
			t.Errorf("Stat(%q) mode %#o does not say type %#o", c.path, st.Mode(), c.typ)
		}
		if st.Mode()&0o777 != c.perm {
			t.Errorf("Stat(%q) permissions %#o, want %#o", c.path, st.Mode()&0o777, c.perm)
		}
	}
	if st, err := o.Stat("added.txt"); err == nil && st.Size() != 6 {
		t.Errorf("Stat size %d, want 6", st.Size())
	}
	if _, err := o.Stat("nowhere.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat of an absent path gave %v", err)
	}

	// MkDir refuses a path already taken, in either layer.
	if err := o.MkDir("newdir", 0o755); !errors.Is(err, ErrExists) {
		t.Errorf("MkDir twice gave %v, want ErrExists", err)
	}
	if err := o.MkDir("base.txt", 0o755); !errors.Is(err, ErrExists) {
		t.Errorf("MkDir over a base entry gave %v, want ErrExists", err)
	}

	// OpenFile: a working copy is an ordinary file, so it answers ReadAt from
	// the middle without any of the cost an archive entry carries.
	h, err := o.OpenFile("added.txt")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if h.Size() != 6 {
		t.Errorf("handle size %d, want 6", h.Size())
	}
	buf := make([]byte, 3)
	if n, err := h.ReadAt(buf, 3); err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt: %v", err)
	} else if string(buf[:n]) != "!!!" {
		t.Errorf("ReadAt(3) = %q, want %q", buf[:n], "!!!")
	}
	h.Close()
	if _, err := o.OpenFile("newdir"); !errors.Is(err, ErrIsDirectory) {
		t.Errorf("opening a directory gave %v, want ErrIsDirectory", err)
	}
	// The base here hands out no handles, and saying so beats a nil panic.
	if _, err := o.OpenFile("base.txt"); err == nil {
		t.Error("the base offers no handles, and OpenFile did not say so")
	}

	// Rename is a read and two writes, which the spool makes cheap.
	if err := o.Rename("added.txt", "moved.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got, err := o.ReadFile("moved.txt"); err != nil || string(got) != "six!!!" {
		t.Errorf("after Rename, moved.txt = %q, %v", got, err)
	}
	if _, err := o.ReadFile("added.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old name still answers: %v", err)
	}

	// ReadLink goes to the base, which is where links would be.
	if _, err := o.ReadLink("base.txt"); err == nil {
		t.Error("ReadLink of a plain file did not refuse")
	}
	if _, err := o.ReadLink("moved.txt"); err == nil {
		t.Error("ReadLink of a working copy did not refuse")
	}

	// ReadFile of a directory is not a file.
	if _, err := o.ReadFile("newdir"); !errors.Is(err, ErrIsDirectory) {
		t.Errorf("ReadFile of a directory gave %v, want ErrIsDirectory", err)
	}
	// And a name with nothing in it is refused rather than spooled.
	if err := o.WriteFile("", nil, 0); err == nil {
		t.Error("a file with no name was written")
	}
	if err := o.MkDir("/", 0); err == nil {
		t.Error("a directory with no name was made")
	}
}
