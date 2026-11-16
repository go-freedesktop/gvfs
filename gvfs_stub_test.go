// Copyright (c) 2026 the go-freedesktop/gvfs authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package gvfs

import (
	"context"
	"errors"
	"testing"
)

func TestStubUnsupported(t *testing.T) {
	ctx := context.Background()
	c := NewClient(nil)
	if p, err := Mount(ctx, "smb://h/s", "u", "p"); p != "" || !errors.Is(err, ErrUnsupported) {
		t.Errorf("Mount = %q, %v", p, err)
	}
	if p, err := c.Mount(ctx, "smb://h/s", "u", "p"); p != "" || !errors.Is(err, ErrUnsupported) {
		t.Errorf("Client.Mount = %q, %v", p, err)
	}
	if err := Unmount(ctx, "smb://h/s"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Unmount = %v", err)
	}
	if err := c.Unmount(ctx, "smb://h/s"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Client.Unmount = %v", err)
	}
}
