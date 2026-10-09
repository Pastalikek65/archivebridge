package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const maxPlanBytes = 256 << 20

func WritePlan(p *Plan, filename string) error {
	if err := validatePlan(p, true); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > maxPlanBytes {
		return errors.New("plan exceeds the maximum serialized size")
	}
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(b)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(filename)
		return writeErr
	}
	if closeErr != nil {
		_ = os.Remove(filename)
		return closeErr
	}
	return nil
}

func ReadPlan(filename string) (*Plan, error) {
	b, err := readJSONFile(filename, maxPlanBytes)
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := decodeStrict(b, &p); err != nil {
		return nil, fmt.Errorf("invalid plan: %w", err)
	}
	if err := validatePlan(&p, true); err != nil {
		return nil, fmt.Errorf("invalid plan: %w", err)
	}
	return &p, nil
}

func ReadManifest(archiveDir string) (*Manifest, error) {
	st, err := os.Lstat(archiveDir)
	if err != nil {
		return nil, err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return nil, errors.New("archive root must be a real directory")
	}
	filename := filepath.Join(archiveDir, manifestFilename)
	b, err := readJSONFile(filename, maxPlanBytes)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := decodeStrict(b, &m); err != nil {
		return nil, fmt.Errorf("invalid manifest: %w", err)
	}
	if err := validateManifest(&m); err != nil {
		return nil, fmt.Errorf("invalid manifest: %w", err)
	}
	return &m, nil
}

func readJSONFile(filename string, max int64) ([]byte, error) {
	st, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return nil, errors.New("JSON document must be a regular non-symlink file")
	}
	if st.Size() < 0 || st.Size() > max {
		return nil, errors.New("JSON document exceeds the maximum serialized size")
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("JSON document exceeds the maximum serialized size")
	}
	return b, nil
}

func decodeStrict(b []byte, dest any) error {
	if err := checkJSONNoDuplicateKeys(b); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(dest); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("trailing data after JSON document")
	}
	return nil
}

func checkJSONNoDuplicateKeys(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := scanJSONValue(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing data after JSON document")
	}
	return nil
}

func scanJSONValue(d *json.Decoder, depth int) error {
	if depth > 256 {
		return errors.New("JSON nesting exceeds configured bound")
	}
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			keyTok, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := keyTok.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if seen[key] {
				return errors.New("duplicate JSON object key")
			}
			seen[key] = true
			if err := scanJSONValue(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for d.More() {
			if err := scanJSONValue(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func validatePlan(p *Plan, requirePaths bool) error {
	if p == nil {
		return errors.New("plan is nil")
	}
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported plan schema version %d", p.SchemaVersion)
	}
	if !validID(p.ID) {
		return errors.New("invalid plan ID")
	}
	limits := DefaultLimits()
	if len(p.Sources) == 0 || len(p.Sources) > limits.MaxArchives || len(p.Files) > limits.MaxEntries || len(p.Sidecars) > limits.MaxEntries || len(p.Albums) > limits.MaxEntries || len(p.Issues) > limits.MaxEntries {
		return errors.New("plan exceeds bounded item counts")
	}
	seenSources := make(map[string]bool)
	seenSourceHashes := make(map[string]bool)
	for i, s := range p.Sources {
		if s.Name == "" || len(s.Name) > 255 || strings.ContainsAny(s.Name, "/\\:") || hasControl(s.Name) || s.Bytes < 0 || !validHexSHA(s.SHA256) || (s.Format != "zip" && s.Format != "tar.gz") {
			return fmt.Errorf("invalid source at index %d", i)
		}
		if requirePaths && (!filepath.IsAbs(s.Path) || s.Path == "") {
			return fmt.Errorf("source %d must have an absolute local path", i)
		}
		if !requirePaths && s.Path != "" {
			return fmt.Errorf("public source contains a local path at index %d", i)
		}
		key := s.Name + "\x00" + s.SHA256
		if seenSources[key] {
			return errors.New("duplicate source identity")
		}
		seenSources[key] = true
		if seenSourceHashes[s.SHA256] {
			return errors.New("duplicate source archive hash")
		}
		seenSourceHashes[s.SHA256] = true
	}
	mediaByID := make(map[string]MediaOccurrence, len(p.Files))
	mediaBySourcePath := make(map[string]string, len(p.Files))
	entryCategoryBySourcePath := make(map[string]string, len(p.Files)+len(p.Sidecars))
	var expandedPayload int64
	for _, f := range p.Files {
		if !validID(f.ID) || f.SourceIndex < 0 || f.SourceIndex >= len(p.Sources) || f.Bytes < 0 || !validHexSHA(f.SHA256) || f.SHA256 == "" || !safeEntryPath(f.EntryPath) {
			return errors.New("invalid media occurrence")
		}
		if f.ID != stableID(p.Sources[f.SourceIndex].SHA256, f.EntryPath, f.SHA256) {
			return errors.New("media occurrence ID does not match its source identity")
		}
		if f.OutputPath != "media/sha256/"+f.SHA256 {
			return errors.New("invalid generated media output path")
		}
		if f.Bytes > limits.MaxMediaBytes || f.Bytes > limits.MaxTotalBytes-expandedPayload {
			return errors.New("media byte counts exceed finite limits")
		}
		expandedPayload += f.Bytes
		if f.MIME == "" {
			return errors.New("media occurrence has no MIME type")
		}
		if len(f.MIME) > 128 || strings.ContainsAny(f.MIME, "\r\n \t") || !strings.Contains(f.MIME, "/") || hasControl(f.MIME) {
			return errors.New("invalid media MIME type")
		}
		switch f.MetadataStatus {
		case "none", "matched", "unmatched", "ambiguous", "malformed":
		default:
			return errors.New("invalid media metadata status")
		}
		if f.Date != "" {
			if _, err := time.Parse(time.RFC3339, f.Date); err != nil {
				return errors.New("media date is not RFC3339")
			}
			if f.MetadataStatus != "matched" {
				return errors.New("media date lacks a matched metadata sidecar")
			}
		}
		key := fmt.Sprintf("%d\x00%s", f.SourceIndex, f.EntryPath)
		if _, ok := mediaByID[f.ID]; ok {
			return errors.New("duplicate media occurrence ID")
		}
		mediaByID[f.ID] = f
		if _, ok := mediaBySourcePath[key]; ok {
			return errors.New("duplicate media source entry")
		}
		mediaBySourcePath[key] = f.ID
		entryCategoryBySourcePath[key] = "media"
	}
	sidecarByID := make(map[string]Sidecar, len(p.Sidecars))
	sidecarBySourcePath := make(map[string]bool, len(p.Sidecars))
	for _, s := range p.Sidecars {
		if !validID(s.ID) || s.SourceIndex < 0 || s.SourceIndex >= len(p.Sources) || s.Bytes < 0 || !validHexSHA(s.SHA256) || !safeEntryPath(s.EntryPath) {
			return errors.New("invalid sidecar")
		}
		if s.ID != stableID(p.Sources[s.SourceIndex].SHA256, s.EntryPath, s.SHA256) {
			return errors.New("sidecar ID does not match its source identity")
		}
		if s.OutputPath != "sidecars/sha256/"+s.SHA256+".json" {
			return errors.New("invalid generated sidecar output path")
		}
		if s.Bytes > limits.MaxTotalBytes-expandedPayload {
			return errors.New("sidecar byte counts exceed finite limits")
		}
		expandedPayload += s.Bytes
		key := fmt.Sprintf("%d\x00%s", s.SourceIndex, s.EntryPath)
		if sidecarBySourcePath[key] {
			return errors.New("duplicate sidecar source entry")
		}
		if entryCategoryBySourcePath[key] == "media" {
			return errors.New("archive source entry cannot be both media and sidecar")
		}
		sidecarBySourcePath[key] = true
		entryCategoryBySourcePath[key] = "sidecar"
		if _, ok := sidecarByID[s.ID]; ok {
			return errors.New("duplicate sidecar ID")
		}
		sidecarByID[s.ID] = s
	}
	for _, f := range p.Files {
		if f.MetadataID != "" {
			sc, ok := sidecarByID[f.MetadataID]
			if !ok || sc.SourceIndex != f.SourceIndex {
				return errors.New("media metadata ID does not refer to a same-source sidecar")
			}
		}
		if (f.MetadataStatus == "matched" || f.MetadataStatus == "malformed") && f.MetadataID == "" {
			return errors.New("media metadata status requires a sidecar reference")
		}
		if f.MetadataID != "" && f.MetadataStatus != "matched" && f.MetadataStatus != "malformed" {
			return errors.New("media metadata reference has an incompatible status")
		}
		seen := map[string]bool{}
		for _, id := range f.AlbumIDs {
			if !validID(id) || seen[id] {
				return errors.New("invalid or duplicate media album reference")
			}
			seen[id] = true
		}
	}
	albumByID := make(map[string]Album, len(p.Albums))
	for _, a := range p.Albums {
		if !validID(a.ID) || a.Title == "" || len(a.Title) > 255 || strings.ContainsAny(a.Title, "/\\:") || hasControl(a.Title) || a.Folder == "" || !safeAlbumFolder(a.Folder) {
			return errors.New("invalid album")
		}
		albumIDMatchesSource := false
		if len(a.MediaIDs) > 0 {
			first, ok := mediaByID[a.MediaIDs[0]]
			if ok && first.SourceIndex >= 0 && first.SourceIndex < len(p.Sources) {
				albumIDMatchesSource = a.ID == stableID(p.Sources[first.SourceIndex].SHA256, "album", a.Folder)
			}
		} else {
			for _, src := range p.Sources {
				if a.ID == stableID(src.SHA256, "album", a.Folder) {
					albumIDMatchesSource = true
					break
				}
			}
		}
		if !albumIDMatchesSource {
			return errors.New("album ID does not match a source identity")
		}
		if _, ok := albumByID[a.ID]; ok {
			return errors.New("duplicate album ID")
		}
		albumByID[a.ID] = a
		seen := map[string]bool{}
		for _, mid := range a.MediaIDs {
			f, ok := mediaByID[mid]
			if !ok || seen[mid] {
				return errors.New("invalid album media reference")
			}
			seen[mid] = true
			if a.ID != stableID(p.Sources[f.SourceIndex].SHA256, "album", a.Folder) {
				return errors.New("album membership crosses source identity")
			}
			present := false
			for _, aid := range f.AlbumIDs {
				if aid == a.ID {
					present = true
					break
				}
			}
			if !present {
				return errors.New("album and media relationship is inconsistent")
			}
		}
	}
	for _, f := range p.Files {
		for _, id := range f.AlbumIDs {
			a, ok := albumByID[id]
			if !ok {
				return errors.New("media references an unknown album")
			}
			found := false
			for _, mid := range a.MediaIDs {
				if mid == f.ID {
					found = true
					break
				}
			}
			if !found {
				return errors.New("media and album relationship is inconsistent")
			}
		}
	}
	for _, issue := range p.Issues {
		if issue.SourceIndex < 0 || issue.SourceIndex >= len(p.Sources) || !validIssueCode(issue.Code) || issue.Details == "" || len(issue.Details) > 4096 || strings.ContainsAny(issue.Details, "/\\:") || hasControl(issue.Details) {
			return errors.New("invalid issue")
		}
		if issue.EntryPath != "" && !safeEntryPath(issue.EntryPath) {
			return errors.New("unsafe issue entry path")
		}
		local := filepath.ToSlash(p.Sources[issue.SourceIndex].Path)
		if local != "" && strings.Contains(strings.ToLower(issue.Details), strings.ToLower(local)) {
			return errors.New("issue details disclose a local source path")
		}
	}
	if err := validateStats(p.Stats, p.Sources, p.Files, p.Sidecars, p.Albums, p.Issues); err != nil {
		return err
	}
	computed, err := calculatePlanID(p)
	if err != nil {
		return err
	}
	if computed != p.ID {
		return errors.New("plan ID does not match plan contents")
	}
	return nil
}

func validateManifest(m *Manifest) error {
	if m == nil {
		return errors.New("manifest is nil")
	}
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported manifest schema version %d", m.SchemaVersion)
	}
	p := &Plan{SchemaVersion: m.SchemaVersion, ID: m.PlanID, Files: m.Files, Sidecars: m.Sidecars, Albums: m.Albums, Issues: m.Issues, Stats: m.Stats}
	p.Sources = make([]Source, len(m.Sources))
	for i, s := range m.Sources {
		p.Sources[i] = Source{Name: s.Name, SHA256: s.SHA256, Bytes: s.Bytes, Format: s.Format}
	}
	return validatePlan(p, false)
}

func validateStats(stats Stats, sources []Source, files []MediaOccurrence, sidecars []Sidecar, albums []Album, issues []Issue) error {
	var mb, sb int64
	for _, f := range files {
		if f.Bytes > DefaultLimits().MaxTotalBytes-mb {
			return errors.New("media byte count exceeds finite bound")
		}
		mb += f.Bytes
	}
	for _, s := range sidecars {
		if s.Bytes > DefaultLimits().MaxTotalBytes-sb {
			return errors.New("sidecar byte count exceeds finite bound")
		}
		sb += s.Bytes
	}
	if stats.SourceCount != len(sources) || stats.MediaCount != len(files) || stats.SidecarCount != len(sidecars) || stats.AlbumCount != len(albums) || stats.IssueCount != len(issues) || stats.MediaBytes != mb || stats.SidecarBytes != sb {
		return errors.New("plan statistics do not match contents")
	}
	return nil
}

func safeEntryPath(s string) bool {
	if s == "" || len(s) > 1024 || hasControl(s) || strings.ContainsAny(s, "\\:") || strings.HasPrefix(s, "/") || path.Clean(s) != s {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validIssueCode(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}
func safeAlbumFolder(s string) bool {
	if strings.HasPrefix(s, "@album/") {
		return len(s) > 7 && safeEntryPath(strings.TrimPrefix(s, "@"))
	}
	return safeEntryPath(s)
}
