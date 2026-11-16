module github.com/go-freedesktop/gvfs/real

go 1.27.2

require (
	github.com/go-filesystems/interface v0.5.0
	github.com/go-filesystems/smb v0.7.0
	github.com/go-freedesktop/gvfs v0.0.0
)

require (
	github.com/go-filesystems/hostcopy v0.1.0 // indirect
	github.com/godbus/dbus/v5 v5.2.4 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

replace github.com/go-freedesktop/gvfs => ../
