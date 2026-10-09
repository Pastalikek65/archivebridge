// Package immich contains the opt-in, local-to-Immich transfer engine.
//
// It is deliberately an HTTP client only. It does not start or configure an
// Immich server, and reports describe only the selected portable archive.
package immich

import (
	"errors"
	"time"
)

const (
	ReportSchemaVersion    = 1
	SupportedServerVersion = "3.3.1"
	dateTransferPolicy     = "takeout-date-authoritative-generated-xmp-v1"
)

// RequiredPermissions is the complete least-privilege permission set checked
// before any remote mutation. Callers receive a copy so they cannot change the
// engine's preflight policy.
func RequiredPermissions() []string {
	return []string{
		"asset.upload",
		"asset.read",
		"asset.download",
		"album.read",
		"album.create",
		"albumAsset.create",
		"user.read",
	}
}

// Options configure a single explicit server operation. APIKey, Resume, and
// Progress are runtime-only fields and are never serialized in a report.
type Options struct {
	ServerURL         string             `json:"serverUrl"`
	APIKey            string             `json:"-"`
	AllowLoopbackHTTP bool               `json:"allowLoopbackHttp"`
	SkipUnresolved    bool               `json:"skipUnresolved"`
	Timeout           time.Duration      `json:"timeout"`
	Resume            *Report            `json:"-"`
	Progress          func(Report) error `json:"-"`
}

// PlanReport is an offline preview of the media and source relationships in
// one verified portable archive. It contains no server/account data.
type PlanReport struct {
	SchemaVersion        int              `json:"schemaVersion"`
	Status               string           `json:"status"`
	PlanID               string           `json:"planId"`
	ManifestSHA256       string           `json:"manifestSha256"`
	DateTransferPolicy   string           `json:"dateTransferPolicy"`
	FileModifiedAtNotice string           `json:"fileModifiedAtNotice"`
	MediaOccurrences     int              `json:"mediaOccurrences"`
	UniqueContents       int              `json:"uniqueContents"`
	SourceAlbums         int              `json:"sourceAlbums"`
	UnresolvedCount      int              `json:"unresolvedCount"`
	SkippedCount         int              `json:"skippedCount"`
	Files                []PlannedFile    `json:"files"`
	Sidecars             []PlannedSidecar `json:"sidecars"`
	Albums               []PlannedAlbum   `json:"albums"`
	Issues               []Issue          `json:"issues"`
}

// PlannedFile describes one source occurrence; repeated occurrences remain
// distinct even when they have identical bytes.
type PlannedFile struct {
	OccurrenceID   string   `json:"occurrenceId"`
	SourceEntry    string   `json:"sourceEntry"`
	OutputPath     string   `json:"outputPath"`
	SHA256         string   `json:"sha256"`
	Bytes          int64    `json:"bytes"`
	Date           string   `json:"date,omitempty"`
	SourceAlbumIDs []string `json:"sourceAlbumIds"`
	State          string   `json:"state"`
	Reason         string   `json:"reason,omitempty"`
}

// PlannedSidecar records preserved raw Takeout metadata. Sidecars are not
// uploaded to Immich in v1.
type PlannedSidecar struct {
	SidecarID   string `json:"sidecarId"`
	SourceEntry string `json:"sourceEntry"`
	SHA256      string `json:"sha256"`
	Bytes       int64  `json:"bytes"`
	State       string `json:"state"`
	Transferred bool   `json:"transferred"`
}

// PlannedAlbum preserves a source album's identity separately from its title.
type PlannedAlbum struct {
	SourceAlbumID string   `json:"sourceAlbumId"`
	Title         string   `json:"title"`
	Folder        string   `json:"folder"`
	OccurrenceIDs []string `json:"occurrenceIds"`
	State         string   `json:"state"`
}

// Report is a versioned, private, resumable account of one import or remote
// verification. It intentionally excludes API keys and unbounded server data.
type Report struct {
	SchemaVersion        int                 `json:"schemaVersion"`
	Mode                 string              `json:"mode"`
	Status               string              `json:"status"`
	PlanID               string              `json:"planId"`
	ManifestSHA256       string              `json:"manifestSha256"`
	DateTransferPolicy   string              `json:"dateTransferPolicy"`
	ServerOrigin         string              `json:"serverOrigin"`
	ServerVersion        string              `json:"serverVersion"`
	AccountID            string              `json:"accountId"`
	FileModifiedAtNotice string              `json:"fileModifiedAtNotice"`
	Files                []OccurrenceResult  `json:"files"`
	Sidecars             []PlannedSidecar    `json:"sidecars"`
	Contents             []ContentResult     `json:"contents"`
	Albums               []AlbumResult       `json:"albums"`
	Memberships          []MembershipResult  `json:"memberships"`
	Issues               []Issue             `json:"issues"`
	Verification         *RemoteVerification `json:"verification,omitempty"`
}

// OccurrenceResult is the import state of a single media occurrence.
type OccurrenceResult struct {
	OccurrenceID   string   `json:"occurrenceId"`
	SourceEntry    string   `json:"sourceEntry"`
	OutputPath     string   `json:"outputPath"`
	SHA256         string   `json:"sha256"`
	Bytes          int64    `json:"bytes"`
	Date           string   `json:"date,omitempty"`
	SourceAlbumIDs []string `json:"sourceAlbumIds"`
	State          string   `json:"state"`
	Reason         string   `json:"reason,omitempty"`
	RemoteAssetID  string   `json:"remoteAssetId,omitempty"`
}

// ContentResult groups identical file bytes for upload/deduplication while
// retaining every source occurrence in OccurrenceIDs.
type ContentResult struct {
	SHA256                 string   `json:"sha256"`
	Bytes                  int64    `json:"bytes"`
	OccurrenceIDs          []string `json:"occurrenceIds"`
	RemoteAssetID          string   `json:"remoteAssetId,omitempty"`
	State                  string   `json:"state"`
	OriginalSHA256Verified bool     `json:"originalSha256Verified"`
	VerifiedOriginalBytes  int64    `json:"verifiedOriginalBytes"`
}

// AlbumResult is tied to the source album ID by an exact namespaced marker.
// A matching title alone is never enough to reuse a remote album.
type AlbumResult struct {
	SourceAlbumID string   `json:"sourceAlbumId"`
	Title         string   `json:"title"`
	Folder        string   `json:"folder"`
	Marker        string   `json:"marker"`
	OccurrenceIDs []string `json:"occurrenceIds"`
	RemoteAlbumID string   `json:"remoteAlbumId,omitempty"`
	OwnerID       string   `json:"ownerId,omitempty"`
	State         string   `json:"state"`
}

// MembershipResult records each source album-to-occurrence relationship.
type MembershipResult struct {
	SourceAlbumID string `json:"sourceAlbumId"`
	OccurrenceID  string `json:"occurrenceId"`
	RemoteAlbumID string `json:"remoteAlbumId,omitempty"`
	RemoteAssetID string `json:"remoteAssetId,omitempty"`
	State         string `json:"state"`
}

// Issue contains a stable, sanitized diagnostic. It must never contain an API
// key, response body, server header, or raw transport error.
type Issue struct {
	Code          string `json:"code"`
	Details       string `json:"details"`
	OccurrenceID  string `json:"occurrenceId,omitempty"`
	SourceAlbumID string `json:"sourceAlbumId,omitempty"`
	SourceEntry   string `json:"sourceEntry,omitempty"`
}

// RemoteVerification summarizes read-only checks performed against the
// selected remote assets and exact album memberships.
type RemoteVerification struct {
	Status             string `json:"status"`
	ContentsChecked    int    `json:"contentsChecked"`
	AlbumsChecked      int    `json:"albumsChecked"`
	MembershipsChecked int    `json:"membershipsChecked"`
	BytesChecked       int64  `json:"bytesChecked"`
}

// Error is a stable error envelope. Cause is available to errors.Is/As but is
// never included in Error(), preventing URLs, response bodies, or credentials
// from leaking through a transport error.
type Error struct {
	Code    string
	Message string
	cause   error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return "Immich operation failed."
}

func (e *Error) Unwrap() error { return e.cause }

var (
	ErrInvalidOptions = errors.New("invalid Immich options")
	ErrPreflight      = errors.New("Immich preflight failed")
	ErrUnresolved     = errors.New("archive contains unresolved media metadata")
	ErrResumeMismatch = errors.New("resume report does not match this operation")
)
