//go:build !linux

package store

// Only Linux exposes file handles to user space. macOS needs none: APFS never
// reuses an inode number, so device and inode are a persistent identity there.

func fileHandleAt(int, string) (string, error) { return "", nil }

func fileHandleOf(int) (string, error) { return "", nil }

func identityHandleSupported(string) (bool, error) { return false, nil }
