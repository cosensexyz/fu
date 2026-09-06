//go:build !linux

package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHandleIsEmptyOutsideLinux(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entry")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	identity, _, err := entryIdentityAt(unixAtFDCWD, path)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Handle != "" || !identity.Valid() {
		t.Fatalf("outside Linux the identity is device and inode only, got %+v", identity)
	}
	supported, err := identityHandleSupported(dir)
	if err != nil || supported {
		t.Fatalf("identityHandleSupported = %v, %v; want false, nil", supported, err)
	}
}
