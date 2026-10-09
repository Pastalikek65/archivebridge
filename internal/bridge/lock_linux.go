//go:build linux

package bridge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type outputLock struct {
	file *os.File
}

func acquireOutputLock(out string) (*outputLock, error) {
	name := filepath.Join(out, lockFilename)
	fd, err := syscall.Open(name, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("cannot open output lock without following links: %w", err)
	}
	f := os.NewFile(uintptr(fd), name)
	closeOnError := func(err error) (*outputLock, error) {
		_ = f.Close()
		return nil, err
	}
	if err := validateLockFile(name, f); err != nil {
		return closeOnError(err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return closeOnError(ErrOutputLocked)
		}
		return closeOnError(fmt.Errorf("cannot lock output: %w", err))
	}
	// Recheck after locking so a replaced or newly linked path is never accepted.
	if err := validateLockFile(name, f); err != nil {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		return closeOnError(err)
	}
	return &outputLock{file: f}, nil
}

func validateLockFile(name string, f *os.File) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() != 0 {
		return errors.New("output lock must be a zero-byte regular file")
	}
	pathInfo, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() || !os.SameFile(st, pathInfo) {
		return errors.New("output lock path is not the opened regular file")
	}
	if info, ok := st.Sys().(*syscall.Stat_t); !ok || info.Nlink != 1 {
		return errors.New("output lock must have exactly one link")
	}
	return nil
}

func validateExistingOutputLock(out string) error {
	name := filepath.Join(out, lockFilename)
	if _, err := os.Lstat(name); errors.Is(err, os.ErrNotExist) {
		return nil // Legacy schema-1 exports predate the persistent lock file.
	} else if err != nil {
		return err
	}
	fd, err := syscall.Open(name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	return validateLockFile(name, f)
}

func (l *outputLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	fd := int(l.file.Fd())
	unlockErr := syscall.Flock(fd, syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
