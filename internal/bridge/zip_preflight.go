package bridge

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"
)

const (
	zipEOCDSignature       = 0x06054b50
	zip64LocatorSignature  = 0x07064b50
	zip64EOCDSignature     = 0x06064b50
	zipCentralSignature    = 0x02014b50
	zipLocalSignature      = 0x04034b50
	zipEOCDLength          = 22
	zip64LocatorLength     = 20
	zip64EOCDLength        = 56
	zipCentralHeaderLength = 46
	zipMaxCommentLength    = 1<<16 - 1
	zipMaxDirectoryBytes   = 64 << 20
	zipMaxNameBytes        = 1024
	zipMaxPathBytes        = 16 << 20
)

// preflightZIP bounds the metadata that archive/zip will allocate while
// constructing its File slice. It also checks the central names before the
// standard reader allocates per-entry headers. Self-extracting and spanned
// archives are deliberately unsupported: accepted archives have one disk and
// a zero base offset.
func preflightZIP(r io.ReaderAt, fileSize int64, limits Limits, priorEntries int) error {
	if fileSize < zipEOCDLength {
		return errors.New("ZIP end-of-central-directory record is missing")
	}
	var prefix [4]byte
	if err := readAtFull(r, prefix[:], 0); err != nil {
		return errors.New("ZIP first record is truncated")
	}
	firstSignature := binary.LittleEndian.Uint32(prefix[:])
	if firstSignature != zipLocalSignature && firstSignature != zipEOCDSignature && firstSignature != zip64EOCDSignature {
		return errors.New("self-extracting ZIP or non-ZIP preamble is unsupported")
	}
	tailSize := int64(zipEOCDLength + zipMaxCommentLength)
	if tailSize > fileSize {
		tailSize = fileSize
	}
	tail := make([]byte, int(tailSize))
	if err := readAtFull(r, tail, fileSize-tailSize); err != nil {
		return errors.New("ZIP end-of-central-directory record is truncated")
	}

	// ZIP comments may contain the EOCD signature. Require a candidate whose
	// declared comment ends at EOF, choosing the last such candidate.
	eocdRel := -1
	for i := len(tail) - zipEOCDLength; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:i+4]) != zipEOCDSignature {
			continue
		}
		commentLen := int(binary.LittleEndian.Uint16(tail[i+20 : i+22]))
		if i+zipEOCDLength+commentLen == len(tail) {
			eocdRel = i
			break
		}
	}
	if eocdRel < 0 {
		return errors.New("ZIP end-of-central-directory record is missing or has trailing data")
	}
	eocdOffset := fileSize - tailSize + int64(eocdRel)
	eocd := tail[eocdRel : eocdRel+zipEOCDLength]
	disk := binary.LittleEndian.Uint16(eocd[4:6])
	cdDisk := binary.LittleEndian.Uint16(eocd[6:8])
	recordsOnDisk := binary.LittleEndian.Uint16(eocd[8:10])
	recordsTotal16 := binary.LittleEndian.Uint16(eocd[10:12])
	directorySize32 := binary.LittleEndian.Uint32(eocd[12:16])
	directoryOffset32 := binary.LittleEndian.Uint32(eocd[16:20])

	zip64 := recordsOnDisk == 0xffff || recordsTotal16 == 0xffff || directorySize32 == 0xffffffff || directoryOffset32 == 0xffffffff
	var records, directorySize, directoryOffset uint64
	var directoryEnd uint64
	if zip64 {
		if eocdOffset < zip64LocatorLength {
			return errors.New("ZIP64 locator is missing")
		}
		locatorOffset := eocdOffset - zip64LocatorLength
		locator := make([]byte, zip64LocatorLength)
		if err := readAtFull(r, locator, locatorOffset); err != nil {
			return errors.New("ZIP64 locator is truncated")
		}
		if binary.LittleEndian.Uint32(locator[0:4]) != zip64LocatorSignature ||
			binary.LittleEndian.Uint32(locator[4:8]) != 0 ||
			binary.LittleEndian.Uint32(locator[16:20]) != 1 {
			return errors.New("spanned ZIP64 archives are unsupported")
		}
		zip64Offset := binary.LittleEndian.Uint64(locator[8:16])
		if zip64Offset > uint64(^uint64(0)>>1) || zip64Offset > uint64(locatorOffset) {
			return errors.New("ZIP64 end record offset is out of bounds")
		}
		var header [zip64EOCDLength]byte
		if err := readAtFull(r, header[:], int64(zip64Offset)); err != nil {
			return errors.New("ZIP64 end record is truncated")
		}
		if binary.LittleEndian.Uint32(header[0:4]) != zip64EOCDSignature {
			return errors.New("ZIP64 end record has an invalid signature")
		}
		zip64RecordSize := binary.LittleEndian.Uint64(header[4:12])
		if zip64RecordSize < 44 || zip64RecordSize > zipMaxDirectoryBytes {
			return errors.New("ZIP64 end record metadata exceeds configured bounds")
		}
		if zip64Offset > ^uint64(0)-12-zip64RecordSize || zip64Offset+12+zip64RecordSize != uint64(locatorOffset) {
			return errors.New("ZIP64 end record has an invalid extent")
		}
		zip64Disk := binary.LittleEndian.Uint32(header[16:20])
		zip64CDDisk := binary.LittleEndian.Uint32(header[20:24])
		recordsOnDisk64 := binary.LittleEndian.Uint64(header[24:32])
		records = binary.LittleEndian.Uint64(header[32:40])
		directorySize = binary.LittleEndian.Uint64(header[40:48])
		directoryOffset = binary.LittleEndian.Uint64(header[48:56])
		if zip64Disk != 0 || zip64CDDisk != 0 || recordsOnDisk64 != records {
			return errors.New("spanned ZIP64 archives are unsupported")
		}
		directoryEnd = zip64Offset
	} else {
		if disk != 0 || cdDisk != 0 || recordsOnDisk != recordsTotal16 {
			return errors.New("spanned ZIP archives are unsupported")
		}
		records = uint64(recordsTotal16)
		directorySize = uint64(directorySize32)
		directoryOffset = uint64(directoryOffset32)
		directoryEnd = uint64(eocdOffset)
	}

	if directorySize > zipMaxDirectoryBytes {
		return fmt.Errorf("ZIP central-directory metadata exceeds %d bytes", zipMaxDirectoryBytes)
	}
	if directoryOffset > uint64(^uint64(0)>>1) || directorySize > ^uint64(0)-directoryOffset || directoryOffset+directorySize != directoryEnd {
		return errors.New("self-extracting ZIP or invalid central-directory offset is unsupported")
	}
	if directoryEnd > uint64(fileSize) || directoryOffset > uint64(fileSize) {
		return errors.New("ZIP central-directory extent is out of bounds")
	}
	if priorEntries < 0 || priorEntries > limits.MaxEntries || records > uint64(limits.MaxEntries-priorEntries) {
		return errors.New("archive entry limit exceeded")
	}
	if records == 0 && (directoryOffset != 0 || directorySize != 0 || firstSignature == zipLocalSignature) {
		return errors.New("empty ZIP contains an unsupported preamble")
	}
	if records != 0 && firstSignature != zipLocalSignature {
		return errors.New("ZIP members must start with a local file record")
	}

	seen := make(map[string]struct{}, min(int(records), 4096))
	var pathBytes int64
	offset := directoryOffset
	for i := uint64(0); i < records; i++ {
		if offset > directoryEnd || directoryEnd-offset < zipCentralHeaderLength {
			return errors.New("ZIP central directory has a truncated member header")
		}
		var fixed [zipCentralHeaderLength]byte
		if err := readAtFull(r, fixed[:], int64(offset)); err != nil {
			return errors.New("ZIP central directory has a truncated member header")
		}
		if binary.LittleEndian.Uint32(fixed[0:4]) != zipCentralSignature {
			return errors.New("ZIP central directory has an invalid member signature")
		}
		flags := binary.LittleEndian.Uint16(fixed[8:10])
		if flags&(0x0001|0x0040|0x2000) != 0 {
			return errors.New("encrypted ZIP members are unsupported")
		}
		if binary.LittleEndian.Uint16(fixed[34:36]) != 0 {
			return errors.New("spanned ZIP members are unsupported")
		}
		nameLen := int(binary.LittleEndian.Uint16(fixed[28:30]))
		extraLen := int(binary.LittleEndian.Uint16(fixed[30:32]))
		commentLen := int(binary.LittleEndian.Uint16(fixed[32:34]))
		variableLen := uint64(nameLen + extraLen + commentLen)
		if nameLen == 0 || nameLen > zipMaxNameBytes || variableLen > directoryEnd-offset-zipCentralHeaderLength {
			return errors.New("ZIP central-directory member metadata is out of bounds")
		}
		name := make([]byte, nameLen)
		if err := readAtFull(r, name, int64(offset+zipCentralHeaderLength)); err != nil {
			return errors.New("ZIP central-directory member name is truncated")
		}
		if !utf8.Valid(name) {
			return errors.New("ZIP member name is not valid UTF-8")
		}
		nameString := string(name)
		directory := strings.HasSuffix(nameString, "/")
		cleanName, err := safeArchiveMemberName(nameString, directory)
		if err != nil {
			return err
		}
		pathBytes += int64(len(cleanName))
		if pathBytes > zipMaxPathBytes {
			return errors.New("archive member path data exceeds configured bounds")
		}
		key := strings.ToLower(cleanName)
		if _, ok := seen[key]; ok {
			return errors.New("duplicate or case-colliding archive member paths")
		}
		seen[key] = struct{}{}
		offset += zipCentralHeaderLength + variableLen
	}
	if offset != directoryEnd {
		return errors.New("ZIP central-directory record count or size does not match")
	}
	return nil
}

func safeArchiveMemberName(raw string, directory bool) (string, error) {
	if len(raw) == 0 || len(raw) > zipMaxNameBytes || !utf8.ValidString(raw) || strings.Contains(raw, "\\") || strings.HasPrefix(raw, "/") || strings.Contains(raw, ":") || hasControl(raw) {
		return "", errors.New("unsafe archive member path")
	}
	name := raw
	if directory {
		name = strings.TrimSuffix(name, "/")
	}
	if name == "" || path.Clean(name) != name || name == "." || strings.HasPrefix(name, "../") {
		return "", errors.New("unsafe archive member path")
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("unsafe archive member path")
		}
	}
	return name, nil
}

func readAtFull(r io.ReaderAt, b []byte, off int64) error {
	if off < 0 {
		return errors.New("negative ZIP offset")
	}
	n, err := r.ReadAt(b, off)
	if n == len(b) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return err
}
