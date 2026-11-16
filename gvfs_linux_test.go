// Copyright (c) 2026 the go-freedesktop/gvfs authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package gvfs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// startBus runs a private dbus-daemon and returns its address. The fake gvfs
// daemon and the client under test are two connections to it. Where there is
// no dbus-daemon the test is skipped, unless GVFS_REQUIRE_BUS=1 (CI) makes
// that a failure.
func startBus(t *testing.T) string {
	t.Helper()
	exe, err := exec.LookPath("dbus-daemon")
	if err != nil {
		if os.Getenv("GVFS_REQUIRE_BUS") == "1" {
			t.Fatal("dbus-daemon not found and GVFS_REQUIRE_BUS=1")
		}
		t.Skip("no dbus-daemon")
	}
	// A bare configuration: no service directories, so nothing is ever
	// activated (a machine with gvfs installed would start the real gvfsd on
	// this private bus), and any peer may talk to any other.
	dir := t.TempDir()
	conf := filepath.Join(dir, "bus.conf")
	const busConf = `<busconfig><type>session</type><listen>unix:dir=%s</listen><auth>EXTERNAL</auth>
<policy context="default"><allow send_destination="*" eavesdrop="true"/><allow eavesdrop="true"/><allow own="*"/></policy></busconfig>`
	if err := os.WriteFile(conf, []byte(fmt.Sprintf(busConf, dir)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "--config-file="+conf, "--nofork", "--print-address=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(addr)
}

func connect(t *testing.T, addr string) *dbus.Conn {
	t.Helper()
	c, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// fakeDaemon is the part of gvfsd the client talks to: the MountTracker and,
// for unmounting, the Mount object of a mounted share.
type fakeDaemon struct {
	t    *testing.T
	conn *dbus.Conn

	mu       sync.Mutex
	mounts   []dbusMount
	specs    []dbusSpec
	sources  []dbusSource
	answers  []string // the AskPassword answers, one string per request
	asks     int      // how many times to ask for the password
	failWith *dbus.Error
	block    chan struct{}
	unmounts []string
}

func newFakeDaemon(t *testing.T, addr string) *fakeDaemon {
	t.Helper()
	f := &fakeDaemon{t: t, conn: connect(t, addr), asks: 1}
	if err := f.conn.Export(f, trackerPath, trackerIface); err != nil {
		t.Fatal(err)
	}
	if err := f.conn.Export(&fakeMount{f}, "/org/gtk/vfs/mount/1", mountIface); err != nil {
		t.Fatal(err)
	}
	rep, err := f.conn.RequestName(daemonName, dbus.NameFlagDoNotQueue)
	if err != nil || rep != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("RequestName: %v %v", rep, err)
	}
	return f
}

func (f *fakeDaemon) ListMounts2(userVisibleOnly bool) ([]dbusMount, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A copy: godbus encodes the reply after this returns, outside the lock.
	return append([]dbusMount{}, f.mounts...), nil
}

// MountLocation behaves like the daemon: it asks the source for a password,
// then registers the mount, as gvfsd-smb would after authenticating.
func (f *fakeDaemon) MountLocation(spec dbusSpec, src dbusSource) *dbus.Error {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.sources = append(f.sources, src)
	asks, fail, block := f.asks, f.failWith, f.block
	f.mu.Unlock()

	if block != nil {
		<-block
	}
	for i := 0; i < asks; i++ {
		var handled, aborted, anon bool
		var pw, user, domain string
		var save uint32
		err := f.conn.Object(src.BusName, src.Path).Call(opIface+".AskPassword", 0,
			"Enter password", "", "", uint32(0)).Store(&handled, &aborted, &pw, &user, &domain, &anon, &save)
		if err != nil {
			return dbus.MakeFailedError(err)
		}
		f.mu.Lock()
		f.answers = append(f.answers, strings.Join([]string{pw, user, domain}, "|"))
		f.mu.Unlock()
		if aborted {
			return dbus.MakeFailedError(errors.New("Operation was cancelled"))
		}
	}
	if fail != nil {
		return fail
	}
	items := map[string]dbus.Variant{}
	for k, v := range spec.Items {
		items[k] = v
	}
	items["user"] = dbus.MakeVariant(cstr("alice")) // added once authenticated
	f.mu.Lock()
	f.mounts = append(f.mounts, dbusMount{
		BusName: f.conn.Names()[0], Path: "/org/gtk/vfs/mount/1",
		DisplayName: "share", FusePath: cstr("/run/user/1000/gvfs/fake"),
		Spec: dbusSpec{Prefix: spec.Prefix, Items: items}, DefaultLocation: cstr("smb://x/y"),
	})
	f.mu.Unlock()
	return nil
}

type fakeMount struct{ f *fakeDaemon }

func (m *fakeMount) Unmount(busName string, path dbus.ObjectPath, flags uint32) *dbus.Error {
	m.f.mu.Lock()
	defer m.f.mu.Unlock()
	m.f.unmounts = append(m.f.unmounts, busName+" "+string(path))
	if m.f.failWith != nil {
		return m.f.failWith
	}
	m.f.mounts = nil
	return nil
}

// set changes the fake's behaviour under its lock. The race detector cannot
// see the D-Bus socket order a test's write before a call and the handler's
// read after it, so the lock has to say it.
func (f *fakeDaemon) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// seen returns copies of what the fake recorded.
func (f *fakeDaemon) seen() (specs []dbusSpec, answers, unmounts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(specs, f.specs...), append(answers, f.answers...), append(unmounts, f.unmounts...)
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

func setup(t *testing.T) (*fakeDaemon, *Client) {
	addr := startBus(t)
	f := newFakeDaemon(t, addr)
	return f, NewClient(connect(t, addr))
}

func TestMountSMB(t *testing.T) {
	f, c := setup(t)
	path, err := c.Mount(ctx(t), "smb://127.0.0.1:4455/Share", `CORP\alice`, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/run/user/1000/gvfs/fake" {
		t.Errorf("path = %q", path)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.specs) != 1 || len(f.answers) != 1 {
		t.Fatalf("specs %d answers %d", len(f.specs), len(f.answers))
	}
	if f.answers[0] != "s3cret|alice|CORP" {
		t.Errorf("AskPassword answer = %q", f.answers[0])
	}
	items := f.specs[0].Items
	for k, want := range map[string]string{"type": "smb-share", "server": "127.0.0.1", "share": "share", "port": "4455"} {
		if b, _ := items[k].Value().([]byte); string(b) != want+"\x00" {
			t.Errorf("item %s = %q, want %q", k, b, want)
		}
	}
	if _, ok := items["user"]; ok {
		t.Error("the user was put in the mount spec; gvfs adds it itself")
	}
	if f.sources[0].BusName != c.conn.Names()[0] || !strings.HasPrefix(string(f.sources[0].Path), opPathPrefix) {
		t.Errorf("source = %+v", f.sources[0])
	}
	// The operation object is withdrawn once the mount is done.
	err = c.conn.Object(c.conn.Names()[0], f.sources[0].Path).Call(opIface+".Aborted", 0).Err
	if err == nil {
		t.Error("the MountOperation object is still exported after Mount returned")
	}
}

func TestMountAlreadyMounted(t *testing.T) {
	f, c := setup(t)
	if _, err := c.Mount(ctx(t), "smb://h/s", "u", "p"); err != nil {
		t.Fatal(err)
	}
	path, err := c.Mount(ctx(t), "smb://h/s", "u", "p")
	if err != nil || path != "/run/user/1000/gvfs/fake" {
		t.Fatalf("second mount = %q, %v", path, err)
	}
	if specs, _, _ := f.seen(); len(specs) != 1 {
		t.Errorf("MountLocation called %d times, want 1", len(specs))
	}
}

func TestMountDAV(t *testing.T) {
	f, c := setup(t)
	if _, err := c.Mount(ctx(t), "dav://h:8080/dir/", "u", "p"); err != nil {
		t.Fatal(err)
	}
	specs, _, _ := f.seen()
	if got := string(specs[0].Prefix); got != "/dir/\x00" {
		t.Errorf("prefix = %q", got)
	}
	if err := c.Unmount(ctx(t), "dav://h:8080/dir"); err != nil {
		t.Fatalf("unmount: %v", err)
	}
}

func TestMountFallbackPaths(t *testing.T) {
	f, c := setup(t)
	if _, err := c.Mount(ctx(t), "smb://h/s", "u", "p"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.mounts[0].FusePath = cstr("")
	f.mu.Unlock()
	if p, _ := c.Mount(ctx(t), "smb://h/s", "u", "p"); p != "smb://x/y" {
		t.Errorf("default location path = %q", p)
	}
}

func TestMountAuthRefused(t *testing.T) {
	f, c := setup(t)
	f.set(func() { f.asks = 2 })
	_, err := c.Mount(ctx(t), "smb://h/s", "u", "bad")
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
	if !strings.Contains(err.Error(), "Operation was cancelled") {
		t.Errorf("daemon error lost: %v", err)
	}
	if _, answers, _ := f.seen(); len(answers) != 2 || answers[1] != "||" {
		t.Errorf("AskPassword answers %q; the second must abort without the secret", answers)
	}
}

func TestMountDaemonError(t *testing.T) {
	f, c := setup(t)
	f.set(func() { f.failWith = dbus.MakeFailedError(errors.New("no route")) })
	_, err := c.Mount(ctx(t), "smb://h/s", "u", "p")
	if err == nil || errors.Is(err, ErrAuthFailed) || !strings.Contains(err.Error(), "no route") {
		t.Fatalf("err = %v", err)
	}
}

func TestMountNotListedAfterwards(t *testing.T) {
	f, c := setup(t)
	// A daemon that reports success but lists no mount.
	if err := f.conn.Export(noListing{f}, trackerPath, trackerIface); err != nil {
		t.Fatal(err)
	}
	_, err := c.Mount(ctx(t), "smb://h/s", "u", "p")
	if err == nil || !strings.Contains(err.Error(), "does not list") {
		t.Fatalf("err = %v", err)
	}
}

type noListing struct{ f *fakeDaemon }

func (noListing) ListMounts2(bool) ([]dbusMount, *dbus.Error)    { return []dbusMount{}, nil }
func (noListing) MountLocation(dbusSpec, dbusSource) *dbus.Error { return nil }

func TestMountSecondListFails(t *testing.T) {
	f, c := setup(t)
	if err := f.conn.Export(failSecondList{}, trackerPath, trackerIface); err != nil {
		t.Fatal(err)
	}
	_, err := c.Mount(ctx(t), "smb://h/s", "u", "p")
	if err == nil || !strings.Contains(err.Error(), "list mounts") {
		t.Fatalf("err = %v", err)
	}
}

// failSecondList lists fine once, then fails.
type failSecondList struct{}

var listed sync.Once

func (failSecondList) ListMounts2(bool) ([]dbusMount, *dbus.Error) {
	first := false
	listed.Do(func() { first = true })
	if first {
		return []dbusMount{}, nil
	}
	return nil, dbus.MakeFailedError(errors.New("gone"))
}
func (failSecondList) MountLocation(dbusSpec, dbusSource) *dbus.Error { return nil }

func TestMountCancelled(t *testing.T) {
	f, c := setup(t)
	block := make(chan struct{})
	f.set(func() { f.block = block })
	t.Cleanup(func() { close(block) })
	cx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := c.Mount(cx, "smb://h/s", "u", "p")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestUnmount(t *testing.T) {
	f, c := setup(t)
	if _, err := c.Mount(ctx(t), "smb://h/s", "u", "p"); err != nil {
		t.Fatal(err)
	}
	if err := c.Unmount(ctx(t), "smb://h/s"); err != nil {
		t.Fatal(err)
	}
	if _, _, unmounts := f.seen(); len(unmounts) != 1 || !strings.HasPrefix(unmounts[0], c.conn.Names()[0]+" "+opPathPrefix) {
		t.Errorf("unmount calls = %v", unmounts)
	}
	if err := c.Unmount(ctx(t), "smb://h/s"); !errors.Is(err, ErrNotMounted) {
		t.Errorf("second unmount = %v, want ErrNotMounted", err)
	}
}

func TestUnmountErrors(t *testing.T) {
	f, c := setup(t)
	if err := c.Unmount(ctx(t), "smb:///"); !errors.Is(err, ErrInvalidURI) {
		t.Errorf("bad URI = %v", err)
	}
	if _, err := c.Mount(ctx(t), "smb:///", "u", "p"); !errors.Is(err, ErrInvalidURI) {
		t.Errorf("bad URI = %v", err)
	}
	if _, err := c.Mount(ctx(t), "smb://h/s", "u", "p"); err != nil {
		t.Fatal(err)
	}
	f.set(func() { f.failWith = dbus.MakeFailedError(errors.New("busy")) })
	if err := c.Unmount(ctx(t), "smb://h/s"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("unmount refused = %v", err)
	}
}

func TestNoDaemon(t *testing.T) {
	c := NewClient(connect(t, startBus(t)))
	if _, err := c.Mount(ctx(t), "smb://h/s", "u", "p"); err == nil || !strings.Contains(err.Error(), "list mounts") {
		t.Errorf("Mount = %v", err)
	}
	if err := c.Unmount(ctx(t), "smb://h/s"); err == nil || !strings.Contains(err.Error(), "list mounts") {
		t.Errorf("Unmount = %v", err)
	}
}

type wrongType struct{}

func (wrongType) ListMounts2(bool) (string, *dbus.Error) { return "not an array", nil }

func TestListStoreFails(t *testing.T) {
	addr := startBus(t)
	d := connect(t, addr)
	if err := d.Export(wrongType{}, trackerPath, trackerIface); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RequestName(daemonName, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	c := NewClient(connect(t, addr))
	if _, err := c.Mount(ctx(t), "smb://h/s", "u", "p"); err == nil || !strings.Contains(err.Error(), "list mounts") {
		t.Errorf("err = %v", err)
	}
}

func TestExportErrors(t *testing.T) {
	addr := startBus(t)
	newFakeDaemon(t, addr)
	c := NewClient(connect(t, addr))
	c.opPrefix = "not a path"
	if _, err := c.Mount(ctx(t), "smb://h/s", "u", "p"); err == nil || !strings.Contains(err.Error(), "export") {
		t.Errorf("Mount = %v", err)
	}
	if err := c.Unmount(ctx(t), "smb://h/s"); !errors.Is(err, ErrNotMounted) {
		t.Errorf("Unmount = %v", err)
	}

	// A connection that never said Hello has no unique name to give the daemon.
	raw, err := dbus.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if err := raw.Auth(nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewClient(raw).exportOp(&opHandler{}); err == nil || !strings.Contains(err.Error(), "unique name") {
		t.Errorf("exportOp without Hello = %v", err)
	}
}

func TestUnmountExportFails(t *testing.T) {
	f, c := setup(t)
	if _, err := c.Mount(ctx(t), "smb://h/s", "u", "p"); err != nil {
		t.Fatal(err)
	}
	_ = f
	c.opPrefix = "bad"
	if err := c.Unmount(ctx(t), "smb://h/s"); err == nil || !strings.Contains(err.Error(), "export") {
		t.Errorf("Unmount = %v", err)
	}
}

func TestPackageFuncs(t *testing.T) {
	addr := startBus(t)
	f := newFakeDaemon(t, addr)
	old := connectSession
	t.Cleanup(func() { connectSession = old })
	connectSession = func(...dbus.ConnOption) (*dbus.Conn, error) { return dbus.Connect(addr) }

	path, err := Mount(ctx(t), "smb://h/s", "u", "p")
	if err != nil || path != "/run/user/1000/gvfs/fake" {
		t.Fatalf("Mount = %q, %v", path, err)
	}
	if err := Unmount(ctx(t), "smb://h/s"); err != nil {
		t.Fatal(err)
	}
	if _, _, unmounts := f.seen(); len(unmounts) != 1 {
		t.Errorf("unmounts = %v", unmounts)
	}

	boom := errors.New("no session bus")
	connectSession = func(...dbus.ConnOption) (*dbus.Conn, error) { return nil, boom }
	if _, err := Mount(ctx(t), "smb://h/s", "u", "p"); !errors.Is(err, boom) {
		t.Errorf("Mount without a bus = %v", err)
	}
	if err := Unmount(ctx(t), "smb://h/s"); !errors.Is(err, boom) {
		t.Errorf("Unmount without a bus = %v", err)
	}
}

func TestOpHandler(t *testing.T) {
	h := &opHandler{user: "u", domain: "d", password: "p"}
	handled, aborted, pw, user, domain, anon, save, derr := h.AskPassword("m", "du", "dd", 0)
	if !handled || aborted || pw != "p" || user != "u" || domain != "d" || anon || save != 0 || derr != nil {
		t.Errorf("first ask: %v %v %q %q %q %v %v %v", handled, aborted, pw, user, domain, anon, save, derr)
	}
	if h.refused() {
		t.Error("refused after one ask")
	}
	handled, aborted, pw, _, _, _, _, _ = h.AskPassword("m", "", "", 0)
	if !handled || !aborted || pw != "" || !h.refused() {
		t.Errorf("second ask: handled %v aborted %v pw %q refused %v", handled, aborted, pw, h.refused())
	}

	// Defaults from the daemon fill in what the caller did not give.
	h = &opHandler{password: "p"}
	_, _, _, user, domain, _, _, _ = h.AskPassword("m", "du", "dd", 0)
	if user != "du" || domain != "dd" {
		t.Errorf("defaults: %q %q", user, domain)
	}

	if handled, aborted, _, derr := h.AskQuestion("m", []string{"a"}); !handled || !aborted || derr != nil {
		t.Error("AskQuestion must abort")
	}
	if handled, aborted, _, derr := h.ShowProcesses("m", nil, nil); !handled || !aborted || derr != nil {
		t.Error("ShowProcesses must abort")
	}
	if h.ShowUnmountProgress("m", 1, 2) != nil || h.Aborted() != nil {
		t.Error("progress and abort notifications return no error")
	}
}
