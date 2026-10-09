package bridge

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestZIPPreflightRejectsDeclaredEntryCountBeforeStandardReader(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "declared-too-many.zip")
	data := fakeEOCD(2, 2, 0, 0)
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	limits := DefaultLimits()
	limits.MaxEntries = 1
	_, err := Inspect(context.Background(), []string{filename}, limits)
	if err == nil || !strings.Contains(err.Error(), "archive entry limit exceeded") {
		t.Fatalf("Inspect error = %v, want bounded preflight entry-count rejection", err)
	}
}

func TestZIPPreflightRejectsOversizedCentralMetadataWithoutReadingIt(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "declared-large-central-directory.zip")
	data := fakeEOCD(0, 0, uint32(zipMaxDirectoryBytes+1), 0)
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Inspect(context.Background(), []string{filename}, DefaultLimits())
	if err == nil || !strings.Contains(err.Error(), "central-directory metadata exceeds") {
		t.Fatalf("Inspect error = %v, want bounded preflight metadata rejection", err)
	}
}

func TestZIPPreflightSupportsZIP64(t *testing.T) {
	data := emptyZIP64()
	if err := preflightZIP(bytes.NewReader(data), int64(len(data)), DefaultLimits(), 0); err != nil {
		t.Fatalf("ZIP64 preflight: %v", err)
	}
	if _, err := zip.NewReader(bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("standard ZIP reader rejected supported ZIP64 fixture: %v", err)
	}
}

func TestZIPPreflightRejectsSelfExtractingArchive(t *testing.T) {
	data := fakeEOCD(0, 0, 0, 1)
	if err := preflightZIP(bytes.NewReader(data), int64(len(data)), DefaultLimits(), 0); err == nil || !strings.Contains(err.Error(), "self-extracting ZIP") {
		t.Fatalf("preflight error = %v, want explicit self-extractor rejection", err)
	}
}

func fakeEOCD(recordsOnDisk, records uint16, directorySize, directoryOffset uint32) []byte {
	b := make([]byte, zipEOCDLength)
	binary.LittleEndian.PutUint32(b[0:4], zipEOCDSignature)
	binary.LittleEndian.PutUint16(b[4:6], 0)
	binary.LittleEndian.PutUint16(b[6:8], 0)
	binary.LittleEndian.PutUint16(b[8:10], recordsOnDisk)
	binary.LittleEndian.PutUint16(b[10:12], records)
	binary.LittleEndian.PutUint32(b[12:16], directorySize)
	binary.LittleEndian.PutUint32(b[16:20], directoryOffset)
	binary.LittleEndian.PutUint16(b[20:22], 0)
	return b
}

func emptyZIP64() []byte {
	b := make([]byte, zip64EOCDLength+zip64LocatorLength+zipEOCDLength)
	binary.LittleEndian.PutUint32(b[0:4], zip64EOCDSignature)
	binary.LittleEndian.PutUint64(b[4:12], 44)
	binary.LittleEndian.PutUint16(b[12:14], 45)
	binary.LittleEndian.PutUint16(b[14:16], 45)
	// Disk numbers and all entry/size/offset fields remain zero.
	loc := zip64EOCDLength
	binary.LittleEndian.PutUint32(b[loc:loc+4], zip64LocatorSignature)
	binary.LittleEndian.PutUint32(b[loc+4:loc+8], 0)
	binary.LittleEndian.PutUint64(b[loc+8:loc+16], 0)
	binary.LittleEndian.PutUint32(b[loc+16:loc+20], 1)
	eocd := loc + zip64LocatorLength
	binary.LittleEndian.PutUint32(b[eocd:eocd+4], zipEOCDSignature)
	binary.LittleEndian.PutUint16(b[eocd+4:eocd+6], 0)
	binary.LittleEndian.PutUint16(b[eocd+6:eocd+8], 0)
	binary.LittleEndian.PutUint16(b[eocd+8:eocd+10], 0xffff)
	binary.LittleEndian.PutUint16(b[eocd+10:eocd+12], 0xffff)
	binary.LittleEndian.PutUint32(b[eocd+12:eocd+16], 0xffffffff)
	binary.LittleEndian.PutUint32(b[eocd+16:eocd+20], 0xffffffff)
	return b
}
