package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// The placeholder cap is a safety net, comfortably above the 8192 inodes
// per group of ext4's default layout. Filling this many without reaching
// the original's number means the allocator is not handing out the lowest
// free number from the original's group -- the original may sit in a
// higher group than the one being filled -- and the helper stops steering
// rather than go on consuming the filesystem.
const maxInodeFillers = 16384

// Once the original's number is released, another process can take it, or
// free a lower number that diverts the replacement, before the replacement
// is created. A taker in a parallel test package usually holds the number
// until its fixture is cleaned up, so the required Linux coverage keeps
// trying for this long; nothing on the happy path waits at all. A variable
// so the deadline's own test can shorten it.
var replaceOnSameInodeDeadline = 10 * time.Second

// InodeReuse reports how a replacement made by ReplaceOnSameInode ended up.
type InodeReuse struct {
	// Reused is whether the replacement holds the original's inode number.
	Reused bool
	// Miss says why it does not; empty when Reused.
	Miss string
}

// ReplaceOnSameInode replaces parent/name with a fresh, empty entry of the
// requested kind -- a directory or a regular file -- and makes it land on
// the inode number the original held, on any filesystem that hands freed
// numbers back at all. The replacement is installed under the name either
// way, and callers fill in its content afterwards: writing into the empty
// file or the empty directory keeps the number, and the file is created
// 0o644, so a caller that needs another mode sets it then.
//
// A plain remove-and-recreate reuses the original's number only while no
// lower number is free in the group, because ext4 with a journal hands out
// the lowest free one. A lower number usually is free: fu's own temporary
// files and the cleanup of tests in parallel packages release numbers all
// the time, and every retry of the plain form then lands on that hole
// instead. The helper first occupies every free number below the
// original's with placeholders of the same kind, so that they draw from
// the same group the replacement will, then releases the original and
// creates the replacement, which now takes the lowest free number: the
// original's. A directory original is emptied before the placeholders go
// in -- its contents are gone whether or not the number comes back --
// since their numbers are released with it and any of them below the
// original's would otherwise be a hole the fill never saw. The
// placeholders are removed before returning. When the original itself
// sits in a group the allocator is not filling, no placeholder can steer
// the replacement onto it; the helper then stops at maxInodeFillers and
// reports the miss.
//
// A concurrent process can still defeat one attempt in the window between
// release and creation: by taking the original's number, or by freeing a
// lower one that the replacement then lands on. Under FileHandlesRequired
// the helper keeps allocating placeholders until the deadline -- the one
// that lands on the original's number becomes the replacement -- and
// reports the miss with its reason when the deadline passes; elsewhere one
// attempt is all the filesystem gets, since APFS never hands a number back
// and waiting would only slow every caller down.
func ReplaceOnSameInode(parent, name string, dir bool) (reuse InodeReuse, err error) {
	path := filepath.Join(parent, name)
	info, err := os.Lstat(path)
	if err != nil {
		return InodeReuse{}, err
	}
	original := info.Sys().(*syscall.Stat_t).Ino
	if info.IsDir() {
		contents, err := os.ReadDir(path)
		if err != nil {
			return InodeReuse{}, err
		}
		for _, entry := range contents {
			if err := os.RemoveAll(filepath.Join(path, entry.Name())); err != nil {
				return InodeReuse{}, err
			}
		}
	}
	var fillers []string
	defer func() {
		for _, filler := range fillers {
			if removeErr := os.Remove(filler); removeErr != nil && err == nil {
				err = removeErr
			}
		}
	}()
	// fill allocates placeholders until one lands at or beyond the original's
	// number, or the cap is reached. While the original exists, that fills
	// every free number below it. Once the original has been released, a
	// placeholder landing exactly on its number is the replacement and is
	// returned as landed; it stays in fillers until it has been renamed into
	// place, so an error on the way still removes it. A placeholder landing
	// beyond the number has done its job -- no free number remains below --
	// and goes at once, so retries do not eat into the cap.
	fill := func() (landed string, capped bool, err error) {
		for len(fillers) < maxInodeFillers {
			filler := filepath.Join(parent, fmt.Sprintf(".inode-filler-%d", len(fillers)))
			if err := createEmpty(filler, dir); err != nil {
				return "", false, err
			}
			fillers = append(fillers, filler)
			got, err := inodeNumber(filler)
			if err != nil {
				return "", false, err
			}
			if got == original {
				return filler, false, nil
			}
			if got > original {
				if err := os.Remove(filler); err != nil {
					return "", false, err
				}
				fillers = fillers[:len(fillers)-1]
				return "", false, nil
			}
		}
		return "", true, nil
	}
	_, capped, err := fill()
	if err != nil {
		return InodeReuse{}, err
	}
	if err := os.Remove(path); err != nil {
		return InodeReuse{}, err
	}
	if err := createEmpty(path, dir); err != nil {
		return InodeReuse{}, err
	}
	got, err := inodeNumber(path)
	if err != nil {
		return InodeReuse{}, err
	}
	if got == original {
		return InodeReuse{Reused: true}, nil
	}
	if capped {
		return InodeReuse{Miss: cappedMiss(len(fillers), original)}, nil
	}
	if !FileHandlesRequired() {
		return InodeReuse{Miss: fmt.Sprintf("the filesystem did not hand inode %d back on one attempt; the replacement holds %d", original, got)}, nil
	}
	deadline := time.Now().Add(replaceOnSameInodeDeadline)
	for time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		landed, capped, err := fill()
		if err != nil {
			return InodeReuse{}, err
		}
		if landed != "" {
			if err := os.Remove(path); err != nil {
				return InodeReuse{}, err
			}
			if err := os.Rename(landed, path); err != nil {
				return InodeReuse{}, err
			}
			fillers = fillers[:len(fillers)-1]
			return InodeReuse{Reused: true}, nil
		}
		if capped {
			return InodeReuse{Miss: cappedMiss(len(fillers), original)}, nil
		}
	}
	return InodeReuse{Miss: fmt.Sprintf("inode %d was not handed back within %s: a concurrent allocation may still hold it, or this filesystem does not reuse freed numbers; the replacement holds %d", original, replaceOnSameInodeDeadline, got)}, nil
}

func cappedMiss(fillers int, original uint64) string {
	return fmt.Sprintf("filled %d placeholders without reaching inode %d: the allocator is not handing out the lowest free number from the original's group", fillers, original)
}

func createEmpty(path string, dir bool) error {
	if dir {
		return os.Mkdir(path, 0o755)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

func inodeNumber(path string) (uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	return info.Sys().(*syscall.Stat_t).Ino, nil
}
