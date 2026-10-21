package safefs

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var lockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

func tryLockFile(file *os.File) error {
	if err := lockFileEx.Find(); err != nil {
		return err
	}
	var overlapped syscall.Overlapped
	const exclusiveNonblocking = 0x2 | 0x1
	result, _, err := lockFileEx.Call(file.Fd(), exclusiveNonblocking, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if result != 0 {
		return nil
	}
	if errors.Is(err, syscall.Errno(33)) {
		return ErrLocked
	} // ERROR_LOCK_VIOLATION
	return err
}
