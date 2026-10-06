//go:build !windows

package archive

import "golang.org/x/sys/unix"

const regularOpenFlags = unix.O_NONBLOCK | unix.O_NOFOLLOW
