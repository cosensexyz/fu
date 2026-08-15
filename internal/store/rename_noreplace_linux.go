//go:build linux

package store

import "golang.org/x/sys/unix"

func renameNoReplace(oldDirFD int, oldName string, newDirFD int, newName string) error {
	return mapAtomicRenameError("no-replace", unix.Renameat2(oldDirFD, oldName, newDirFD, newName, unix.RENAME_NOREPLACE))
}
