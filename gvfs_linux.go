// Copyright (c) 2026 the go-freedesktop/gvfs authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package gvfs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/godbus/dbus/v5"
)

// connectSession is replaceable by tests.
var connectSession = dbus.ConnectSessionBus

// opSeq numbers the MountOperation objects of this process.
var opSeq atomic.Uint64

// Client talks to the gvfs daemon over one D-Bus connection.
type Client struct {
	conn     *dbus.Conn
	opPrefix string
}

// NewClient returns a Client that uses conn, which must be a connection to
// the session bus. The caller keeps ownership of conn.
func NewClient(conn *dbus.Conn) *Client {
	return &Client{conn: conn, opPrefix: opPathPrefix}
}

// Mount mounts uri in the user's session through a fresh session-bus
// connection. See Client.Mount.
func Mount(ctx context.Context, uri, user, password string) (mountPath string, err error) {
	conn, err := connectSession()
	if err != nil {
		return "", fmt.Errorf("gvfs: session bus: %w", err)
	}
	defer conn.Close()
	return NewClient(conn).Mount(ctx, uri, user, password)
}

// Unmount unmounts uri through a fresh session-bus connection. See
// Client.Unmount.
func Unmount(ctx context.Context, uri string) error {
	conn, err := connectSession()
	if err != nil {
		return fmt.Errorf("gvfs: session bus: %w", err)
	}
	defer conn.Close()
	return NewClient(conn).Unmount(ctx, uri)
}

// Mount mounts uri, an smb://host[:port]/share, dav://host[:port]/path or
// davs:// location, and returns where it is reachable: the gvfs-fuse path
// (/run/user/<uid>/gvfs/...), or the mount's default location when gvfs-fuse
// is not running. The user and password answer the daemon's AskPassword call
// directly: no dialog appears. A user written DOMAIN\name carries its
// domain. An already mounted location is not an error; its path is returned.
//
// Anything else gvfs asks (a TLS certificate question, say) is refused, and a
// second password request, which means the credentials were wrong, aborts the
// mount with ErrAuthFailed. The mount may take as long as ctx allows.
func (c *Client) Mount(ctx context.Context, uri, user, password string) (mountPath string, err error) {
	loc, err := parseURI(uri)
	if err != nil {
		return "", err
	}
	if m, err := c.find(ctx, loc); err != nil {
		return "", err
	} else if m != nil {
		return m.path(), nil
	}
	domain, name := splitUser(user)
	h := &opHandler{user: name, domain: domain, password: password}
	src, release, err := c.exportOp(h)
	if err != nil {
		return "", err
	}
	defer release()
	call := c.conn.Object(daemonName, trackerPath).CallWithContext(ctx,
		trackerIface+".MountLocation", 0, loc.spec(), src)
	if call.Err != nil {
		err = fmt.Errorf("gvfs: mount %s: %w", uri, call.Err)
		if h.refused() {
			err = errors.Join(err, ErrAuthFailed)
		}
		return "", err
	}
	m, err := c.find(ctx, loc)
	if err != nil {
		return "", err
	}
	if m == nil {
		return "", fmt.Errorf("gvfs: mounted, but the mount tracker does not list %q", uri)
	}
	return m.path(), nil
}

// Unmount unmounts uri. It returns ErrNotMounted when nothing is mounted
// there.
func (c *Client) Unmount(ctx context.Context, uri string) error {
	loc, err := parseURI(uri)
	if err != nil {
		return err
	}
	m, err := c.find(ctx, loc)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("%w: %s", ErrNotMounted, uri)
	}
	src, release, err := c.exportOp(&opHandler{})
	if err != nil {
		return err
	}
	defer release()
	// Mount.Unmount(dbus_id, obj_path, flags), called on the mount's own
	// daemon (client/gdaemonmount.c).
	call := c.conn.Object(m.BusName, m.Path).CallWithContext(ctx,
		mountIface+".Unmount", 0, src.BusName, src.Path, uint32(0))
	if call.Err != nil {
		return fmt.Errorf("gvfs: unmount %s: %w", uri, call.Err)
	}
	return nil
}

// find returns the mount of loc listed by the daemon, or nil.
func (c *Client) find(ctx context.Context, loc *location) (*dbusMount, error) {
	var mounts []dbusMount
	call := c.conn.Object(daemonName, trackerPath).CallWithContext(ctx,
		trackerIface+".ListMounts2", 0, false)
	if call.Err != nil {
		return nil, fmt.Errorf("gvfs: list mounts: %w", call.Err)
	}
	if err := call.Store(&mounts); err != nil {
		return nil, fmt.Errorf("gvfs: list mounts: %w", err)
	}
	for i := range mounts {
		if loc.matches(&mounts[i]) {
			return &mounts[i], nil
		}
	}
	return nil, nil
}

// exportOp exports h as a MountOperation object and returns the GMountSource
// that names it, and the function that withdraws it.
func (c *Client) exportOp(h *opHandler) (dbusSource, func(), error) {
	// Names()[0] is the unique name, empty until the connection said Hello.
	names := c.conn.Names()
	if names[0] == "" {
		return dbusSource{}, nil, errors.New("gvfs: the D-Bus connection has no unique name (no Hello)")
	}
	path := dbus.ObjectPath(c.opPrefix + strconv.FormatUint(opSeq.Add(1), 10))
	if err := c.conn.Export(h, path, opIface); err != nil {
		return dbusSource{}, nil, fmt.Errorf("gvfs: export %s: %w", path, err)
	}
	return dbusSource{BusName: names[0], Path: path}, func() {
		_ = c.conn.Export(nil, path, opIface)
	}, nil
}

// opHandler implements org.gtk.vfs.MountOperation for the daemon's callbacks
// (common/gmountoperationdbus.c is the client side of the same interface).
type opHandler struct {
	user, domain, password string

	mu    sync.Mutex
	asked int
}

// refused reports whether the daemon asked for the password again.
func (h *opHandler) refused() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.asked > 1
}

// AskPassword answers the first request with the credentials and aborts any
// further one, since a second request means the first answer was refused.
func (h *opHandler) AskPassword(message, defaultUser, defaultDomain string, flags uint32) (handled, aborted bool, password, username, domain string, anonymous bool, save uint32, derr *dbus.Error) {
	h.mu.Lock()
	h.asked++
	first := h.asked == 1
	h.mu.Unlock()
	if !first {
		return true, true, "", "", "", false, 0, nil
	}
	domain = h.domain
	if domain == "" {
		domain = defaultDomain
	}
	username = h.user
	if username == "" {
		username = defaultUser
	}
	return true, false, h.password, username, domain, false, 0, nil
}

// AskQuestion aborts: this package has no one to ask.
func (h *opHandler) AskQuestion(message string, choices []string) (handled, aborted bool, choice uint32, derr *dbus.Error) {
	return true, true, 0, nil
}

// ShowProcesses aborts, for the same reason.
func (h *opHandler) ShowProcesses(message string, choices []string, processes []int32) (handled, aborted bool, choice uint32, derr *dbus.Error) {
	return true, true, 0, nil
}

// ShowUnmountProgress ignores the progress report.
func (h *opHandler) ShowUnmountProgress(message string, timeLeft, bytesLeft int64) *dbus.Error {
	return nil
}

// Aborted ignores the notification.
func (h *opHandler) Aborted() *dbus.Error { return nil }
