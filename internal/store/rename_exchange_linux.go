//go:build linux

package store

import "golang.org/x/sys/unix"

func renameExchange(oldDirFD int, oldName string, newDirFD int, newName string) error {
	return mapAtomicRenameError("exchange", unix.Renameat2(oldDirFD, oldName, newDirFD, newName, unix.RENAME_EXCHANGE))
}
