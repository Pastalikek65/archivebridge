package immich

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type localContentIdentity struct {
	sha256 string
	sha1   string
	bytes  int64
}

func hashPortableMedia(ctx context.Context, archiveRoot, outputPath, expectedSHA string, expectedBytes int64) (localContentIdentity, error) {
	f, err := openPortableMedia(archiveRoot, outputPath, expectedBytes)
	if err != nil {
		return localContentIdentity{}, err
	}
	defer f.Close()
	full := sha256.New()
	hint := sha1.New()
	limited := &contextReader{ctx: ctx, r: io.LimitReader(f, bridgeMaxMediaBytes+1)}
	n, err := io.Copy(io.MultiWriter(full, hint), limited)
	if err != nil {
		return localContentIdentity{}, err
	}
	fullHex := hex.EncodeToString(full.Sum(nil))
	if n != expectedBytes || n > bridgeMaxMediaBytes || fullHex != expectedSHA {
		return localContentIdentity{}, errors.New("portable media changed or does not match its manifest")
	}
	return localContentIdentity{sha256: fullHex, sha1: base64.StdEncoding.EncodeToString(hint.Sum(nil)), bytes: n}, nil
}

const bridgeMaxMediaBytes = int64(32 << 30)

func openPortableMedia(archiveRoot, outputPath string, expectedBytes int64) (*os.File, error) {
	if expectedBytes < 0 || expectedBytes > bridgeMaxMediaBytes || outputPath == "" || strings.Contains(outputPath, "\\") || strings.HasPrefix(outputPath, "/") || path.Clean(outputPath) != outputPath {
		return nil, errors.New("manifest media path or size is unsafe")
	}
	for _, segment := range strings.Split(outputPath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return nil, errors.New("manifest media path is unsafe")
		}
	}
	root, err := filepath.Abs(archiveRoot)
	if err != nil {
		return nil, err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, errors.New("portable archive root is not a real directory")
	}
	full := filepath.Join(root, filepath.FromSlash(outputPath))
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, errors.New("manifest media path escaped the archive root")
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	current := root
	for _, segment := range parts[:len(parts)-1] {
		current = filepath.Join(current, segment)
		st, statErr := os.Lstat(current)
		if statErr != nil || st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return nil, errors.New("manifest media parent is not a real directory")
		}
	}
	before, err := os.Lstat(full)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Size() != expectedBytes {
		return nil, errors.New("manifest media is missing or not a regular file of the expected size")
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() != expectedBytes || !os.SameFile(before, opened) {
		_ = f.Close()
		return nil, errors.New("manifest media changed while opening")
	}
	return f, nil
}

func hashToString(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }
