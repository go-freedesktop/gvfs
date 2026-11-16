# gvfs — go-freedesktop

[![ci](https://github.com/go-freedesktop/gvfs/actions/workflows/ci.yml/badge.svg)](https://github.com/go-freedesktop/gvfs/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-freedesktop/gvfs.svg)](https://pkg.go.dev/github.com/go-freedesktop/gvfs)
[![License](https://img.shields.io/badge/license-BSD--3--Clause-blue)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.27%2B-00ADD8)](https://go.dev/dl/)
[![Coverage](https://img.shields.io/badge/coverage-100%25-1a7f37)](#tests)

Mount an SMB (`smb://host[:port]/share`) or WebDAV (`dav://`, `davs://`)
location in the user's GNOME session through
[gvfs](https://wiki.gnome.org/Projects/gvfs), over D-Bus, in pure Go
(`CGO_ENABLED=0`, [`github.com/godbus/dbus/v5`](https://github.com/godbus/dbus)).
The share then shows up in GNOME Files (Nautilus) like one mounted from its
sidebar, and the program can unmount it again.

The program answers gvfs's own "ask password" request itself, with the user
and password it was given: **no dialog appears and no secret is in a command
line** (the alternative, `gio mount smb://user:pass@host/share`, puts it in
argv, and `gio mount` prompts on a terminal otherwise).

```go
path, err := gvfs.Mount(ctx, "smb://127.0.0.1:4455/share", "alice", password)
// path = /run/user/1000/gvfs/smb-share:port=4455,server=127.0.0.1,share=share
// ... use it like any directory, or let Files show it ...
err = gvfs.Unmount(ctx, "smb://127.0.0.1:4455/share")
```

## API

| | |
|---|---|
| `Mount(ctx, uri, user, password) (mountPath, error)` | mounts; an already mounted location returns its path |
| `Unmount(ctx, uri) error` | unmounts; `ErrNotMounted` when nothing is mounted there |
| `NewClient(conn)` | the same two methods on a session-bus connection you own |

`mountPath` is the gvfs-fuse path (`/run/user/<uid>/gvfs/...`), or the mount's
default location when gvfs-fuse is not running.

- `user` may be written `DOMAIN\name`. Credentials in the URI are refused
  (`ErrInvalidURI`): they are arguments, so they never sit in a string that
  gets logged.
- Anything else gvfs asks (a TLS certificate question, a busy unmount) is
  refused. A **second** password request means the first answer was wrong:
  the mount is aborted and the error wraps `ErrAuthFailed`.
- For `smb://host/share/sub/dir` the share is mounted; the sub-path is not.
- On any other GOOS every call returns `ErrUnsupported`; the package still
  cross-compiles.

## How it talks to gvfs

Read from [GNOME/gvfs](https://gitlab.gnome.org/GNOME/gvfs) at tag **1.62.0**:

- `common/gvfsdaemonprotocol.h`: the name `org.gtk.vfs.Daemon`, the path
  `/org/gtk/vfs/mounttracker`, the 30-minute mount timeout.
- `common/org.gtk.vfs.xml`: `org.gtk.vfs.MountTracker` (`MountLocation`,
  `ListMounts2`), `org.gtk.vfs.MountOperation` (`AskPassword`, `AskQuestion`,
  `ShowProcesses`, `ShowUnmountProgress`, `Aborted`), `org.gtk.vfs.Mount`
  (`Unmount`).
- `client/gdaemonfile.c` (`g_daemon_file_mount_enclosing_volume`) and
  `daemon/mount.c` (`handle_mount_location`): the call GIO makes, and how the
  daemon matches and rejects it.
- `common/gmountspec.c`, `common/gmountsource.c`, `common/gmountoperationdbus.c`,
  `common/gmounttracker.c`: the wire forms `(aya{sv})`, `(so)`,
  `(sossssssbay(aya{sv})ay)`, and the `AskPassword` reply.
- `client/smburi.c`, `daemon/gvfsbackenddav.c`: how a URI becomes a mount spec.
- `client/gdaemonmount.c`: the unmount call.

`Mount` exports an `org.gtk.vfs.MountOperation` object on its own bus
connection, calls `MountLocation(spec, (busname, path))`, and the mount daemon
calls `AskPassword` back on it. The object is withdrawn when the call returns.

## Tests

```
go test -race ./...
```

- **Unit tests, 100% of statements** on Linux, macOS and Windows, against a
  fake gvfs daemon (`MountTracker`, `Mount`) that two godbus connections reach
  through a private `dbus-daemon` with no service directories, so a machine
  with gvfs installed never activates the real one. Without `dbus-daemon` they
  skip; CI sets `GVFS_REQUIRE_BUS=1` so a runner without it fails.
- **A real mount**, `real/` (its own module): serves a share from
  [go-filesystems/smb](https://github.com/go-filesystems/smb) in-process on
  127.0.0.1, mounts it, reads and writes through the gvfs-fuse path, checks
  `gio mount -l`, unmounts.

  ```
  cd real && GVFS_REAL=1 go test -count=1 -v ./...
  ```

  It needs a GNOME session (session bus, `gvfsd`, `gvfsd-smb`, `gvfs-fuse`).
  It passed on Ubuntu 24.04 with gvfs 1.54.4; the protocol was read at 1.62.0.

## License

BSD-3-Clause.
