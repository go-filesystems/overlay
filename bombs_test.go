// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package overlay

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// Regression tests for the memory-bomb audit (2026-11-01).

func totalAlloc() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

// listerBase answers ListDir from a function, so a test can make a base that
// lists a directory inside itself.
type listerBase struct {
	*fakeBase
	list func(dir string) []filesystem.DirEntry
}

func (b listerBase) ListDir(dir string) ([]filesystem.DirEntry, error) {
	return b.list(dir), nil
}

func dirEntry(name string) filesystem.DirEntry { return filesystem.NewDirEntry(1, name, fileTypeDir) }
func fileEntry(name string) filesystem.DirEntry {
	return filesystem.NewDirEntry(2, name, 0)
}

func sealOver(t *testing.T, b filesystem.Filesystem, lim Limits) (*recorder, error) {
	t.Helper()
	o, err := New(b, WithSpool(t.TempDir()), WithLimits(lim))
	if err != nil {
		t.Fatal(err)
	}
	r := &recorder{}
	return r, o.Seal(r)
}

// Before: a base listing a directory "d" inside every directory made Seal recurse
// and grow `seen` without end (it had not returned after 200 s).
func TestSealRefusesABaseThatListsItself(t *testing.T) {
	loop := listerBase{&fakeBase{files: map[string]string{}}, func(string) []filesystem.DirEntry {
		return []filesystem.DirEntry{dirEntry("d")}
	}}
	before := totalAlloc()
	_, err := sealOver(t, loop, Limits{})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("looping base: want ErrTooLarge, got %v", err)
	}
	if alloc := totalAlloc() - before; alloc > 32<<20 {
		t.Fatalf("looping base allocated %d bytes before being refused", alloc)
	}
}

// A cycle that branches is exponential in depth: only the entry count stops it.
func TestSealRefusesABranchingCycleByEntryCount(t *testing.T) {
	loop := listerBase{&fakeBase{files: map[string]string{}}, func(string) []filesystem.DirEntry {
		return []filesystem.DirEntry{dirEntry("x"), dirEntry("y")}
	}}
	_, err := sealOver(t, loop, Limits{MaxDepth: 1 << 20, MaxEntries: 1000})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("branching cycle: want ErrTooLarge, got %v", err)
	}
}

func TestSealDepthExactBound(t *testing.T) {
	// A file under "a/a" has two directories above it: descend reaches depth 2.
	base := &fakeBase{files: map[string]string{"a/a/f": "x"}}
	if _, err := sealOver(t, base, Limits{MaxDepth: 2}); err != nil {
		t.Fatalf("depth 2 under MaxDepth=2: %v", err)
	}
	if _, err := sealOver(t, base, Limits{MaxDepth: 1}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("depth 2 over MaxDepth=1: %v", err)
	}
}

func TestSealEntryExactBound(t *testing.T) {
	base := &fakeBase{files: map[string]string{"a": "1", "b": "2", "c/d": "3"}} // a b c c/d = 4 entries
	r, err := sealOver(t, base, Limits{MaxEntries: 4})
	if err != nil {
		t.Fatalf("4 entries under MaxEntries=4: %v", err)
	}
	if len(r.files) != 3 {
		t.Fatalf("sealed %d files, want 3", len(r.files))
	}
	if _, err := sealOver(t, base, Limits{MaxEntries: 3}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("4 entries over MaxEntries=3: %v", err)
	}
}

// "." and ".." are references, not entries: a raw directory listing carries them.
func TestSealSkipsDotAndDotDot(t *testing.T) {
	base := listerBase{&fakeBase{files: map[string]string{"f": "x"}}, func(dir string) []filesystem.DirEntry {
		return []filesystem.DirEntry{dirEntry("."), dirEntry(".."), fileEntry("f")}
	}}
	r, err := sealOver(t, base, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.files) != 1 || r.files["f"] != "x" || len(r.dirs) != 0 {
		t.Fatalf("sealed files %v dirs %v, want only f", r.files, r.dirs)
	}
}

func TestSealRefusesAnEntryNameThatIsNotOneComponent(t *testing.T) {
	for _, name := range []string{"", "a/b"} {
		base := listerBase{&fakeBase{files: map[string]string{}}, func(string) []filesystem.DirEntry {
			return []filesystem.DirEntry{fileEntry(name)}
		}}
		if _, err := sealOver(t, base, Limits{}); err == nil || !strings.Contains(err.Error(), "invalid name") {
			t.Fatalf("entry %q: want an invalid-name error, got %v", name, err)
		}
	}
}

func TestLimitsDefaults(t *testing.T) {
	o, err := New(&fakeBase{files: map[string]string{}}, WithSpool(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if o.lim.MaxEntries != DefaultMaxEntries || o.lim.MaxDepth != DefaultMaxDepth || o.lim.MaxSpool != 0 {
		t.Fatalf("defaults: %+v", o.lim)
	}
}

func spoolFiles(t *testing.T, o *Overlay) int {
	t.Helper()
	entries, err := os.ReadDir(o.Spool())
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// Before: every WriteFile left the copy it replaced, and a delete left the copy
// of the file it deleted: 100 rewrites of one 1 MiB file held 100 MiB.
func TestRewritingAFileDoesNotLeakTheOldCopy(t *testing.T) {
	o, _ := newOverlay(t, map[string]string{})
	buf := make([]byte, 1<<20)
	for i := 0; i < 20; i++ {
		if err := o.WriteFile("same", buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if n := spoolFiles(t, o); n != 1 {
		t.Fatalf("spool holds %d files after 20 rewrites of one path, want 1", n)
	}
	if o.spoolBytes != 1<<20 {
		t.Fatalf("spoolBytes = %d, want %d", o.spoolBytes, 1<<20)
	}
	if err := o.DeleteFile("same"); err != nil {
		t.Fatal(err)
	}
	if n := spoolFiles(t, o); n != 0 || o.spoolBytes != 0 {
		t.Fatalf("after DeleteFile: %d spool files, %d bytes", n, o.spoolBytes)
	}
}

func TestDeleteDirReleasesTheDirectoryEntryOnly(t *testing.T) {
	// A tombstone replacing a working copy frees it (DeleteDir on a file path).
	o, _ := newOverlay(t, map[string]string{})
	if err := o.WriteFile("f", []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := o.DeleteDir("f"); err != nil {
		t.Fatal(err)
	}
	if n := spoolFiles(t, o); n != 0 || o.spoolBytes != 0 {
		t.Fatalf("after DeleteDir over a file: %d spool files, %d bytes", n, o.spoolBytes)
	}
}

func TestMaxSpoolExactBound(t *testing.T) {
	o, err := New(&fakeBase{files: map[string]string{}}, WithSpool(t.TempDir()), WithLimits(Limits{MaxSpool: 10}))
	if err != nil {
		t.Fatal(err)
	}
	ten := []byte("0123456789")
	if err := o.WriteFile("a", ten, 0o644); err != nil {
		t.Fatalf("10 bytes under MaxSpool=10: %v", err)
	}
	// Replacing the same path frees the old copy: still 10 live bytes.
	if err := o.WriteFile("a", ten, 0o644); err != nil {
		t.Fatalf("rewrite of the same path: %v", err)
	}
	before := spoolFiles(t, o)
	if err := o.WriteFile("b", []byte("x"), 0o644); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("11th live byte: want ErrTooLarge, got %v", err)
	}
	if spoolFiles(t, o) != before {
		t.Fatal("a refused write touched the spool")
	}
	// Deleting makes room again.
	if err := o.DeleteFile("a"); err != nil {
		t.Fatal(err)
	}
	if err := o.WriteFile("b", []byte("x"), 0o644); err != nil {
		t.Fatalf("write after room was freed: %v", err)
	}
}

// FuzzSealBase drives Seal over a base whose listing is derived from the input:
// it may list ".", "..", an empty name, a name with a slash, or a directory
// inside itself. Seal must always return (an error or a result), never loop,
// recurse without bound or panic.
func FuzzSealBase(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4})
	f.Add([]byte{0x81, 0x81, 0x81, 0x81, 0x81})
	f.Add([]byte{3, 0x82, 0x83, 0x84})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			return
		}
		names := []string{"a", "b", "..", ".", "x", "d", "", "q/r"}
		list := func(dir string) []filesystem.DirEntry {
			var h uint
			for i := 0; i < len(dir); i++ {
				h = h*31 + uint(dir[i])
			}
			var out []filesystem.DirEntry
			n := int(data[h%uint(len(data))]) % 4
			for i := 0; i < n; i++ {
				b := data[(h+uint(i)+1)%uint(len(data))]
				name := names[int(b)%len(names)]
				if b&0x80 != 0 {
					out = append(out, dirEntry(name))
				} else {
					out = append(out, fileEntry(name))
				}
			}
			return out
		}
		base := listerBase{&fakeBase{files: map[string]string{}}, list}
		o, err := New(base, WithSpool(t.TempDir()), WithLimits(Limits{MaxEntries: 300, MaxDepth: 12}))
		if err != nil {
			t.Fatal(err)
		}
		_ = o.Seal(&recorder{})
	})
}

// FuzzSpoolAccounting applies a random sequence of writes, deletes, makedirs and
// renames and checks the spool's books: the bytes counted equal the live working
// copies, they never exceed MaxSpool, and the spool directory holds exactly one
// file per live working copy (no leaked copy of a replaced or deleted file).
func FuzzSpoolAccounting(f *testing.F) {
	f.Add([]byte{0, 5, 0, 5, 1, 5})
	f.Add([]byte{0, 39, 0, 38, 0, 37, 4, 12})
	f.Fuzz(func(t *testing.T, ops []byte) {
		o, err := New(&fakeBase{files: map[string]string{"a": "1", "d/e": "2"}}, WithSpool(t.TempDir()), WithLimits(Limits{MaxSpool: 64}))
		if err != nil {
			t.Fatal(err)
		}
		paths := []string{"a", "b", "d", "d/e", "d/f", "../x", "/a"}
		for i := 0; i+1 < len(ops); i += 2 {
			p := paths[int(ops[i+1])%len(paths)]
			switch ops[i] % 5 {
			case 0:
				_ = o.WriteFile(p, make([]byte, int(ops[i+1])%40), 0)
			case 1:
				_ = o.DeleteFile(p)
			case 2:
				_ = o.DeleteDir(p)
			case 3:
				_ = o.MkDir(p, 0)
			case 4:
				_ = o.Rename(p, paths[(int(ops[i+1])/7)%len(paths)])
			}
		}
		var live int64
		files := 0
		for _, c := range o.changes {
			if c.kind == kindFile {
				live += c.size
				files++
			}
		}
		if live != o.spoolBytes || live > 64 {
			t.Fatalf("books: live %d, counted %d, limit 64", live, o.spoolBytes)
		}
		if n := spoolFiles(t, o); n != files {
			t.Fatalf("spool holds %d files for %d working copies", n, files)
		}
	})
}
