<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems-overlay.png" alt="go-filesystems/overlay" width="720"></p>

# overlay

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/overlay.svg)](https://pkg.go.dev/github.com/go-filesystems/overlay)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-filesystems/overlay/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/overlay/actions/workflows/ci.yml)

Makes a **read-only filesystem writable** without rewriting it on every change —
pure Go, `CGO_ENABLED=0`, builds for every target Go builds for.

```go
o, err := overlay.New(base)          // base is any filesystem.Filesystem
defer o.Close()                      // discards unsealed changes, on purpose

o.WriteFile("/notes.txt", b, 0o644)  // lands in a spool; costs what it costs
o.DeleteFile("/old.log")             // shadowed, not moved
o.Seal(builder)                      // the container is rewritten ONCE, here
```

## Why it exists

An archive **is** a read-only filesystem: iso9660, squashfs, zip and tar all
answer reads and refuse writes, because there is nothing to mutate in place.

Offering a writable `Filesystem` over one anyway means every `WriteFile` rewrites
the whole container — **a `DeleteFile` on three gigabytes moving three gigabytes** —
and the interface then promises something cheap that is not. The caller finds out
by watching a disk.

So changes accumulate instead. A write lands in a spool directory; the container is
rewritten **once**, by `Seal`, at a moment the caller chose and named. The interface
stops lying: mutation is cheap, and the expensive thing has its own name.

## Only what changed is copied

An entry nobody touched is read from the base, and at seal time handed to the
builder **straight from the base**. An archive of three gigabytes with one small
file replaced spools that one small file.

⛔ That streaming path was unreachable for a while, and nothing noticed. `Seal`
asserted an *anonymous* interface for "can this hand out a file handle" —
structurally identical to `filesystem.Opener`, and therefore satisfied by nothing,
because Go interface satisfaction is by method set against a **named** type. Every
seal read whole files into memory and the result was correct every time. What found
it was a test that asked the base **which handles it had been asked for** and got
back an empty list.

## `Seal` takes a `Builder`, and the size is part of it

```go
type Builder interface {
	AddDir(name string, perm os.FileMode) error
	AddFile(name string, perm os.FileMode, size int64, r io.Reader) error
}
```

The size is **given rather than discovered** because tar cannot be written without
it: an entry's length goes in its header, *before* its bytes. A builder handed only
a reader would have to spool the whole entry to learn how long it is — copying a
1.2 GiB video to a temporary file to count it, when the caller knew the answer —
which is the cost this whole layer exists to avoid. A format that does not need the
size ignores it.

`r` delivers exactly `size` bytes. A builder may rely on that; a caller that breaks
it produces an archive whose header disagrees with its data, which is the one
failure every reader reports as corruption.

Entries arrive in **path order, directories before what is inside them**, so a
builder that must create a parent before its children can rely on that rather than
sorting for itself.

Implementations in the family: `iso9660.Builder`, `sevenzip`'s writer, and the tar,
zip, ar, cpio and rar builders in
[`go-filesystems/unarchive`](https://github.com/go-filesystems/unarchive).

## Close discards, Seal commits

Unsealed changes live in the spool and nowhere else. `Close` throws them away, **on
purpose and loudly**:

- a `Close` that sealed in passing would commit a rewrite nobody asked for;
- a `Close` that silently kept the spool would leave gigabytes in a temporary
  directory for the next reboot to collect.

`WithSpool(dir)` puts the working copies somewhere the caller owns, and `Close`
then leaves that directory alone.

## What the upper layer does not do

| | |
|---|---|
| symbolic links | `ReadLink` asks the base; nothing in the upper layer makes links |
| in-place mutation of the base | never — the base is read, and only read |
| `Rename` | a read and two writes, which is what it costs here |

## Licence

BSD-3-Clause.
