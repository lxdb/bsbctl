//go:build darwin

package launchagent

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	renameSwap = 0x00000002
	renameExcl = 0x00000004
)

func platformExchange(directoryFD int, first, second string) error {
	return renameatx(directoryFD, first, directoryFD, second, renameSwap)
}

func platformRenameExclusive(directoryFD int, oldName, newName string) error {
	return renameatx(directoryFD, oldName, directoryFD, newName, renameExcl)
}

func renameatx(oldDirectoryFD int, oldName string, newDirectoryFD int, newName string, flags uintptr) error {
	oldPointer, err := unix.BytePtrFromString(oldName)
	if err != nil {
		return err
	}
	newPointer, err := unix.BytePtrFromString(newName)
	if err != nil {
		return err
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_RENAMEATX_NP,
		uintptr(oldDirectoryFD), uintptr(unsafe.Pointer(oldPointer)),
		uintptr(newDirectoryFD), uintptr(unsafe.Pointer(newPointer)),
		flags, 0,
	)
	runtime.KeepAlive(oldPointer)
	runtime.KeepAlive(newPointer)
	if errno != 0 {
		return errno
	}
	return nil
}
