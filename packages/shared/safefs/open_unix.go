//go:build !windows

package safefs

import "syscall"

const regularOpenFlags = syscall.O_NONBLOCK | syscall.O_NOFOLLOW
