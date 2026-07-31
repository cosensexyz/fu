//go:build !darwin && !linux

package store

import "errors"

func renameNoReplace(int, string, int, string) error {
	return errors.New("atomic no-replace rename is unavailable on this platform")
}
