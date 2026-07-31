//go:build !darwin && !linux

package store

import "errors"

func renameExchange(int, string, int, string) error {
	return errors.New("atomic exchange rename is unavailable on this platform")
}
