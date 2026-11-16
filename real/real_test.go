// Copyright (c) 2026 the go-freedesktop/gvfs authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

// Package real holds the test that needs a real GNOME session: a session bus
// and gvfsd-smb. It is its own module so the SMB server it serves from does
// not enter the dependency graph of gvfs itself.
//
//	GVFS_REAL=1 go test -count=1 -v ./real/
package real

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fssmb "github.com/go-filesystems/smb"
	"github.com/go-freedesktop/gvfs"
)

// TestRealMount serves an SMB share from this process on 127.0.0.1, mounts it
// in the user's session through gvfs with a password given to the program (no
// dialog, no argv), writes and reads through the gvfs-fuse path, checks that
// the mount is listed the way GNOME Files lists it, and unmounts.
func TestRealMount(t *testing.T) {
	if os.Getenv("GVFS_REAL") != "1" {
		t.Skip("set GVFS_REAL=1 on a Linux machine with a session bus and gvfsd-smb")
	}

	mem := newMemFS(true)
	body := bytes.Repeat([]byte("go-freedesktop/gvfs "), 3000)
	if err := mem.WriteFile("/greeting.txt", body, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := fssmb.New()
	srv.AddUser("alice", "hunter2")
	if err := srv.Share("disk", mem); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	uri := fmt.Sprintf("smb://127.0.0.1:%d/disk", ln.Addr().(*net.TCPAddr).Port)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	root, err := gvfs.Mount(ctx, uri, "alice", "hunter2")
	if err != nil {
		t.Fatalf("Mount(%s): %v", uri, err)
	}
	t.Logf("mounted %s at %s", uri, root)
	mounted := true
	defer func() {
		if mounted {
			_ = gvfs.Unmount(context.Background(), uri)
		}
	}()
	if want := fmt.Sprintf("/run/user/%d/gvfs/", os.Getuid()); !strings.HasPrefix(root, want) {
		t.Fatalf("mount path %q is not under %s", root, want)
	}

	got, err := os.ReadFile(filepath.Join(root, "greeting.txt"))
	if err != nil {
		t.Fatalf("read through gvfs-fuse: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("read %d bytes, want %d", len(got), len(body))
	}

	note := []byte("written through gvfs\n")
	if err := os.WriteFile(filepath.Join(root, "note.txt"), note, 0o644); err != nil {
		t.Fatalf("write through gvfs-fuse: %v", err)
	}
	back, err := os.ReadFile(filepath.Join(root, "note.txt"))
	if err != nil || !bytes.Equal(back, note) {
		t.Errorf("read back %q, %v", back, err)
	}
	if onServer, err := mem.ReadFile("/note.txt"); err != nil || !bytes.Equal(onServer, note) {
		t.Errorf("the SMB server holds %q, %v", onServer, err)
	}

	// GNOME Files lists exactly what GIO's mount list holds.
	if out := gio(t, "mount", "-l"); !strings.Contains(out, "127.0.0.1") {
		t.Errorf("gio mount -l does not list the share:\n%s", out)
	}

	// Mounting again is not an error and gives the same place.
	if again, err := gvfs.Mount(ctx, uri, "alice", "hunter2"); err != nil || again != root {
		t.Errorf("second Mount = %q, %v", again, err)
	}

	if err := gvfs.Unmount(ctx, uri); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	mounted = false
	if out := gio(t, "mount", "-l"); strings.Contains(out, "127.0.0.1") {
		t.Errorf("the share is still listed after Unmount:\n%s", out)
	}
	if err := gvfs.Unmount(ctx, uri); err == nil {
		t.Error("second Unmount succeeded")
	}
}

func gio(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("gio", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("gio %v: %v\n%s", args, err, out)
	}
	return string(out)
}
