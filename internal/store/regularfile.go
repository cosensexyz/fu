package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var errRegularFileChanged = errors.New("regular file changed while being opened")

type regularFileReadHooks struct {
	beforePostStat func() error
}

type regularFileStamp struct {
	identity  FileIdentity
	mode      uint32
	size      int64
	mtimeSec  int64
	mtimeNsec int64
	ctimeSec  int64
	ctimeNsec int64
}

func stampRegularFile(stat *unix.Stat_t) regularFileStamp {
	return regularFileStamp{
		identity:  identityFromStat(stat),
		mode:      uint32(stat.Mode),
		size:      stat.Size,
		mtimeSec:  int64(stat.Mtim.Sec),
		mtimeNsec: int64(stat.Mtim.Nsec),
		ctimeSec:  int64(stat.Ctim.Sec),
		ctimeNsec: int64(stat.Ctim.Nsec),
	}
}

func verifyRegularFileRead(name string, before, after *unix.Stat_t, byteCount int64) error {
	if err := requireRegularStat(name, after); err != nil {
		return fmt.Errorf("%w: %q changed type during read: %v", errRegularFileChanged, name, err)
	}
	if stampRegularFile(before) != stampRegularFile(after) || byteCount != before.Size {
		return fmt.Errorf("%w: %q metadata or byte count changed during read", errRegularFileChanged, name)
	}
	return nil
}

func finishRegularFileRead(file *os.File, name string, before unix.Stat_t, byteCount int64, hooks regularFileReadHooks) error {
	if hooks.beforePostStat != nil {
		if err := hooks.beforePostStat(); err != nil {
			_ = file.Close()
			return err
		}
	}
	var after unix.Stat_t
	statErr := unix.Fstat(int(file.Fd()), &after)
	closeErr := file.Close()
	if statErr != nil {
		return statErr
	}
	if closeErr != nil {
		return closeErr
	}
	return verifyRegularFileRead(name, &before, &after, byteCount)
}

func requireRegularStat(name string, stat *unix.Stat_t) error {
	_, kind, err := modeAndKind(stat)
	if err != nil {
		return fmt.Errorf("inspect %q: %w", name, err)
	}
	if kind != ownedFile {
		return fmt.Errorf("%w: %q is a %s, not a regular file", errUnsupportedOwnedType, name, kind)
	}
	return nil
}

func openRegularFileAt(parentFD int, name string) (*os.File, unix.Stat_t, error) {
	return openRegularFileAtMode(parentFD, name, unix.O_RDONLY)
}

// openRegularFileAtMode is openRegularFileAt with an explicit access mode for
// callers that need more than a read-only descriptor.
func openRegularFileAtMode(parentFD int, name string, access int) (*os.File, unix.Stat_t, error) {
	observed, err := statAt(parentFD, name)
	if err != nil {
		return nil, unix.Stat_t{}, err
	}
	if err := requireRegularStat(name, &observed); err != nil {
		return nil, unix.Stat_t{}, err
	}
	fd, err := unix.Openat(parentFD, name,
		access|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, unix.Stat_t{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, unix.Stat_t{}, errors.New("invalid regular-file descriptor")
	}
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		_ = file.Close()
		return nil, unix.Stat_t{}, err
	}
	if err := requireRegularStat(name, &opened); err != nil {
		_ = file.Close()
		return nil, unix.Stat_t{}, fmt.Errorf("%w: %q changed type after classification: %v", errRegularFileChanged, name, err)
	}
	if identityFromStat(&opened) != identityFromStat(&observed) {
		_ = file.Close()
		return nil, unix.Stat_t{}, fmt.Errorf("%w: %q changed identity after classification", errRegularFileChanged, name)
	}
	return file, opened, nil
}

func readRegularFileAt(parentFD int, name string, maxBytes int64) ([]byte, error) {
	return readRegularFileAtWithHooks(parentFD, name, maxBytes, regularFileReadHooks{})
}

func readRegularFileAtWithHooks(parentFD int, name string, maxBytes int64, hooks regularFileReadHooks) ([]byte, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("read regular file %q: negative size limit %d", name, maxBytes)
	}
	file, stat, err := openRegularFileAt(parentFD, name)
	if err != nil {
		return nil, err
	}
	if stat.Size < 0 || stat.Size > maxBytes {
		_ = file.Close()
		return nil, fmt.Errorf("regular file %q size %d exceeds limit %d", name, stat.Size, maxBytes)
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if readErr != nil {
		_ = file.Close()
		return nil, readErr
	}
	if err := finishRegularFileRead(file, name, stat, int64(len(raw)), hooks); err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("regular file %q exceeds limit %d while being read", name, maxBytes)
	}
	return raw, nil
}

// ReadRegularFile reads a bounded regular file without following the final
// component and without allowing a special-file replacement to block.
func ReadRegularFile(path string, maxBytes int64) ([]byte, error) {
	return readRegularFileWithHooks(path, maxBytes, regularFileReadHooks{})
}

func readRegularFileWithHooks(path string, maxBytes int64, hooks regularFileReadHooks) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return readRegularFileRootWithHooks(root, filepath.Base(path), maxBytes, hooks)
}

// ReadRegularFileRoot is ReadRegularFile relative to an already checked root.
func ReadRegularFileRoot(root *os.Root, path string, maxBytes int64) ([]byte, error) {
	return readRegularFileRootWithHooks(root, path, maxBytes, regularFileReadHooks{})
}

func readRegularFileRootWithHooks(root *os.Root, path string, maxBytes int64, hooks regularFileReadHooks) ([]byte, error) {
	dir := filepath.Dir(path)
	parent, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return readRegularFileAtWithHooks(int(parent.Fd()), filepath.Base(path), maxBytes, hooks)
}
