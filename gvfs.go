// Copyright (c) 2026 the go-freedesktop/gvfs authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

// Package gvfs mounts an SMB (smb://host[:port]/share) or WebDAV
// (dav://, davs://) location in the user's GNOME session through gvfs, over
// D-Bus, in pure Go.
//
// It speaks the protocol GIO itself speaks to the gvfs daemon
// (org.gtk.vfs.MountTracker.MountLocation) and answers the daemon's
// org.gtk.vfs.MountOperation.AskPassword call itself, with the user and
// password the caller supplied, so no dialog appears and no secret travels
// in a command line. The mount then shows up in GNOME Files (Nautilus)
// like any mount made from its sidebar.
//
// The wire protocol was read from GNOME/gvfs at tag 1.62.0: see README.md
// for the files cited. The package builds on every platform; outside
// GOOS=linux every operation returns ErrUnsupported.
package gvfs

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/godbus/dbus/v5"
)

// Errors returned by this package. Test with errors.Is.
var (
	// ErrUnsupported is returned by every operation on a platform other than
	// GOOS=linux: gvfs is a Linux desktop service.
	ErrUnsupported = errors.New("gvfs: only supported on GOOS=linux")

	// ErrInvalidURI reports a location this package cannot mount: not an
	// smb://host/share, dav:// or davs:// URI, or one carrying credentials.
	ErrInvalidURI = errors.New("gvfs: invalid location")

	// ErrNotMounted is returned by Unmount for a location that is not
	// currently mounted.
	ErrNotMounted = errors.New("gvfs: location is not mounted")

	// ErrAuthFailed is joined to a Mount error when gvfs asked for the
	// password a second time, that is when the credentials were refused.
	ErrAuthFailed = errors.New("gvfs: credentials refused")
)

// Names, paths and interfaces of the gvfs D-Bus API, from
// common/gvfsdaemonprotocol.h and common/org.gtk.vfs.xml (gvfs 1.62.0).
const (
	daemonName   = "org.gtk.vfs.Daemon"
	trackerPath  = "/org/gtk/vfs/mounttracker"
	trackerIface = "org.gtk.vfs.MountTracker"
	mountIface   = "org.gtk.vfs.Mount"
	opIface      = "org.gtk.vfs.MountOperation"

	// opPathPrefix is where this package exports its MountOperation object;
	// the daemon calls it back there. gvfs itself uses /org/gtk/gvfs/mountop/N.
	opPathPrefix = "/org/go_freedesktop/gvfs/mountop/"

	defaultSMBPort  = 445
	defaultHTTPPort = 80
	defaultTLSPort  = 443
)

// dbusSpec is a GMountSpec on the wire: (aya{sv}). The prefix is a
// NUL-terminated byte string and every item value is a variant holding one
// (g_mount_spec_to_dbus_with_path in common/gmountspec.c).
type dbusSpec struct {
	Prefix []byte
	Items  map[string]dbus.Variant
}

// dbusSource is a GMountSource on the wire: (so), the bus name and object path
// of the MountOperation the daemon calls back (common/gmountsource.c).
type dbusSource struct {
	BusName string
	Path    dbus.ObjectPath
}

// dbusMount is one mount as ListMounts2 returns it: (sossssssbay(aya{sv})ay),
// decoded in common/gmounttracker.c.
type dbusMount struct {
	BusName         string
	Path            dbus.ObjectPath
	DisplayName     string
	StableName      string
	XContentTypes   string
	Icon            string
	SymbolicIcon    string
	FilenameEncoder string
	UserVisible     bool
	FusePath        []byte
	Spec            dbusSpec
	DefaultLocation []byte
}

// location is a parsed URI: the mount type, its identifying items and its
// prefix.
type location struct {
	typ    string
	items  map[string]string
	prefix string
}

// cstr is a byte string as GLib's "^ay" builds it: NUL-terminated.
func cstr(s string) []byte { return append([]byte(s), 0) }

// fromCstr strips the terminating NUL.
func fromCstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// parseURI maps a URI to the mount spec gvfs builds for it: smb_from_uri in
// client/smburi.c and g_mount_spec_from_dav_uri in daemon/gvfsbackenddav.c.
// Credentials are refused in the URI: they are separate arguments, so no
// secret rides in a string that is logged or listed.
func parseURI(raw string) (*location, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURI, err)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: credentials belong in the user and password arguments, not the URI", ErrInvalidURI)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("%w: no host in %q", ErrInvalidURI, u.Scheme+"://")
	}
	port := 0
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("%w: bad port %q", ErrInvalidURI, p)
		}
	}
	switch strings.ToLower(u.Scheme) {
	case "smb":
		return parseSMB(u, host, port)
	case "dav", "davs":
		return parseDAV(u, host, port), nil
	}
	return nil, fmt.Errorf("%w: scheme %q is neither smb, dav nor davs", ErrInvalidURI, u.Scheme)
}

func parseSMB(u *url.URL, host string, port int) (*location, error) {
	share, _, _ := strings.Cut(strings.TrimLeft(u.Path, "/"), "/")
	if share == "" {
		return nil, fmt.Errorf("%w: smb URI needs a share: smb://host/share", ErrInvalidURI)
	}
	l := &location{
		typ:    "smb-share",
		items:  map[string]string{"server": strings.ToLower(host), "share": strings.ToLower(share)},
		prefix: "/",
	}
	if port != 0 && port != defaultSMBPort {
		l.items["port"] = strconv.Itoa(port)
	}
	return l, nil
}

func parseDAV(u *url.URL, host string, port int) *location {
	ssl := strings.EqualFold(u.Scheme, "davs")
	if strings.Contains(host, ":") { // IPv6: the spec keeps the brackets
		host = "[" + host + "]"
	}
	l := &location{
		typ:    "dav",
		items:  map[string]string{"host": host, "ssl": strconv.FormatBool(ssl)},
		prefix: u.Path,
	}
	def := defaultHTTPPort
	if ssl {
		def = defaultTLSPort
	}
	if port != 0 && port != def {
		l.items["port"] = strconv.Itoa(port)
	}
	return l
}

// spec returns the wire form of l.
func (l *location) spec() dbusSpec {
	items := map[string]dbus.Variant{"type": dbus.MakeVariant(cstr(l.typ))}
	for k, v := range l.items {
		items[k] = dbus.MakeVariant(cstr(v))
	}
	return dbusSpec{Prefix: cstr(l.prefix), Items: items}
}

// matches reports whether m is the mount of l. gvfs adds a user (and
// sometimes a domain) item to a spec once it has authenticated, so those two
// keys are ignored; every other item must be equal (g_mount_spec_match).
// A DAV mount also needs the same path prefix, up to a trailing slash.
func (l *location) matches(m *dbusMount) bool {
	got := map[string]string{}
	for k, v := range m.Spec.Items {
		b, ok := v.Value().([]byte)
		if !ok {
			return false
		}
		got[k] = fromCstr(b)
	}
	if got["type"] != l.typ {
		return false
	}
	delete(got, "type")
	delete(got, "user")
	delete(got, "domain")
	if len(got) != len(l.items) {
		return false
	}
	for k, v := range l.items {
		if got[k] != v {
			return false
		}
	}
	if l.typ == "dav" {
		return strings.TrimSuffix(fromCstr(m.Spec.Prefix), "/") == strings.TrimSuffix(l.prefix, "/")
	}
	return true
}

// path is where the mount is reachable: the FUSE path gvfs-fuse exposes
// (/run/user/<uid>/gvfs/...), or, when gvfs-fuse is not serving the session,
// uri. The mount's DefaultLocation is not a substitute: it is the root INSIDE
// the mount, and measured on Ubuntu 24.04 it is "/", which read as a path is
// the host's root directory (go-fileshare/automount live run, 2026-11-16).
func (m *dbusMount) path(uri string) string {
	if p := fromCstr(m.FusePath); p != "" {
		return p
	}
	return uri
}

// splitUser separates DOMAIN\user into its domain and user.
func splitUser(user string) (domain, name string) {
	if d, n, ok := strings.Cut(user, `\`); ok {
		return d, n
	}
	return "", user
}
