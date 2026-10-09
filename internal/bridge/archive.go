package bridge

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

type archiveEntry struct {
	Path      string
	Directory bool
	SizeHint  int64
	Reader    io.Reader
	BytesRead int64
}

type memberCounterReader struct {
	ctx      context.Context
	source   io.Reader
	read     *int64
	total    *int64
	maxTotal int64
}

func (r *memberCounterReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
	}
	if r.total != nil {
		remaining := r.maxTotal - *r.total
		if remaining <= 0 {
			var probe [1]byte
			n, err := r.source.Read(probe[:])
			if n != 0 {
				return 0, errors.New("total expanded byte limit exceeded")
			}
			return 0, err
		}
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
	}
	n, err := r.source.Read(p)
	if r.read != nil {
		*r.read += int64(n)
	}
	if r.total != nil {
		*r.total += int64(n)
	}
	return n, err
}

type archiveCallback func(*archiveEntry) error

// walkArchive streams every regular member and verifies archive-level checksums
// by consuming each stream to EOF. The shared counters bound all selected parts.
func walkArchive(ctx context.Context, filename, format string, limits Limits, total, tarExpandedTotal *int64, entryCount *int, visit archiveCallback) error {
	seen := make(map[string]string)
	var totalPathBytes int64
	acceptName := func(raw string, directory bool) (string, error) {
		if len(raw) == 0 || len(raw) > 1024 || !utf8.ValidString(raw) || strings.Contains(raw, "\\") || strings.HasPrefix(raw, "/") || strings.Contains(raw, ":") || hasControl(raw) {
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
		key := strings.ToLower(name)
		if old, ok := seen[key]; ok {
			if old == name {
				return "", errors.New("duplicate archive member path")
			}
			return "", errors.New("case-colliding archive member paths")
		}
		seen[key] = name
		totalPathBytes += int64(len(name))
		if totalPathBytes > 16<<20 {
			return "", errors.New("archive member path data exceeds configured bounds")
		}
		*entryCount++
		if *entryCount > limits.MaxEntries {
			return "", errors.New("archive entry limit exceeded")
		}
		return name, nil
	}
	checkContext := func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	openEntry := func(r io.Reader, closeFn func() error, name string, size int64, mode int) error {
		entry := &archiveEntry{Path: name, SizeHint: size}
		if mode == 1 {
			entry.Reader = &boundedReader{ctx: ctx, source: r, total: total, read: &entry.BytesRead, maxTotal: limits.MaxTotalBytes}
		} else if mode == 2 {
			entry.Reader = &memberCounterReader{ctx: ctx, source: r, read: &entry.BytesRead, total: total, maxTotal: limits.MaxTotalBytes}
		} else {
			entry.Reader = &memberCounterReader{ctx: ctx, source: r, read: &entry.BytesRead}
		}
		visitErr := visit(entry)
		if visitErr == nil {
			_, visitErr = io.Copy(io.Discard, entry.Reader)
		}
		closeErr := closeFn()
		if visitErr != nil {
			return visitErr
		}
		if closeErr != nil {
			return fmt.Errorf("archive member integrity check failed: %w", closeErr)
		}
		return nil
	}

	if err := checkContext(); err != nil {
		return err
	}
	switch format {
	case "zip":
		f, err := os.Open(filename)
		if err != nil {
			return fmt.Errorf("cannot open source archive: %w", err)
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if err := preflightZIP(f, st.Size(), limits, *entryCount); err != nil {
			return fmt.Errorf("invalid or over-limit ZIP archive: %w", err)
		}
		zr, err := zip.NewReader(f, st.Size())
		if err != nil {
			return fmt.Errorf("invalid ZIP archive: %w", err)
		}
		for _, zf := range zr.File {
			if err := checkContext(); err != nil {
				return err
			}
			dir := zf.FileInfo().IsDir()
			name, err := acceptName(zf.Name, dir)
			if err != nil {
				return err
			}
			mode := zf.Mode()
			if dir {
				if !strings.HasSuffix(zf.Name, "/") {
					return errors.New("invalid ZIP directory entry")
				}
				if zf.UncompressedSize64 != 0 {
					return errors.New("ZIP directory entry has a payload")
				}
				continue
			}
			if !mode.IsRegular() {
				return errors.New("ZIP contains a non-regular member")
			}
			if zf.UncompressedSize64 > uint64(limits.MaxTotalBytes) || zf.UncompressedSize64 > uint64(^uint64(0)>>1) {
				return errors.New("archive member exceeds configured byte limit")
			}
			r, err := zf.Open()
			if err != nil {
				return fmt.Errorf("cannot open ZIP member: %w", err)
			}
			if err := openEntry(r, r.Close, name, int64(zf.UncompressedSize64), 1); err != nil {
				return err
			}
		}
		return nil
	case "tar.gz":
		f, err := os.Open(filename)
		if err != nil {
			return fmt.Errorf("cannot open source archive: %w", err)
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("invalid gzip archive: %w", err)
		}
		// Bound TAR framing/trailing bytes separately from member payload bytes.
		var tarExpandedMax int64 = limits.MaxTotalBytes + (16 << 20) + int64(limits.MaxEntries)*2048
		expanded := &boundedReader{ctx: ctx, source: gz, total: tarExpandedTotal, maxTotal: tarExpandedMax}
		tr := tar.NewReader(expanded)
		for {
			if err := checkContext(); err != nil {
				_ = gz.Close()
				return err
			}
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				_ = gz.Close()
				return fmt.Errorf("invalid TAR.GZ archive: %w", err)
			}
			dir := h.Typeflag == tar.TypeDir
			name, err := acceptName(h.Name, dir)
			if err != nil {
				_ = gz.Close()
				return err
			}
			if dir {
				if h.Size != 0 {
					_ = gz.Close()
					return errors.New("TAR.GZ directory entry has a payload")
				}
				continue
			}
			if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
				_ = gz.Close()
				return errors.New("TAR.GZ contains a non-regular member")
			}
			if h.Size < 0 || h.Size > limits.MaxTotalBytes {
				_ = gz.Close()
				return errors.New("archive member exceeds configured byte limit")
			}
			if err := openEntry(tr, func() error { return nil }, name, h.Size, 2); err != nil {
				_ = gz.Close()
				return err
			}
		}
		// archive/tar stops at the end marker; drain gzip to force CRC and size checks.
		if _, err := io.Copy(io.Discard, expanded); err != nil {
			_ = gz.Close()
			return fmt.Errorf("gzip integrity check failed: %w", err)
		}
		if err := gz.Close(); err != nil {
			return fmt.Errorf("gzip integrity check failed: %w", err)
		}
		return nil
	default:
		return errors.New("unsupported source archive format")
	}
}

type boundedReader struct {
	ctx      context.Context
	source   io.Reader
	total    *int64
	read     *int64
	maxTotal int64
}

func (r *boundedReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
	}
	remaining := r.maxTotal - *r.total
	if remaining <= 0 {
		var probe [1]byte
		n, err := r.source.Read(probe[:])
		if n != 0 {
			return 0, errors.New("total expanded byte limit exceeded")
		}
		return 0, err
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.source.Read(p)
	if r.read != nil {
		*r.read += int64(n)
	}
	*r.total += int64(n)
	return n, err
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(p)
	}
}

func detectFormat(filename string) (string, error) {
	name := strings.ToLower(filepath.Base(filename))
	if strings.HasSuffix(name, ".zip") {
		return "zip", nil
	}
	if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") {
		return "tar.gz", nil
	}
	return "", fmt.Errorf("unsupported archive extension for %q", filepath.Base(filename))
}
