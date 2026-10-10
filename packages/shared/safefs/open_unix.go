//go:build !windows

package safefs

import "syscall"

const externalRegularOpenFlags = syscall.O_NONBLOCK
const regularOpenFlags = externalRegularOpenFlags | syscall.O_NOFOLLOW
