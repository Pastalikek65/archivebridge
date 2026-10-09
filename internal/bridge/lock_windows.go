//go:build windows

package bridge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

const (
	lockFileExclusive     = 0x00000002
	lockFileFailImmediate = 0x00000001
	errorLockViolation    = syscall.Errno(33)
)

var (
	lockFileExProc   = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	unlockFileExProc = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")
)

type outputLock struct {
	file       *os.File
	overlapped syscall.Overlapped
}

func acquireOutputLock(out string) (*outputLock, error) {
	name := filepath.Join(out, lockFilename)
	namep, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	// OPEN_ALWAYS plus OPEN_REPARSE_POINT creates or opens the lock without
	// following an existing symlink or other reparse point. Delete sharing is
	// intentionally absent so another process cannot replace the locked path.
	h, err := syscall.CreateFile(namep, syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot open output lock without following links: %w", err)
	}
	f := os.NewFile(uintptr(h), name)
	closeOnError := func(err error) (*outputLock, error) {
		_ = f.Close()
		return nil, err
	}
	if err := validateWindowsLockFile(name, f); err != nil {
		return closeOnError(err)
	}
	lock := &outputLock{file: f}
	r1, _, callErr := lockFileExProc.Call(uintptr(h), lockFileExclusive|lockFileFailImmediate, 0, 1, 0, uintptr(unsafe.Pointer(&lock.overlapped)))
	if r1 == 0 {
		if callErr == errorLockViolation {
			return closeOnError(ErrOutputLocked)
		}
		if callErr == nil || callErr == syscall.Errno(0) {
			return closeOnError(errors.New("LockFileEx failed without a Windows error code"))
		}
		return closeOnError(fmt.Errorf("cannot lock output: %w", callErr))
	}
	if err := validateWindowsLockFile(name, f); err != nil {
		_, _, _ = unlockFileExProc.Call(uintptr(h), 0, 1, 0, uintptr(unsafe.Pointer(&lock.overlapped)))
		return closeOnError(err)
	}
	return lock, nil
}

func validateWindowsLockFile(name string, f *os.File) error {
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
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&(syscall.FILE_ATTRIBUTE_DIRECTORY|syscall.FILE_ATTRIBUTE_REPARSE_POINT) != 0 || info.NumberOfLinks != 1 || info.FileSizeHigh != 0 || info.FileSizeLow != 0 {
		return errors.New("output lock must be a zero-byte, single-link regular file")
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
	namep, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	h, err := syscall.CreateFile(namep, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(h), name)
	defer f.Close()
	return validateWindowsLockFile(name, f)
}

func (l *outputLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	r1, _, err := unlockFileExProc.Call(uintptr(l.file.Fd()), 0, 1, 0, uintptr(unsafe.Pointer(&l.overlapped)))
	closeErr := l.file.Close()
	l.file = nil
	if r1 == 0 {
		if err != nil && err != syscall.Errno(0) {
			return err
		}
		return errors.New("cannot unlock output")
	}
	return closeErr
}
