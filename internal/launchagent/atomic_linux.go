//go:build linux

package launchagent

import "golang.org/x/sys/unix"

func platformExchange(directoryFD int, first, second string) error {
	return unix.Renameat2(directoryFD, first, directoryFD, second, unix.RENAME_EXCHANGE)
}

func platformRenameExclusive(directoryFD int, oldName, newName string) error {
	return unix.Renameat2(directoryFD, oldName, directoryFD, newName, unix.RENAME_NOREPLACE)
}
