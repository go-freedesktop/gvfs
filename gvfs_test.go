// Copyright (c) 2026 the go-freedesktop/gvfs authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package gvfs

import (
	"errors"
	"reflect"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestParseURI(t *testing.T) {
	tests := []struct {
		uri    string
		typ    string
		items  map[string]string
		prefix string
	}{
		{"smb://127.0.0.1:4455/Share", "smb-share", map[string]string{"server": "127.0.0.1", "share": "share", "port": "4455"}, "/"},
		{"smb://HOST/Data/sub/dir", "smb-share", map[string]string{"server": "host", "share": "data"}, "/"},
		{"smb://host:445/data", "smb-share", map[string]string{"server": "host", "share": "data"}, "/"},
		{"SMB://host//data", "smb-share", map[string]string{"server": "host", "share": "data"}, "/"},
		{"dav://h:8080/a/b", "dav", map[string]string{"host": "h", "ssl": "false", "port": "8080"}, "/a/b"},
		{"dav://h:80/", "dav", map[string]string{"host": "h", "ssl": "false"}, "/"},
		{"davs://h/x", "dav", map[string]string{"host": "h", "ssl": "true"}, "/x"},
		{"davs://h:443", "dav", map[string]string{"host": "h", "ssl": "true"}, ""},
		{"davs://h:444/", "dav", map[string]string{"host": "h", "ssl": "true", "port": "444"}, "/"},
		{"dav://[::1]:8080/p", "dav", map[string]string{"host": "[::1]", "ssl": "false", "port": "8080"}, "/p"},
	}
	for _, tc := range tests {
		l, err := parseURI(tc.uri)
		if err != nil {
			t.Errorf("%s: %v", tc.uri, err)
			continue
		}
		if l.typ != tc.typ || !reflect.DeepEqual(l.items, tc.items) || l.prefix != tc.prefix {
			t.Errorf("%s: got %q %v %q, want %q %v %q", tc.uri, l.typ, l.items, l.prefix, tc.typ, tc.items, tc.prefix)
		}
	}
}

func TestParseURIErrors(t *testing.T) {
	for _, uri := range []string{
		"://bad",                    // url.Parse error
		"smb://user:secret@h/share", // credentials
		"smb://user@h/share",        // credentials
		"smb:///share",              // no host
		"smb://h",                   // no share
		"smb://h/",                  // no share
		"smb://h:0/share",           // bad port
		"smb://h:70000/share",       // bad port
		"smb://h:x/share",           // bad port
		"ftp://h/share",             // scheme
		"/just/a/path",              // no scheme
	} {
		if _, err := parseURI(uri); !errors.Is(err, ErrInvalidURI) {
			t.Errorf("%s: err = %v, want ErrInvalidURI", uri, err)
		}
	}
}

func TestSpecWire(t *testing.T) {
	l, err := parseURI("smb://h:99/s")
	if err != nil {
		t.Fatal(err)
	}
	s := l.spec()
	if got := string(s.Prefix); got != "/\x00" {
		t.Errorf("prefix = %q", got)
	}
	want := map[string]string{"type": "smb-share", "server": "h", "share": "s", "port": "99"}
	if len(s.Items) != len(want) {
		t.Fatalf("items = %v", s.Items)
	}
	for k, v := range want {
		b, ok := s.Items[k].Value().([]byte)
		if !ok || string(b) != v+"\x00" {
			t.Errorf("item %s = %v, want %q NUL-terminated", k, s.Items[k], v)
		}
	}
	if got := dbus.SignatureOf(s).String(); got != "(aya{sv})" {
		t.Errorf("spec signature = %s", got)
	}
	if got := dbus.SignatureOf(dbusSource{}).String(); got != "(so)" {
		t.Errorf("source signature = %s", got)
	}
	if got := dbus.SignatureOf(dbusMount{}).String(); got != "(sossssssbay(aya{sv})ay)" {
		t.Errorf("mount signature = %s", got)
	}
}

func mountOf(typ string, extra map[string]string, prefix string) *dbusMount {
	items := map[string]dbus.Variant{"type": dbus.MakeVariant(cstr(typ))}
	for k, v := range extra {
		items[k] = dbus.MakeVariant(cstr(v))
	}
	return &dbusMount{Spec: dbusSpec{Prefix: cstr(prefix), Items: items}}
}

func TestMatches(t *testing.T) {
	smb, _ := parseURI("smb://h:99/s")
	dav, _ := parseURI("dav://h/dir/")
	base := map[string]string{"server": "h", "share": "s", "port": "99"}
	withUser := map[string]string{"server": "h", "share": "s", "port": "99", "user": "u", "domain": "d"}
	davItems := map[string]string{"host": "h", "ssl": "false"}

	bad := mountOf("smb-share", base, "/")
	bad.Spec.Items["server"] = dbus.MakeVariant("not bytes")

	for name, tc := range map[string]struct {
		loc  *location
		m    *dbusMount
		want bool
	}{
		"equal":              {smb, mountOf("smb-share", base, "/"), true},
		"user and domain":    {smb, mountOf("smb-share", withUser, "/"), true},
		"other type":         {smb, mountOf("smb-server", base, "/"), false},
		"missing item":       {smb, mountOf("smb-share", map[string]string{"server": "h", "share": "s"}, "/"), false},
		"extra item":         {smb, mountOf("smb-share", map[string]string{"server": "h", "share": "s", "port": "99", "x": "y"}, "/"), false},
		"other value":        {smb, mountOf("smb-share", map[string]string{"server": "h", "share": "t", "port": "99"}, "/"), false},
		"not a byte string":  {smb, bad, false},
		"dav equal":          {dav, mountOf("dav", davItems, "/dir"), true},
		"dav trailing slash": {dav, mountOf("dav", davItems, "/dir/"), true},
		"dav other prefix":   {dav, mountOf("dav", davItems, "/other"), false},
	} {
		if got := tc.loc.matches(tc.m); got != tc.want {
			t.Errorf("%s: matches = %v, want %v", name, got, tc.want)
		}
	}
}

func TestPath(t *testing.T) {
	m := &dbusMount{FusePath: cstr("/run/user/1000/gvfs/x"), DefaultLocation: cstr("smb://h/s")}
	if got := m.path(); got != "/run/user/1000/gvfs/x" {
		t.Errorf("fuse path = %q", got)
	}
	m.FusePath = cstr("")
	if got := m.path(); got != "smb://h/s" {
		t.Errorf("default location = %q", got)
	}
	m.DefaultLocation = nil
	if got := m.path(); got != "" {
		t.Errorf("empty = %q", got)
	}
}

func TestCstr(t *testing.T) {
	if got := cstr("ab"); string(got) != "ab\x00" {
		t.Errorf("cstr = %q", got)
	}
	if got := fromCstr([]byte("ab\x00cd")); got != "ab" {
		t.Errorf("fromCstr = %q", got)
	}
	if got := fromCstr([]byte("ab")); got != "ab" {
		t.Errorf("fromCstr without NUL = %q", got)
	}
}

func TestSplitUser(t *testing.T) {
	if d, n := splitUser(`CORP\alice`); d != "CORP" || n != "alice" {
		t.Errorf("got %q %q", d, n)
	}
	if d, n := splitUser("alice"); d != "" || n != "alice" {
		t.Errorf("got %q %q", d, n)
	}
}
