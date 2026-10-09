// Package bridge inspects Google Photos Takeout archives and exports their
// original members with a portable relationship manifest.
package bridge

import (
	"errors"
	"fmt"
	"strings"
)

const SchemaVersion = 1

// ErrOutputLocked reports that another exporter currently holds the output lock.
// Other lock acquisition failures are returned as distinct errors.
var ErrOutputLocked = errors.New("output is locked by another exporter")

// Limits bounds the amount of untrusted archive data accepted by Inspect.
// Zero-valued fields are replaced with DefaultLimits values.
type Limits struct {
	MaxJSONBytes  int64 `json:"maxJSONBytes"`
	MaxEntries    int   `json:"maxEntries"`
	MaxArchives   int   `json:"maxArchives"`
	MaxMediaBytes int64 `json:"maxMediaBytes"`
	MaxTotalBytes int64 `json:"maxTotalBytes"`
}

// DefaultLimits returns conservative bounds for local Takeout archives.
func DefaultLimits() Limits {
	return Limits{
		MaxJSONBytes:  1 << 20,
		MaxEntries:    100_000,
		MaxArchives:   16,
		MaxMediaBytes: 32 << 30,
		MaxTotalBytes: 1 << 40,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxJSONBytes == 0 {
		l.MaxJSONBytes = d.MaxJSONBytes
	}
	if l.MaxEntries == 0 {
		l.MaxEntries = d.MaxEntries
	}
	if l.MaxArchives == 0 {
		l.MaxArchives = d.MaxArchives
	}
	if l.MaxMediaBytes == 0 {
		l.MaxMediaBytes = d.MaxMediaBytes
	}
	if l.MaxTotalBytes == 0 {
		l.MaxTotalBytes = d.MaxTotalBytes
	}
	return l
}

func (l Limits) validate() error {
	if l.MaxJSONBytes < 1 || l.MaxEntries < 1 || l.MaxArchives < 1 || l.MaxMediaBytes < 1 || l.MaxTotalBytes < 1 {
		return errors.New("limits must be positive")
	}
	d := DefaultLimits()
	if l.MaxJSONBytes > d.MaxJSONBytes || l.MaxEntries > d.MaxEntries || l.MaxArchives > d.MaxArchives || l.MaxMediaBytes > d.MaxMediaBytes || l.MaxTotalBytes > d.MaxTotalBytes {
		return errors.New("limits may lower but not exceed built-in safety bounds")
	}
	return nil
}

// Plan is a deterministic inspection result. Source paths are private local
// state and are omitted from the portable Manifest written during export.
type Plan struct {
	SchemaVersion int               `json:"schemaVersion"`
	ID            string            `json:"id"`
	Sources       []Source          `json:"sources"`
	Files         []MediaOccurrence `json:"files"`
	Sidecars      []Sidecar         `json:"sidecars"`
	Albums        []Album           `json:"albums"`
	Issues        []Issue           `json:"issues"`
	Stats         Stats             `json:"stats"`
}

type Source struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Format string `json:"format"`
}

// PublicSource contains identity information without revealing the local path.
type PublicSource struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Format string `json:"format"`
}

type MediaOccurrence struct {
	ID             string   `json:"id"`
	SourceIndex    int      `json:"sourceIndex"`
	EntryPath      string   `json:"entryPath"`
	SHA256         string   `json:"sha256"`
	Bytes          int64    `json:"bytes"`
	OutputPath     string   `json:"outputPath"`
	MIME           string   `json:"mime"`
	MetadataStatus string   `json:"metadataStatus"`
	MetadataID     string   `json:"metadataId,omitempty"`
	Date           string   `json:"date,omitempty"`
	AlbumIDs       []string `json:"albumIds"`
}

type Sidecar struct {
	ID          string `json:"id"`
	SourceIndex int    `json:"sourceIndex"`
	EntryPath   string `json:"entryPath"`
	SHA256      string `json:"sha256"`
	Bytes       int64  `json:"bytes"`
	OutputPath  string `json:"outputPath"`
}

type Album struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Folder   string   `json:"folder"`
	MediaIDs []string `json:"mediaIds"`
}

type Issue struct {
	Code        string `json:"code"`
	SourceIndex int    `json:"sourceIndex"`
	EntryPath   string `json:"entryPath,omitempty"`
	Details     string `json:"details"`
}

type Stats struct {
	SourceCount  int   `json:"sourceCount"`
	MediaCount   int   `json:"mediaCount"`
	SidecarCount int   `json:"sidecarCount"`
	AlbumCount   int   `json:"albumCount"`
	IssueCount   int   `json:"issueCount"`
	MediaBytes   int64 `json:"mediaBytes"`
	SidecarBytes int64 `json:"sidecarBytes"`
}

// Manifest is portable: it includes source identities and occurrences but
// never stores host-specific source paths.
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	PlanID        string            `json:"planId"`
	Sources       []PublicSource    `json:"sources"`
	Files         []MediaOccurrence `json:"files"`
	Sidecars      []Sidecar         `json:"sidecars"`
	Albums        []Album           `json:"albums"`
	Issues        []Issue           `json:"issues"`
	Stats         Stats             `json:"stats"`
}

type ExportReport struct {
	PlanID       string  `json:"planId"`
	OutputPath   string  `json:"outputPath"`
	Status       string  `json:"status"`
	FilesWritten int     `json:"filesWritten"`
	FilesReused  int     `json:"filesReused"`
	BytesWritten int64   `json:"bytesWritten"`
	Issues       []Issue `json:"issues"`
}

type VerifyReport struct {
	Status          string  `json:"status"`
	PlanID          string  `json:"planId"`
	FilesChecked    int     `json:"filesChecked"`
	SidecarsChecked int     `json:"sidecarsChecked"`
	BytesChecked    int64   `json:"bytesChecked"`
	Issues          []Issue `json:"issues"`
}

// CompareReport describes a read-only check of the selected source archives
// against both an inspection plan and an exported archive. A matched result
// only covers these selected sources and the exported bytes represented by
// their manifest; it does not claim anything about an entire account.
type CompareReport struct {
	Status          string            `json:"status"`
	PlanID          string            `json:"planId"`
	ArchivePlanID   string            `json:"archivePlanId,omitempty"`
	SourcesChecked  int               `json:"sourcesChecked"`
	MediaChecked    int               `json:"mediaChecked"`
	SidecarsChecked int               `json:"sidecarsChecked"`
	AlbumsChecked   int               `json:"albumsChecked"`
	Mismatches      []CompareMismatch `json:"mismatches"`
}

// CompareMismatch is a sanitized, path-safe explanation of one failed check.
// EntryPath is archive-internal and never contains the source's host path.
type CompareMismatch struct {
	Code      string `json:"code"`
	EntryPath string `json:"entryPath,omitempty"`
	Details   string `json:"details"`
}

func (p *Plan) validateVersion() error {
	if p == nil {
		return errors.New("plan is nil")
	}
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported plan schema version %d", p.SchemaVersion)
	}
	return nil
}

func (m *Manifest) validateVersion() error {
	if m == nil {
		return errors.New("manifest is nil")
	}
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported manifest schema version %d", m.SchemaVersion)
	}
	return nil
}

func validHexSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func validID(s string) bool {
	return len(s) == 64 && validHexSHA(s) && !strings.ContainsAny(s, `/\\`)
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
