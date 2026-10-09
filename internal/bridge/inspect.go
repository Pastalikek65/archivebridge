package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type scannedMember struct {
	path         string
	source       int
	hash         string
	bytes        int64
	media        bool
	sidecar      bool
	metadata     parsedMetadata
	oversizeJSON bool
}

type parsedMetadata struct {
	valid      bool
	malformed  bool
	title      string
	date       string
	albumTitle string
	dateBad    bool
}

type archiveSource struct {
	path, name, format, sha string
	size                    int64
}

// Inspect scans selected Google Photos Takeout ZIP and TAR.GZ parts without
// extracting their member names onto the filesystem.
func Inspect(ctx context.Context, archives []string, limits Limits) (*Plan, error) {
	limits = limits.withDefaults()
	if err := limits.validate(); err != nil {
		return nil, err
	}
	if len(archives) == 0 {
		return nil, errors.New("at least one source archive is required")
	}
	if len(archives) > limits.MaxArchives {
		return nil, errors.New("source archive limit exceeded")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	inputs := make([]archiveSource, 0, len(archives))
	seenPaths := map[string]bool{}
	for _, supplied := range archives {
		abs, err := filepath.Abs(supplied)
		if err != nil {
			return nil, fmt.Errorf("invalid source path: %w", err)
		}
		abs = filepath.Clean(abs)
		key := strings.ToLower(abs)
		if seenPaths[key] {
			return nil, errors.New("duplicate source archive selection")
		}
		seenPaths[key] = true
		st, err := os.Lstat(abs)
		if err != nil {
			return nil, fmt.Errorf("cannot inspect source archive: %w", err)
		}
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return nil, errors.New("source must be a regular non-symlink file")
		}
		format, err := detectFormat(abs)
		if err != nil {
			return nil, err
		}
		sha, size, err := hashFile(ctx, abs)
		if err != nil {
			return nil, fmt.Errorf("cannot hash source archive: %w", err)
		}
		inputs = append(inputs, archiveSource{path: abs, name: filepath.Base(abs), format: format, sha: sha, size: size})
	}
	sort.Slice(inputs, func(i, j int) bool {
		if inputs[i].name != inputs[j].name {
			return inputs[i].name < inputs[j].name
		}
		if inputs[i].sha != inputs[j].sha {
			return inputs[i].sha < inputs[j].sha
		}
		return inputs[i].path < inputs[j].path
	})
	for i := 1; i < len(inputs); i++ {
		if inputs[i].sha == inputs[i-1].sha {
			return nil, errors.New("duplicate identical source archive selection")
		}
	}
	seenSourceHashes := make(map[string]bool, len(inputs))
	for _, src := range inputs {
		if seenSourceHashes[src.sha] {
			return nil, errors.New("duplicate identical source archive selection")
		}
		seenSourceHashes[src.sha] = true
	}

	plan := &Plan{SchemaVersion: SchemaVersion, Sources: make([]Source, len(inputs)), Files: []MediaOccurrence{}, Sidecars: []Sidecar{}, Albums: []Album{}, Issues: []Issue{}}
	var totalBytes int64
	var tarExpandedBytes int64
	var totalEntries int
	allMembers := make([]scannedMember, 0)
	for si, src := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		plan.Sources[si] = Source{Name: src.name, Path: src.path, SHA256: src.sha, Bytes: src.size, Format: src.format}
		err := walkArchive(ctx, src.path, src.format, limits, &totalBytes, &tarExpandedBytes, &totalEntries, func(entry *archiveEntry) error {
			if entry.Directory {
				return nil
			}
			ext := strings.ToLower(path.Ext(entry.Path))
			media := supportedMediaExt[ext]
			jsonSidecar := ext == ".json"
			if media && entry.SizeHint > limits.MaxMediaBytes {
				return errors.New("media member exceeds configured byte limit")
			}
			var h hash.Hash = sha256.New()
			member := scannedMember{path: entry.Path, source: si, media: media, sidecar: jsonSidecar}
			if jsonSidecar {
				captureLimit := int(limits.MaxJSONBytes + 1)
				// Keep only the bounded prefix needed for parsing; hash the complete original.
				prefix := make([]byte, captureLimit)
				n, err := io.ReadFull(io.TeeReader(entry.Reader, h), prefix)
				if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
					return err
				}
				prefix = prefix[:n]
				if n > int(limits.MaxJSONBytes) {
					member.oversizeJSON = true
					if _, err := io.Copy(h, entry.Reader); err != nil {
						return err
					}
				} else {
					if _, err := io.Copy(h, entry.Reader); err != nil {
						return err
					}
					member.metadata = parseMetadata(prefix)
				}
			} else if media {
				n, err := io.Copy(h, io.LimitReader(entry.Reader, limits.MaxMediaBytes+1))
				if err != nil {
					return err
				}
				if n > limits.MaxMediaBytes {
					return errors.New("media member exceeds configured byte limit")
				}
			} else {
				if _, err := io.Copy(h, entry.Reader); err != nil {
					return err
				}
			}
			member.bytes = entry.BytesRead
			member.hash = hex.EncodeToString(h.Sum(nil))
			if media || jsonSidecar {
				allMembers = append(allMembers, member)
			}
			if !media && !jsonSidecar {
				plan.Issues = append(plan.Issues, Issue{Code: "unsupported_member", SourceIndex: si, EntryPath: entry.Path, Details: "Archive member is not a supported photo, video, or JSON sidecar."})
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("cannot inspect source archive %q: %w", src.name, err)
		}
		// The plan identities are invalid if an archive changed while being scanned.
		st, statErr := os.Lstat(src.path)
		if statErr != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return nil, errors.New("source archive changed type during inspection")
		}
		sha, size, err := hashFile(ctx, src.path)
		if err != nil {
			return nil, fmt.Errorf("cannot recheck source archive: %w", err)
		}
		if sha != src.sha || size != src.size {
			return nil, errors.New("source archive changed during inspection")
		}
	}

	mediaByEntry := make(map[int]map[string]int)
	for _, m := range allMembers {
		if !m.media {
			continue
		}
		id := stableID(inputs[m.source].sha, m.path, m.hash)
		occ := MediaOccurrence{ID: id, SourceIndex: m.source, EntryPath: m.path, SHA256: m.hash, Bytes: m.bytes, OutputPath: "media/sha256/" + m.hash, MIME: mimeFor(m.path), MetadataStatus: "none", AlbumIDs: []string{}}
		plan.Files = append(plan.Files, occ)
		if mediaByEntry[m.source] == nil {
			mediaByEntry[m.source] = make(map[string]int)
		}
		mediaByEntry[m.source][m.path] = len(plan.Files) - 1
	}
	for _, m := range allMembers {
		if !m.sidecar {
			continue
		}
		id := stableID(inputs[m.source].sha, m.path, m.hash)
		plan.Sidecars = append(plan.Sidecars, Sidecar{ID: id, SourceIndex: m.source, EntryPath: m.path, SHA256: m.hash, Bytes: m.bytes, OutputPath: "sidecars/sha256/" + m.hash + ".json"})
		if m.oversizeJSON {
			plan.Issues = append(plan.Issues, Issue{Code: "metadata_too_large", SourceIndex: m.source, EntryPath: m.path, Details: "JSON sidecar exceeds the configured metadata parsing limit and was preserved without interpretation."})
		} else if m.metadata.malformed {
			plan.Issues = append(plan.Issues, Issue{Code: "malformed_metadata", SourceIndex: m.source, EntryPath: m.path, Details: "JSON sidecar is malformed and was preserved without interpretation."})
		}
	}
	applyMetadata(plan, allMembers, inputs, mediaByEntry)
	buildAlbums(plan, allMembers, inputs)
	plan.Issues = dedupeIssues(plan.Issues)
	// Stable ordering makes serialization and Plan.ID reproducible.
	sort.Slice(plan.Files, func(i, j int) bool {
		if plan.Files[i].SourceIndex != plan.Files[j].SourceIndex {
			return plan.Files[i].SourceIndex < plan.Files[j].SourceIndex
		}
		return plan.Files[i].EntryPath < plan.Files[j].EntryPath
	})
	sort.Slice(plan.Sidecars, func(i, j int) bool {
		if plan.Sidecars[i].SourceIndex != plan.Sidecars[j].SourceIndex {
			return plan.Sidecars[i].SourceIndex < plan.Sidecars[j].SourceIndex
		}
		return plan.Sidecars[i].EntryPath < plan.Sidecars[j].EntryPath
	})
	for i := range plan.Files {
		sort.Strings(plan.Files[i].AlbumIDs)
	}
	sort.Slice(plan.Albums, func(i, j int) bool {
		if plan.Albums[i].Folder != plan.Albums[j].Folder {
			return plan.Albums[i].Folder < plan.Albums[j].Folder
		}
		return plan.Albums[i].ID < plan.Albums[j].ID
	})
	for i := range plan.Albums {
		sort.Strings(plan.Albums[i].MediaIDs)
	}
	sort.Slice(plan.Issues, func(i, j int) bool {
		a, b := plan.Issues[i], plan.Issues[j]
		if a.SourceIndex != b.SourceIndex {
			return a.SourceIndex < b.SourceIndex
		}
		if a.EntryPath != b.EntryPath {
			return a.EntryPath < b.EntryPath
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Details < b.Details
	})
	plan.Stats = Stats{SourceCount: len(plan.Sources), MediaCount: len(plan.Files), SidecarCount: len(plan.Sidecars), AlbumCount: len(plan.Albums), IssueCount: len(plan.Issues)}
	for _, f := range plan.Files {
		plan.Stats.MediaBytes += f.Bytes
	}
	for _, s := range plan.Sidecars {
		plan.Stats.SidecarBytes += s.Bytes
	}
	id, err := calculatePlanID(plan)
	if err != nil {
		return nil, err
	}
	plan.ID = id
	return plan, nil
}

var supportedMediaExt = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".heic": true, ".heif": true, ".tif": true, ".tiff": true, ".bmp": true, ".avif": true, ".jp2": true, ".jxl": true, ".dng": true, ".cr2": true, ".nef": true, ".arw": true, ".orf": true, ".rw2": true, ".raf": true, ".3fr": true, ".fff": true,
	".mp4": true, ".mov": true, ".m4v": true, ".avi": true, ".mkv": true, ".webm": true, ".3gp": true, ".3g2": true, ".mts": true, ".m2ts": true, ".mpg": true, ".mpeg": true, ".wmv": true, ".vob": true, ".ogv": true,
}

func mimeFor(entry string) string {
	return stableMIMEs[strings.ToLower(path.Ext(entry))]
}

var stableMIMEs = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif", ".webp": "image/webp", ".heic": "image/heic", ".heif": "image/heif", ".tif": "image/tiff", ".tiff": "image/tiff", ".bmp": "image/bmp", ".avif": "image/avif", ".jp2": "image/jp2", ".jxl": "image/jxl",
	".dng": "image/x-adobe-dng", ".cr2": "image/x-canon-cr2", ".nef": "image/x-nikon-nef", ".arw": "image/x-sony-arw", ".orf": "image/x-olympus-orf", ".rw2": "image/x-panasonic-rw2", ".raf": "image/x-fuji-raf", ".3fr": "image/x-hasselblad-3fr", ".fff": "image/x-hasselblad-fff",
	".mp4": "video/mp4", ".m4v": "video/mp4", ".mov": "video/quicktime", ".avi": "video/x-msvideo", ".mkv": "video/x-matroska", ".webm": "video/webm", ".3gp": "video/3gpp", ".3g2": "video/3gpp2", ".mts": "video/mp2t", ".m2ts": "video/mp2t", ".mpg": "video/mpeg", ".mpeg": "video/mpeg", ".wmv": "video/x-ms-wmv", ".vob": "video/dvd", ".ogv": "video/ogg",
}

func hashFile(ctx context.Context, filename string) (string, int64, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, &contextReader{ctx: ctx, r: f})
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func stableID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = io.WriteString(h, p)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func parseMetadata(raw []byte) parsedMetadata {
	if !utf8.Valid(raw) {
		return parsedMetadata{malformed: true}
	}
	if err := checkJSONNoDuplicateKeys(raw); err != nil {
		return parsedMetadata{malformed: true}
	}
	var v map[string]any
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	if err := d.Decode(&v); err != nil || v == nil {
		return parsedMetadata{malformed: true}
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return parsedMetadata{malformed: true}
	}
	m := parsedMetadata{valid: true}
	if s, ok := v["title"].(string); ok {
		m.title = s
	}
	if s, ok := v["albumName"].(string); ok {
		m.albumTitle = s
	}
	if m.albumTitle == "" {
		if s, ok := v["name"].(string); ok && strings.Contains(strings.ToLower(path.Base(s)), "album") {
			m.albumTitle = s
		}
	}
	for _, key := range []string{"photoTakenTime", "creationTime"} {
		obj, ok := v[key].(map[string]any)
		if !ok {
			continue
		}
		stamp, ok := obj["timestamp"]
		if !ok {
			continue
		}
		date, bad := timestampDate(stamp)
		if date != "" {
			m.date = date
			break
		}
		if bad {
			m.dateBad = true
			break
		}
	}
	return m
}

func timestampDate(v any) (string, bool) {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case json.Number:
		s = x.String()
	default:
		return "", false
	}
	sec, err := strconv.ParseInt(s, 10, 64)
	if err != nil || sec < 0 {
		return "", true
	}
	t := time.Unix(sec, 0).UTC()
	if t.Year() < 1 || t.Year() > 9999 {
		return "", true
	}
	return t.Format(time.RFC3339), false
}

func calculatePlanID(p *Plan) (string, error) {
	clone := *p
	clone.ID = ""
	clone.Sources = append([]Source(nil), p.Sources...)
	for i := range clone.Sources {
		clone.Sources[i].Path = ""
	}
	b, err := json.Marshal(clone)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

func applyMetadata(plan *Plan, members []scannedMember, sources []archiveSource, mediaByEntry map[int]map[string]int) {
	type association struct {
		sidecarID  string
		mediaIndex int
		malformed  bool
		date       string
		dateBad    bool
	}
	associations := make([]association, 0)
	ambiguousMedia := make(map[int]bool)
	sidecarIDs := make(map[string]string)
	for _, sc := range plan.Sidecars {
		sidecarIDs[fmt.Sprintf("%d\x00%s", sc.SourceIndex, sc.EntryPath)] = sc.ID
	}
	for _, m := range members {
		if !m.sidecar {
			continue
		}
		id := sidecarIDs[fmt.Sprintf("%d\x00%s", m.source, m.path)]
		folder := path.Dir(m.path)
		candidates := make(map[int]bool)
		if strings.HasSuffix(strings.ToLower(m.path), ".json") {
			name := strings.TrimSuffix(m.path, path.Ext(m.path))
			if idx, ok := mediaByEntry[m.source][name]; ok {
				candidates[idx] = true
			}
		}
		if m.metadata.valid && m.metadata.title != "" && !strings.ContainsAny(m.metadata.title, "/\\") && path.Base(m.metadata.title) == m.metadata.title {
			target := m.metadata.title
			if folder != "." {
				target = path.Join(folder, target)
			}
			if idx, ok := mediaByEntry[m.source][target]; ok {
				candidates[idx] = true
			}
		}
		if len(candidates) > 1 {
			plan.Issues = append(plan.Issues, Issue{Code: "ambiguous_metadata", SourceIndex: m.source, EntryPath: m.path, Details: "JSON sidecar identifies more than one media occurrence and was not attached."})
			for idx := range candidates {
				ambiguousMedia[idx] = true
			}
			continue
		}
		if m.metadata.albumTitle != "" && len(candidates) == 0 {
			continue
		}
		if len(candidates) == 0 {
			if !m.metadata.malformed && !m.oversizeJSON {
				plan.Issues = append(plan.Issues, Issue{Code: "unmatched_metadata", SourceIndex: m.source, EntryPath: m.path, Details: "JSON sidecar has no exact same-folder media match and was preserved unattached."})
			}
			continue
		}
		for idx := range candidates {
			associations = append(associations, association{sidecarID: id, mediaIndex: idx, malformed: m.metadata.malformed || m.oversizeJSON, date: m.metadata.date, dateBad: m.metadata.dateBad})
		}
	}
	byMedia := make(map[int][]association)
	for _, a := range associations {
		byMedia[a.mediaIndex] = append(byMedia[a.mediaIndex], a)
	}
	for idx := range ambiguousMedia {
		if len(byMedia[idx]) == 0 {
			plan.Files[idx].MetadataStatus = "ambiguous"
		}
	}
	for idx, items := range byMedia {
		if len(items) > 1 || ambiguousMedia[idx] {
			plan.Files[idx].MetadataStatus = "ambiguous"
			for range items {
				plan.Issues = append(plan.Issues, Issue{Code: "ambiguous_metadata", SourceIndex: plan.Files[idx].SourceIndex, EntryPath: plan.Files[idx].EntryPath, Details: "More than one JSON sidecar matches this media occurrence; no metadata was attached."})
			}
			continue
		}
		a := items[0]
		plan.Files[idx].MetadataID = a.sidecarID
		if a.malformed {
			plan.Files[idx].MetadataStatus = "malformed"
			continue
		}
		plan.Files[idx].MetadataStatus = "matched"
		plan.Files[idx].Date = a.date
		if a.dateBad {
			plan.Issues = append(plan.Issues, Issue{Code: "malformed_metadata_date", SourceIndex: plan.Files[idx].SourceIndex, EntryPath: plan.Files[idx].EntryPath, Details: "Matched JSON sidecar contains an invalid timestamp; no date was recorded."})
		}
	}
}

func dedupeIssues(issues []Issue) []Issue {
	seen := make(map[string]bool, len(issues))
	out := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		key := fmt.Sprintf("%d\x00%s\x00%s", issue.SourceIndex, issue.EntryPath, issue.Code)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, issue)
	}
	return out
}

func buildAlbums(plan *Plan, members []scannedMember, sources []archiveSource) {
	byID := make(map[string]*Album)
	ensure := func(source int, folder, title string) *Album {
		id := stableID(sources[source].sha, "album", folder)
		a := byID[id]
		if a == nil {
			a = &Album{ID: id, Title: title, Folder: folder, MediaIDs: []string{}}
			byID[id] = a
		}
		if title != "" {
			a.Title = title
		}
		return a
	}
	for _, f := range plan.Files {
		folder := path.Dir(f.EntryPath)
		if folder == "." {
			continue
		}
		parts := strings.Split(folder, "/")
		current := ""
		for _, part := range parts {
			if current == "" {
				current = part
			} else {
				current = path.Join(current, part)
			}
			if ignoreAlbumFolder(part) {
				continue
			}
			a := ensure(f.SourceIndex, current, part)
			a.MediaIDs = append(a.MediaIDs, f.ID)
		}
	}
	for _, m := range members {
		if !m.sidecar || !m.metadata.valid || m.metadata.albumTitle == "" {
			continue
		}
		if strings.ContainsAny(m.metadata.albumTitle, "/\\:") || len(m.metadata.albumTitle) > 255 {
			plan.Issues = append(plan.Issues, Issue{Code: "invalid_album_title", SourceIndex: m.source, EntryPath: m.path, Details: "Album descriptor title is unsafe and was preserved without interpretation."})
			continue
		}
		folder := path.Dir(m.path)
		if folder == "." {
			folder = "@album/" + m.metadata.albumTitle
		}
		ensure(m.source, folder, m.metadata.albumTitle)
	}
	for _, a := range byID {
		plan.Albums = append(plan.Albums, *a)
	}
	for i := range plan.Files {
		for _, a := range plan.Albums {
			for _, mid := range a.MediaIDs {
				if mid == plan.Files[i].ID {
					plan.Files[i].AlbumIDs = append(plan.Files[i].AlbumIDs, a.ID)
				}
			}
		}
	}
}

func ignoreAlbumFolder(folder string) bool {
	lower := strings.ToLower(strings.TrimSpace(folder))
	switch lower {
	case "takeout", "google photos", "photos", "albums", "photos from YYYY":
		return true
	}
	if len(folder) == 16 && strings.EqualFold(folder[:12], "Photos from ") {
		for _, r := range folder[12:] {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}
	return false
}
