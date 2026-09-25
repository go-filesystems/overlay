// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package overlay

import (
	"bytes"
	"errors"
	filesystem "github.com/go-filesystems/interface"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
)

// Builder is what a format offers for authoring one: iso9660's Builder, a zip
// writer, this org's sevenzip writer. Seal drives it and knows nothing else
// about the format.
type Builder interface {
	// AddDir records a directory. A format with no directory entries may
	// ignore it.
	AddDir(name string, perm os.FileMode) error
	// AddFile records a file of exactly size bytes and reads them from r.
	//
	// The size is given rather than discovered because tar cannot be written
	// without it: an entry's length goes in its header, BEFORE its bytes. A
	// builder handed only a reader would have to spool the whole entry to learn
	// how long it is -- copying a 1.2 GiB video to a temporary file to count it,
	// when the caller knew the answer -- which is the cost this whole layer
	// exists to avoid. A format that does not need the size ignores it.
	//
	// r delivers exactly size bytes. A builder may rely on that; a caller that
	// breaks it produces an archive whose header disagrees with its data, which
	// is the one failure every reader reports as corruption.
	AddFile(name string, perm os.FileMode, size int64, r io.Reader) error
}

// Seal hands the merged view to a builder, once.
//
// Entries are offered in path order, directories before what is inside them,
// because a builder that has to create a parent before its children can then
// rely on that rather than sorting for itself.
//
// An entry nobody touched is streamed from the BASE. Only what changed was ever
// copied, which is what keeps a small edit to a large archive small: the spool
// holds the edit, and the rest goes straight through.
func (o *Overlay) Seal(b Builder) error {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.closed {
		return ErrClosed
	}
	entries, err := o.walk()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.dir {
			if err := b.AddDir(e.path, e.mode); err != nil {
				return err
			}
			continue
		}
		if err := o.addFile(b, e); err != nil {
			return err
		}
	}
	return nil
}

// sealEntry is one thing the merged view holds.
type sealEntry struct {
	path  string
	dir   bool
	mode  os.FileMode
	spool string // non-empty when the body is a working copy
}

// addFile opens the one entry and hands it over, from the spool when it was
// changed and from the base when it was not.
func (o *Overlay) addFile(b Builder, e sealEntry) error {
	if e.spool != "" {
		f, err := os.Open(e.spool)
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		return b.AddFile(e.path, e.mode, st.Size(), f)
	}
	// From the base. A handle is asked for first so a large entry is streamed
	// rather than held: ReadFile would put the whole thing in memory, which for
	// an archive of videos is the thing to avoid.
	//
	// ⛔ This asks for filesystem.Opener BY NAME, and the name is the whole
	// point. It used to assert an anonymous interface of the same SHAPE --
	// OpenFile(string) (interface{io.ReaderAt; io.Closer; Size() int64}, error)
	// -- which no driver in this ecosystem can satisfy: Go requires a method's
	// signature to match exactly, and filesystem.File is a different TYPE from
	// an anonymous interface with filesystem.File's methods. Every driver
	// returns filesystem.File, so the assertion always failed and this branch
	// was unreachable. Nothing broke; every seal simply read whole files into
	// memory through the fallback below, which is the one thing the comment
	// above says not to do.
	if op, ok := o.base.(filesystem.Opener); ok {
		h, err := op.OpenFile(e.path)
		if err == nil {
			defer h.Close()
			return b.AddFile(e.path, e.mode, h.Size(), io.NewSectionReader(h, 0, h.Size()))
		}
	}
	data, err := o.base.ReadFile(e.path)
	if err != nil {
		return err
	}
	return b.AddFile(e.path, e.mode, int64(len(data)), bytes.NewReader(data))
}

// walk produces the merged view, sorted so a parent precedes its children.
func (o *Overlay) walk() ([]sealEntry, error) {
	seen := map[string]sealEntry{}

	// The base, minus whatever is shadowed.
	var descend func(dir string) error
	descend = func(dir string) error {
		entries, err := o.base.ListDir(dir)
		if err != nil {
			// A base that cannot list a directory it named is a base problem,
			// and hiding it here would seal a shorter archive than the caller
			// has.
			return err
		}
		for _, e := range entries {
			p := path.Join(dir, e.Name())
			if _, gone := o.lookup(p); gone {
				continue
			}
			isDir := e.FileType() == fileTypeDir
			mode := os.FileMode(0o644)
			if isDir {
				mode = 0o755
			}
			if st, err := o.base.Stat(p); err == nil {
				if m := os.FileMode(st.Mode() & 0o777); m != 0 {
					mode = m
				}
			}
			seen[p] = sealEntry{path: p, dir: isDir, mode: mode}
			if isDir {
				if err := descend(p); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := descend(""); err != nil {
		return nil, err
	}

	// What the upper layer adds, which wins where both have the path.
	for p, c := range o.changes {
		switch c.kind {
		case kindGone:
			delete(seen, p)
		case kindDir:
			seen[p] = sealEntry{path: p, dir: true, mode: c.mode}
		default:
			seen[p] = sealEntry{path: p, mode: c.mode, spool: c.spool}
		}
	}

	out := make([]sealEntry, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// SealToFile seals into target, and does it by RENAME.
//
// The new archive is written beside the target and moved over it at the end, so
// an interrupted seal -- a full disk, a signal, a power cut -- leaves the
// original exactly as it was. Sealing in place would destroy the archive in
// order to write it, which turns every interruption into data loss, and the
// window is the whole rewrite.
func (o *Overlay) SealToFile(target string, mk func(io.WriteSeeker) (Builder, error)) error {
	if mk == nil {
		return errors.New("overlay: no builder")
	}
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".sealing-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Anything that goes wrong from here leaves the target untouched, and the
	// half-written file is removed rather than left to be mistaken for an
	// archive.
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	b, err := mk(tmp)
	if err != nil {
		return err
	}
	if err := o.Seal(b); err != nil {
		return err
	}
	if c, ok := b.(io.Closer); ok {
		if err := c.Close(); err != nil {
			return err
		}
	}
	// On disk before the rename: a rename is atomic, but it only promises to
	// publish what the filesystem already holds.
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return err
	}
	// The deferred Remove must not delete what was just published.
	tmpName = ""
	return nil
}
