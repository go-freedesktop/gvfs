module github.com/go-freedesktop/gvfs/real

go 1.27.4

require (
	github.com/go-filesystems/interface v0.6.0
	github.com/go-filesystems/smb v0.20.1
	github.com/go-freedesktop/gvfs v0.1.1
)

require (
	github.com/go-filesystems/hostcopy v0.1.0 // indirect
	github.com/go-filesystems/inflight v0.6.0 // indirect
	github.com/godbus/dbus/v5 v5.2.4 // indirect
	github.com/quic-go/quic-go v0.64.1 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

replace github.com/go-freedesktop/gvfs => ../
