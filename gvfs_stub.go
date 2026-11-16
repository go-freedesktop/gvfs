// Copyright (c) 2026 the go-freedesktop/gvfs authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package gvfs

import (
	"context"

	"github.com/godbus/dbus/v5"
)

// Client is the non-Linux stand-in: every method returns ErrUnsupported.
type Client struct{}

// NewClient returns a stub Client.
func NewClient(conn *dbus.Conn) *Client { return &Client{} }

// Mount returns ErrUnsupported: gvfs is a Linux desktop service.
func Mount(ctx context.Context, uri, user, password string) (mountPath string, err error) {
	return "", ErrUnsupported
}

// Unmount returns ErrUnsupported.
func Unmount(ctx context.Context, uri string) error { return ErrUnsupported }

// Mount returns ErrUnsupported.
func (c *Client) Mount(ctx context.Context, uri, user, password string) (mountPath string, err error) {
	return "", ErrUnsupported
}

// Unmount returns ErrUnsupported.
func (c *Client) Unmount(ctx context.Context, uri string) error { return ErrUnsupported }
