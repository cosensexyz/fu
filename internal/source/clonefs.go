package source

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/go-git/go-billy/v5"
)

// ErrSourceTooLarge reports that a git source exhausted the aggregate clone
// budget shared by repository objects and checkout files.
var ErrSourceTooLarge = errors.New("git source exceeds clone size limit")

type cloneByteBudget struct {
	mu        sync.Mutex
	remaining int64
}

func (b *cloneByteBudget) reserve(size int64) error {
	if size <= 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if size > b.remaining {
		return ErrSourceTooLarge
	}
	b.remaining -= size
	return nil
}

func (b *cloneByteBudget) refund(size int64) {
	if size <= 0 {
		return
	}
	b.mu.Lock()
	b.remaining += size
	b.mu.Unlock()
}

type limitedCloneFilesystem struct {
	billy.Filesystem
	budget *cloneByteBudget
	ctx    context.Context
}

func (f *limitedCloneFilesystem) Create(name string) (billy.File, error) {
	file, err := f.Filesystem.Create(name)
	return f.wrap(file), err
}

func (f *limitedCloneFilesystem) Open(name string) (billy.File, error) {
	file, err := f.Filesystem.Open(name)
	return f.wrap(file), err
}

func (f *limitedCloneFilesystem) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	file, err := f.Filesystem.OpenFile(name, flag, perm)
	return f.wrap(file), err
}

func (f *limitedCloneFilesystem) TempFile(dir, prefix string) (billy.File, error) {
	file, err := f.Filesystem.TempFile(dir, prefix)
	return f.wrap(file), err
}

func (f *limitedCloneFilesystem) Symlink(target, link string) error {
	if err := cloneContextError(f.ctx); err != nil {
		return err
	}
	size := int64(len(target))
	if err := f.budget.reserve(size); err != nil {
		return err
	}
	if err := f.Filesystem.Symlink(target, link); err != nil {
		f.budget.refund(size)
		return err
	}
	return nil
}

func (f *limitedCloneFilesystem) Chroot(path string) (billy.Filesystem, error) {
	child, err := f.Filesystem.Chroot(path)
	if err != nil {
		return nil, err
	}
	return &limitedCloneFilesystem{Filesystem: child, budget: f.budget, ctx: f.ctx}, nil
}

func (f *limitedCloneFilesystem) Capabilities() billy.Capability {
	return billy.Capabilities(f.Filesystem)
}

func (f *limitedCloneFilesystem) wrap(file billy.File) billy.File {
	if file == nil {
		return nil
	}
	return &limitedCloneFile{File: file, budget: f.budget, ctx: f.ctx}
}

type limitedCloneFile struct {
	billy.File
	budget *cloneByteBudget
	ctx    context.Context
}

func (f *limitedCloneFile) Write(p []byte) (int, error) {
	if err := cloneContextError(f.ctx); err != nil {
		return 0, err
	}
	if err := f.budget.reserve(int64(len(p))); err != nil {
		return 0, err
	}
	n, err := f.File.Write(p)
	f.budget.refund(int64(len(p) - n))
	return n, err
}

func (f *limitedCloneFile) Truncate(size int64) error {
	if err := cloneContextError(f.ctx); err != nil {
		return err
	}
	offset, err := f.File.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	currentSize, err := f.File.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := f.File.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	delta := size - currentSize
	if err := f.budget.reserve(delta); err != nil {
		return err
	}
	if err := f.File.Truncate(size); err != nil {
		f.budget.refund(delta)
		return err
	}
	if delta < 0 {
		f.budget.refund(-delta)
	}
	return nil
}

func cloneContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
