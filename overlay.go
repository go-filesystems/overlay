// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

// Package overlay makes a read-only filesystem writable without rewriting it
// on every change.
//
// # The problem it solves
//
// An archive is a read-only filesystem: iso9660, squashfs, zip and tar all
// answer reads and refuse writes, because there is nothing to mutate in place.
// Offering a writable Filesystem over one anyway means every WriteFile rewrites
// the whole container -- a DeleteFile on three gigabytes moving three gigabytes
// -- and the interface then promises something cheap that is not. The caller
// finds out by watching a disk.
//
// So changes are accumulated instead. A write lands in a spool directory and
// costs what that write costs; the container is rewritten ONCE, by [Overlay.Seal],
// at a moment the caller chose and named. The interface stops lying: mutation is
// cheap, and the expensive thing has its own name.
//
// # What is copied
//
// Only what changed. An entry nobody touched is read from the base and, at seal
// time, handed to the builder straight from the base -- so an archive of three
// gigabytes with one small file replaced spools that one small file.
//
// # Losing changes
//
// Unsealed changes live in the spool and nowhere else. [Overlay.Close] discards
// them, on purpose and loudly: a Close that sealed in passing would commit a
// rewrite nobody asked for, and a Close that silently kept a spool would leave
// gigabytes in a temporary directory for the next reboot to collect. Seal is the
// commit.
package overlay

import (
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"

	filesystem "github.com/go-filesystems/interface"
)

// Errors this package returns.
var (
	// ErrNotFound is returned for a path that is in neither layer, or that a
	// deletion has shadowed.
	ErrNotFound = errors.New("overlay: path not found")
	// ErrNotDirectory is returned when a directory operation names a file.
	ErrNotDirectory = errors.New("overlay: not a directory")
	// ErrIsDirectory is returned when a file operation names a directory.
	ErrIsDirectory = errors.New("overlay: is a directory")
	// ErrClosed is returned once the overlay has been closed.
	ErrClosed = errors.New("overlay: closed")
	// ErrExists is returned by MkDir for a path already taken.
	ErrExists = errors.New("overlay: already exists")
)

// kind is what the upper layer remembers about a path.
type kind int

const (
	// kindFile: the body is in the spool.
	kindFile kind = iota
	// kindDir: a directory the caller made.
	kindDir
	// kindGone: a tombstone. It has to be recorded rather than simply removed,
	// because the base still has the entry and would go on answering for it --
	// a deletion that forgets to shadow is a deletion that does nothing.
	kindGone
)

type change struct {
	kind  kind
	spool string // file in the spool holding the body, for kindFile
	mode  os.FileMode
	size  int64
}

// Overlay is a read-only filesystem plus the changes made to it.
type Overlay struct {
	mu       sync.RWMutex
	base     filesystem.Filesystem
	spool    string
	changes  map[string]*change
	seq      int
	closed   bool
	ownSpool bool
}

// Option configures an Overlay.
type Option func(*Overlay)

// WithSpool puts the working copies in dir instead of a temporary directory.
// The caller then owns it: Close leaves it alone.
func WithSpool(dir string) Option {
	return func(o *Overlay) { o.spool, o.ownSpool = dir, false }
}

// New wraps base. Nothing is read from it until something asks.
func New(base filesystem.Filesystem, opts ...Option) (*Overlay, error) {
	if base == nil {
		return nil, errors.New("overlay: no base filesystem")
	}
	o := &Overlay{base: base, changes: map[string]*change{}}
	for _, opt := range opts {
		opt(o)
	}
	if o.spool == "" {
		dir, err := os.MkdirTemp("", "overlay-spool-")
		if err != nil {
			return nil, err
		}
		o.spool, o.ownSpool = dir, true
	}
	return o, nil
}

// Spool is where working copies are kept, so a caller can say how much room the
// changes are taking.
func (o *Overlay) Spool() string { return o.spool }

// clean is the one spelling of a path this package uses.
func clean(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	p = path.Clean("/" + p)[1:]
	return p
}

// lookup answers what the overlay knows about p: the change if there is one,
// and whether a deletion shadows it.
func (o *Overlay) lookup(p string) (*change, bool) {
	if c, ok := o.changes[p]; ok {
		return c, c.kind == kindGone
	}
	// A deleted directory shadows everything beneath it, or a delete would have
	// to walk the base to find what it hides -- and the base may be large.
	for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
		if c, ok := o.changes[d]; ok && c.kind == kindGone {
			return nil, true
		}
	}
	return nil, false
}

func (o *Overlay) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return ErrClosed
	}
	o.closed = true
	var err error
	if o.ownSpool {
		err = os.RemoveAll(o.spool)
	}
	if cerr := o.base.Close(); err == nil {
		err = cerr
	}
	return err
}

// Stat answers from the upper layer first.
func (o *Overlay) Stat(p string) (filesystem.Stat, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	cp := clean(p)
	c, gone := o.lookup(cp)
	if gone {
		return nil, ErrNotFound
	}
	if c != nil {
		return filesystem.NewStat(posixMode(c.mode, c.kind == kindDir), uint64(c.size), 0), nil
	}
	return o.base.Stat(cp)
}

// posixMode is the st_mode a Stat carries: the type in the top bits, which is
// where a uint16 keeps it and os.FileMode does not.
func posixMode(m os.FileMode, dir bool) uint16 {
	perm := uint16(m.Perm())
	if dir {
		if perm == 0 {
			perm = 0o755
		}
		return 0o040000 | perm
	}
	if perm == 0 {
		perm = 0o644
	}
	return 0o100000 | perm
}

// ReadFile reads the working copy when there is one, and the base otherwise.
func (o *Overlay) ReadFile(p string) ([]byte, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	cp := clean(p)
	c, gone := o.lookup(cp)
	if gone {
		return nil, ErrNotFound
	}
	if c != nil {
		if c.kind == kindDir {
			return nil, ErrIsDirectory
		}
		return os.ReadFile(c.spool)
	}
	return o.base.ReadFile(cp)
}

// WriteFile is the cheap operation this package exists for: it writes to the
// spool, and the container is untouched until Seal.
func (o *Overlay) WriteFile(p string, data []byte, perm os.FileMode) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return ErrClosed
	}
	cp := clean(p)
	if cp == "" || cp == "." {
		return errors.New("overlay: a file with no name")
	}
	o.seq++
	name := path.Join(o.spool, "e"+itoa(o.seq))
	if err := os.WriteFile(name, data, 0o600); err != nil {
		return err
	}
	if perm == 0 {
		perm = 0o644
	}
	o.changes[cp] = &change{kind: kindFile, spool: name, mode: perm, size: int64(len(data))}
	return nil
}

// MkDir records a directory. Nothing is created in the base, which has no idea
// this happened until Seal.
func (o *Overlay) MkDir(p string, perm os.FileMode) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return ErrClosed
	}
	cp := clean(p)
	if cp == "" || cp == "." {
		return errors.New("overlay: a directory with no name")
	}
	if c, gone := o.lookup(cp); !gone && c != nil {
		return ErrExists
	} else if !gone && c == nil {
		if _, err := o.base.Stat(cp); err == nil {
			return ErrExists
		}
	}
	if perm == 0 {
		perm = 0o755
	}
	o.changes[cp] = &change{kind: kindDir, mode: perm}
	return nil
}

// DeleteFile shadows a path. The base keeps it; the overlay stops answering for
// it, and Seal leaves it out.
func (o *Overlay) DeleteFile(p string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return ErrClosed
	}
	cp := clean(p)
	if _, gone := o.lookup(cp); gone {
		return ErrNotFound
	}
	if c, ok := o.changes[cp]; ok && c.kind == kindDir {
		return ErrIsDirectory
	}
	if _, ok := o.changes[cp]; !ok {
		if _, err := o.base.Stat(cp); err != nil {
			return ErrNotFound
		}
	}
	o.changes[cp] = &change{kind: kindGone}
	return nil
}

// DeleteDir shadows a directory and everything under it.
func (o *Overlay) DeleteDir(p string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return ErrClosed
	}
	cp := clean(p)
	if _, gone := o.lookup(cp); gone {
		return ErrNotFound
	}
	o.changes[cp] = &change{kind: kindGone}
	return nil
}

// Rename is a read and two writes, which is what it costs here: the point of
// the spool is that those are cheap.
func (o *Overlay) Rename(from, to string) error {
	data, err := o.ReadFile(from)
	if err != nil {
		return err
	}
	st, err := o.Stat(from)
	if err != nil {
		return err
	}
	if err := o.WriteFile(to, data, os.FileMode(st.Mode()&0o777)); err != nil {
		return err
	}
	return o.DeleteFile(from)
}

// ReadLink asks the base: nothing in the upper layer makes links.
func (o *Overlay) ReadLink(p string) (string, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	cp := clean(p)
	if _, gone := o.lookup(cp); gone {
		return "", ErrNotFound
	}
	if _, ok := o.changes[cp]; ok {
		return "", errors.New("overlay: not a symbolic link")
	}
	return o.base.ReadLink(cp)
}

// ListDir merges the two layers: what the base holds, minus what is shadowed,
// plus what was added.
func (o *Overlay) ListDir(dir string) ([]filesystem.DirEntry, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	cd := clean(dir)
	if _, gone := o.lookup(cd); gone {
		return nil, ErrNotFound
	}

	kinds := map[string]bool{} // name -> is a directory
	if entries, err := o.base.ListDir(cd); err == nil {
		for _, e := range entries {
			kinds[e.Name()] = e.FileType() == fileTypeDir
		}
	} else if len(o.changes) == 0 {
		// Nothing added here and the base says no: its answer is the answer.
		return nil, err
	}
	// What the upper layer adds or takes away in this directory.
	for p, c := range o.changes {
		if path.Dir("/"+p) != "/"+cd && !(cd == "" && !strings.Contains(p, "/")) {
			continue
		}
		name := path.Base(p)
		switch c.kind {
		case kindGone:
			delete(kinds, name)
		case kindDir:
			kinds[name] = true
		default:
			kinds[name] = false
		}
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

// fileTypeDir is the directory marker this org's DirEntry carries.
const fileTypeDir = 2

// OpenFile hands out a handle. A working copy is opened from the spool; an
// untouched entry is asked of the base, if the base offers handles at all.
func (o *Overlay) OpenFile(p string) (filesystem.File, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	cp := clean(p)
	c, gone := o.lookup(cp)
	if gone {
		return nil, ErrNotFound
	}
	if c != nil {
		if c.kind == kindDir {
			return nil, ErrIsDirectory
		}
		f, err := os.Open(c.spool)
		if err != nil {
			return nil, err
		}
		return &spoolFile{f: f, size: c.size}, nil
	}
	op, ok := o.base.(filesystem.Opener)
	if !ok {
		return nil, errors.New("overlay: the base hands out no file handles")
	}
	return op.OpenFile(cp)
}

// spoolFile is a working copy, which is an ordinary file and so answers ReadAt
// without any of the cost an archive entry carries.
type spoolFile struct {
	f    *os.File
	size int64
}

func (s *spoolFile) ReadAt(p []byte, off int64) (int, error) { return s.f.ReadAt(p, off) }
func (s *spoolFile) Size() int64                             { return s.size }
func (s *spoolFile) Close() error                            { return s.f.Close() }

// itoa keeps the spool names short without pulling in strconv for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

var _ filesystem.Filesystem = (*Overlay)(nil)
var _ filesystem.Opener = (*Overlay)(nil)
var _ io.Closer = (*Overlay)(nil)
