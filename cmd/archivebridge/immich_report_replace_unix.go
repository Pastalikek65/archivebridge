//go:build !windows

package main

import (
	"os"
	"path/filepath"
)

func replaceOwnedReport(source, destination string) error {
	if filepath.Dir(source) != filepath.Dir(destination) {
		return os.ErrInvalid
	}
	return os.Rename(source, destination)
}

func installNewReport(source, destination string) error {
	if filepath.Dir(source) != filepath.Dir(destination) {
		return os.ErrInvalid
	}
	return os.Link(source, destination)
}

func syncReportDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
