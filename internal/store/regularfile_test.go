package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRegularFileReadsRejectSameInodeMutationAtFinalBoundary(t *testing.T) {
	tests := []struct {
		name string
		read func(int, string, FileIdentity, regularFileReadHooks) error
		want error
	}{
		{
			name: "bounded read",
			read: func(parentFD int, name string, _ FileIdentity, hooks regularFileReadHooks) error {
				_, err := readRegularFileAtWithHooks(parentFD, name, 1<<20, hooks)
				return err
			},
			want: errRegularFileChanged,
		},
		{
			name: "owned-tree hash",
			read: func(parentFD int, name string, identity FileIdentity, hooks regularFileReadHooks) error {
				_, _, err := hashFileAtWithHooks(parentFD, name, identity, hooks)
				return err
			},
			want: ErrOwnedTreeChanged,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dirPath := t.TempDir()
			const name = "payload"
			path := filepath.Join(dirPath, name)
			if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
				t.Fatal(err)
			}
			dir, err := os.Open(dirPath)
			if err != nil {
				t.Fatal(err)
			}
			defer dir.Close()
			before, err := statAt(int(dir.Fd()), name)
			if err != nil {
				t.Fatal(err)
			}
			identity := identityFromStat(&before)
			hooks := regularFileReadHooks{beforePostStat: func() error {
				file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
				if err != nil {
					return err
				}
				if _, err := file.Write([]byte("replaced")); err != nil {
					_ = file.Close()
					return err
				}
				if err := file.Sync(); err != nil {
					_ = file.Close()
					return err
				}
				if err := file.Close(); err != nil {
					return err
				}
				// Force a timestamp difference even on filesystems with coarse
				// automatic timestamp updates.
				changed := time.Unix(before.Mtim.Sec+2, before.Mtim.Nsec)
				return os.Chtimes(path, changed, changed)
			}}

			err = tt.read(int(dir.Fd()), name, identity, hooks)
			if !errors.Is(err, tt.want) {
				t.Fatalf("same-inode mutation must return %v, got %v", tt.want, err)
			}
			after, err := statAt(int(dir.Fd()), name)
			if err != nil {
				t.Fatal(err)
			}
			if identityFromStat(&after) != identity {
				t.Fatal("test mutation unexpectedly replaced the inode")
			}
			if after.Mode&unix.S_IFMT != unix.S_IFREG {
				t.Fatal("test mutation unexpectedly changed the file type")
			}
		})
	}
}
