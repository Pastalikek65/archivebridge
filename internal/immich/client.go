package immich

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxJSONResponseBytes = int64(16 << 20)
	maxRemoteAlbums      = 10_000
	maxSearchAssets      = 100_000
	searchPageSize       = 1_000
	remoteMetadataWait   = 120 * time.Second
	remoteMetadataPoll   = 250 * time.Millisecond
)

type apiClient struct {
	origin string
	key    string
	http   *http.Client
}

func newAPIClient(opts Options) (*apiClient, string, error) {
	origin, err := normalizeServerOrigin(opts.ServerURL, opts.AllowLoopbackHTTP)
	if err != nil {
		return nil, "", err
	}
	if opts.APIKey == "" || strings.TrimSpace(opts.APIKey) != opts.APIKey || hasControl(opts.APIKey) {
		return nil, "", &Error{Code: "INVALID_API_KEY", Message: "An API key is required and must not contain control characters.", cause: ErrInvalidOptions}
	}
	if opts.Timeout < 0 {
		return nil, "", &Error{Code: "INVALID_TIMEOUT", Message: "Request timeout cannot be negative.", cause: ErrInvalidOptions}
	}
	transport := &http.Transport{
		Proxy:              nil,
		TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
		DisableCompression: true,
		ForceAttemptHTTP2:  true,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   opts.Timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &apiClient{origin: origin, key: opts.APIKey, http: client}, origin, nil
}

func normalizeServerOrigin(raw string, allowLoopbackHTTP bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", &Error{Code: "INVALID_SERVER_URL", Message: "Server URL must be an origin URL without credentials, path, query, or fragment.", cause: ErrInvalidOptions}
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", &Error{Code: "INVALID_SERVER_URL", Message: "Server URL scheme must be HTTPS; HTTP is permitted only for explicitly allowed literal loopback.", cause: ErrInvalidOptions}
	}
	if u.Path != "" && u.Path != "/" && u.Path != "/api" {
		return "", &Error{Code: "INVALID_SERVER_URL", Message: "Server URL may include only the Immich /api prefix.", cause: ErrInvalidOptions}
	}
	if strings.Contains(u.Hostname(), "%") {
		return "", &Error{Code: "INVALID_SERVER_URL", Message: "Server URL contains an unsupported host form.", cause: ErrInvalidOptions}
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || hasControl(host) {
		return "", &Error{Code: "INVALID_SERVER_URL", Message: "Server URL has an invalid host.", cause: ErrInvalidOptions}
	}
	port := u.Port()
	if port != "" {
		n, parseErr := strconv.Atoi(port)
		if parseErr != nil || n < 1 || n > 65535 {
			return "", &Error{Code: "INVALID_SERVER_URL", Message: "Server URL has an invalid port.", cause: ErrInvalidOptions}
		}
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(host)
		if !allowLoopbackHTTP || ip == nil || !(ip.Equal(net.ParseIP("127.0.0.1")) || ip.Equal(net.ParseIP("::1"))) {
			return "", &Error{Code: "HTTP_LOOPBACK_REQUIRED", Message: "Plain HTTP requires explicit opt-in and literal 127.0.0.1 or ::1.", cause: ErrInvalidOptions}
		}
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return u.Scheme + "://" + host, nil
}

func (c *apiClient) request(ctx context.Context, method, path string, body io.Reader, contentType string, status ...int) (*http.Response, error) {
	return c.requestHeaders(ctx, method, path, body, contentType, nil, status...)
}

func (c *apiClient) requestHeaders(ctx context.Context, method, path string, body io.Reader, contentType string, headers http.Header, status ...int) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.origin+"/api"+path, body)
	if err != nil {
		return nil, &Error{Code: "REQUEST_INVALID", Message: "Immich request could not be constructed.", cause: err}
	}
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{Code: "NETWORK_ERROR", Message: "Immich request failed or was interrupted.", cause: err}
	}
	for _, expected := range status {
		if resp.StatusCode == expected {
			return resp, nil
		}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, &Error{Code: "REDIRECT_REFUSED", Message: "Immich redirected a request; redirects are disabled for credential safety."}
	}
	if resp.StatusCode >= http.StatusInternalServerError && (method == http.MethodPost || method == http.MethodPut) {
		return nil, &Error{Code: "MUTATION_RESPONSE_UNKNOWN", Message: "Immich returned a server error after a write request; the outcome must be reconciled before retry."}
	}
	return nil, &Error{Code: "SERVER_RESPONSE", Message: fmt.Sprintf("Immich returned unexpected HTTP status %d.", resp.StatusCode)}
}

func (c *apiClient) jsonRequest(ctx context.Context, method, path string, body any, status ...int) (*http.Response, error) {
	var encoded []byte
	var reader io.Reader
	contentType := ""
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, &Error{Code: "REQUEST_INVALID", Message: "Immich request could not be encoded safely.", cause: err}
		}
		if int64(len(encoded)) > maxJSONResponseBytes {
			return nil, &Error{Code: "REQUEST_TOO_LARGE", Message: "Immich request exceeds the configured JSON bound."}
		}
		reader = bytes.NewReader(encoded)
		contentType = "application/json"
	}
	return c.request(ctx, method, path, reader, contentType, status...)
}

func decodeJSONResponse(resp *http.Response, target any) error {
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONResponseBytes+1))
	if err != nil {
		return &Error{Code: "RESPONSE_READ_FAILED", Message: "Immich response could not be read safely.", cause: err}
	}
	if int64(len(data)) > maxJSONResponseBytes {
		return &Error{Code: "RESPONSE_TOO_LARGE", Message: "Immich JSON response exceeds the configured bound."}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(target); err != nil {
		return &Error{Code: "RESPONSE_INVALID", Message: "Immich returned malformed or incomplete JSON.", cause: err}
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return &Error{Code: "RESPONSE_INVALID", Message: "Immich returned trailing data after its JSON response.", cause: err}
	}
	return nil
}

func (c *apiClient) json(ctx context.Context, method, path string, body, target any, status ...int) error {
	resp, err := c.jsonRequest(ctx, method, path, body, status...)
	if err != nil {
		return err
	}
	return decodeJSONResponse(resp, target)
}

type versionDTO struct {
	Major      int             `json:"major"`
	Minor      int             `json:"minor"`
	Patch      int             `json:"patch"`
	Prerelease json.RawMessage `json:"prerelease"`
}

type apiKeyDTO struct {
	Permissions []string `json:"permissions"`
}

type userDTO struct {
	ID string `json:"id"`
}

func (c *apiClient) preflight(ctx context.Context) (string, string, error) {
	var version versionDTO
	if err := c.json(ctx, http.MethodGet, "/server/version", nil, &version, http.StatusOK); err != nil {
		return "", "", err
	}
	if version.Major != 3 || version.Minor != 3 || version.Patch != 1 || len(version.Prerelease) == 0 || !bytes.Equal(bytes.TrimSpace(version.Prerelease), []byte("null")) {
		return "", "", &Error{Code: "UNSUPPORTED_SERVER_VERSION", Message: "Immich server must be exactly version 3.3.1."}
	}
	var key apiKeyDTO
	if err := c.json(ctx, http.MethodGet, "/api-keys/me", nil, &key, http.StatusOK); err != nil {
		return "", "", err
	}
	permissions := make(map[string]bool, len(key.Permissions))
	for _, permission := range key.Permissions {
		permissions[permission] = true
	}
	if permissions["all"] {
		for _, required := range RequiredPermissions() {
			permissions[required] = true
		}
	}
	missing := make([]string, 0)
	for _, required := range RequiredPermissions() {
		if !permissions[required] {
			missing = append(missing, required)
		}
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		return "", "", &Error{Code: "MISSING_PERMISSIONS", Message: "API key is missing one or more required Immich permissions.", cause: ErrPreflight}
	}
	var user userDTO
	if err := c.json(ctx, http.MethodGet, "/users/me", nil, &user, http.StatusOK); err != nil {
		return "", "", err
	}
	if !validUUID(user.ID) {
		return "", "", &Error{Code: "ACCOUNT_ID_INVALID", Message: "Immich did not return a valid authenticated account ID."}
	}
	return SupportedServerVersion, strings.ToLower(user.ID), nil
}

type remoteAlbumDTO struct {
	ID          string            `json:"id"`
	Name        string            `json:"albumName"`
	Description string            `json:"description"`
	AlbumUsers  []remoteAlbumUser `json:"albumUsers"`
}

type remoteAlbumUser struct {
	Role string `json:"role"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
}

type createAlbumDTO struct {
	AlbumName   string `json:"albumName"`
	Description string `json:"description"`
}

type assetDTO struct {
	ID             string            `json:"id"`
	Checksum       string            `json:"checksum"`
	FileCreatedAt  string            `json:"fileCreatedAt"`
	FileModifiedAt string            `json:"fileModifiedAt"`
	OwnerID        string            `json:"ownerId"`
	IsTrashed      bool              `json:"isTrashed"`
	HasMetadata    bool              `json:"hasMetadata"`
	ExifInfo       *assetExifInfoDTO `json:"exifInfo"`
}

type assetExifInfoDTO struct {
	DateTimeOriginal string `json:"dateTimeOriginal"`
}

type uploadResponseDTO struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type bulkCheckRequest struct {
	Assets []bulkCheckItem `json:"assets"`
}

type bulkCheckItem struct {
	ID       string `json:"id"`
	Checksum string `json:"checksum"`
}

type bulkCheckResponse struct {
	Results []bulkCheckResult `json:"results"`
}

type bulkCheckResult struct {
	ID        string `json:"id"`
	Action    string `json:"action"`
	AssetID   string `json:"assetId"`
	IsTrashed bool   `json:"isTrashed"`
	Reason    string `json:"reason"`
}

type searchRequest struct {
	Size   int          `json:"size"`
	Cursor string       `json:"cursor,omitempty"`
	Filter searchFilter `json:"filter"`
}

type searchFilter struct {
	AlbumIDs *idFilter     `json:"albumIds,omitempty"`
	Checksum *stringFilter `json:"checksum,omitempty"`
}

type idFilter struct {
	Any []string `json:"any"`
}

type stringFilter struct {
	Eq string `json:"eq"`
}

type searchResponse struct {
	Assets *searchAssets `json:"assets"`
}

type searchAssets struct {
	Items      *[]assetDTO     `json:"items"`
	NextCursor json.RawMessage `json:"nextCursor"`
}

func (c *apiClient) getAlbums(ctx context.Context) ([]remoteAlbumDTO, error) {
	var albums []remoteAlbumDTO
	if err := c.json(ctx, http.MethodGet, "/albums", nil, &albums, http.StatusOK); err != nil {
		return nil, err
	}
	if albums == nil || len(albums) > maxRemoteAlbums {
		return nil, &Error{Code: "ALBUM_LIST_INVALID", Message: "Immich album list is missing or exceeds the configured bound."}
	}
	for _, album := range albums {
		if !validUUID(album.ID) || album.Name == "" || hasControl(album.Name) || hasControl(album.Description) || len(album.AlbumUsers) == 0 {
			return nil, &Error{Code: "ALBUM_LIST_INVALID", Message: "Immich returned an incomplete album record."}
		}
		for _, member := range album.AlbumUsers {
			if !validUUID(member.User.ID) || (member.Role != "owner" && member.Role != "editor" && member.Role != "viewer") {
				return nil, &Error{Code: "ALBUM_LIST_INVALID", Message: "Immich returned an incomplete album ownership record."}
			}
		}
		if album.AlbumUsers[0].Role != "owner" {
			return nil, &Error{Code: "ALBUM_LIST_INVALID", Message: "Immich album response did not identify its owner first."}
		}
	}
	return albums, nil
}

func (c *apiClient) bulkUploadCheck(ctx context.Context, id, checksum string) (bulkCheckResult, error) {
	var result bulkCheckResponse
	err := c.json(ctx, http.MethodPost, "/assets/bulk-upload-check", bulkCheckRequest{Assets: []bulkCheckItem{{ID: id, Checksum: checksum}}}, &result, http.StatusOK)
	if err != nil {
		return bulkCheckResult{}, err
	}
	if len(result.Results) != 1 || result.Results[0].ID != id {
		return bulkCheckResult{}, &Error{Code: "UPLOAD_CHECK_INVALID", Message: "Immich returned an invalid duplicate-check result."}
	}
	item := result.Results[0]
	if item.Action != "accept" && item.Action != "reject" {
		return bulkCheckResult{}, &Error{Code: "UPLOAD_CHECK_INVALID", Message: "Immich returned an unsupported upload-check action."}
	}
	if item.Action == "reject" && item.Reason != "duplicate" && item.Reason != "unsupported-format" {
		return bulkCheckResult{}, &Error{Code: "UPLOAD_CHECK_INVALID", Message: "Immich returned an unsupported upload rejection reason."}
	}
	if item.Action == "reject" && item.Reason == "duplicate" && (!validUUID(item.AssetID) || item.IsTrashed) {
		return bulkCheckResult{}, &Error{Code: "DUPLICATE_REJECTED", Message: "Immich reported a duplicate that is missing or trashed."}
	}
	return item, nil
}

func (c *apiClient) getAsset(ctx context.Context, id string) (assetDTO, error) {
	if !validUUID(id) {
		return assetDTO{}, &Error{Code: "REMOTE_ID_INVALID", Message: "Remote asset identifier is malformed."}
	}
	var asset assetDTO
	err := c.json(ctx, http.MethodGet, "/assets/"+url.PathEscape(id), nil, &asset, http.StatusOK)
	if err != nil {
		return assetDTO{}, err
	}
	if !validUUID(asset.ID) || strings.ToLower(asset.ID) != strings.ToLower(id) || !validUUID(asset.OwnerID) || asset.FileCreatedAt == "" || asset.FileModifiedAt == "" || asset.Checksum == "" {
		return assetDTO{}, &Error{Code: "ASSET_RESPONSE_INVALID", Message: "Immich returned incomplete media verification metadata."}
	}
	return asset, nil
}

func (c *apiClient) searchChecksum(ctx context.Context, checksum string) ([]assetDTO, error) {
	return c.searchAssets(ctx, searchFilter{Checksum: &stringFilter{Eq: checksum}})
}

func (c *apiClient) searchAlbumMembers(ctx context.Context, albumID string) ([]string, error) {
	if !validUUID(albumID) {
		return nil, &Error{Code: "REMOTE_ID_INVALID", Message: "Remote album identifier is malformed."}
	}
	assets, err := c.searchAssets(ctx, searchFilter{AlbumIDs: &idFilter{Any: []string{strings.ToLower(albumID)}}})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(assets))
	for _, asset := range assets {
		ids = append(ids, strings.ToLower(asset.ID))
	}
	sort.Strings(ids)
	return ids, nil
}

func (c *apiClient) searchAssets(ctx context.Context, filter searchFilter) ([]assetDTO, error) {
	assets := make([]assetDTO, 0)
	cursor := ""
	seenCursors := map[string]bool{}
	seenIDs := map[string]bool{}
	for page := 0; page < maxSearchAssets/searchPageSize; page++ {
		request := searchRequest{Size: searchPageSize, Cursor: cursor, Filter: filter}
		var response searchResponse
		if err := c.json(ctx, http.MethodPost, "/search/metadata", request, &response, http.StatusOK); err != nil {
			return nil, err
		}
		if response.Assets == nil || response.Assets.Items == nil || len(*response.Assets.Items) > searchPageSize {
			return nil, &Error{Code: "SEARCH_RESPONSE_INVALID", Message: "Immich returned an incomplete or oversized search page."}
		}
		for _, asset := range *response.Assets.Items {
			id := strings.ToLower(asset.ID)
			if !validUUID(id) || seenIDs[id] {
				return nil, &Error{Code: "SEARCH_RESPONSE_INVALID", Message: "Immich search returned an invalid or repeated asset identifier."}
			}
			seenIDs[id] = true
			assets = append(assets, asset)
			if len(assets) > maxSearchAssets {
				return nil, &Error{Code: "SEARCH_RESULT_LIMIT", Message: "Immich search result exceeds the configured safety bound."}
			}
		}
		var next *string
		if len(response.Assets.NextCursor) == 0 {
			return nil, &Error{Code: "SEARCH_RESPONSE_INVALID", Message: "Immich search response omitted its pagination cursor field."}
		}
		if !bytes.Equal(bytes.TrimSpace(response.Assets.NextCursor), []byte("null")) {
			var value string
			if err := json.Unmarshal(response.Assets.NextCursor, &value); err != nil || value == "" {
				return nil, &Error{Code: "SEARCH_RESPONSE_INVALID", Message: "Immich returned an invalid pagination cursor."}
			}
			next = &value
		}
		if next == nil {
			return assets, nil
		}
		if seenCursors[*next] || *next == cursor {
			return nil, &Error{Code: "SEARCH_CURSOR_CYCLE", Message: "Immich repeated a search cursor; pagination was stopped safely."}
		}
		seenCursors[*next] = true
		cursor = *next
	}
	return nil, &Error{Code: "SEARCH_PAGE_LIMIT", Message: "Immich search exceeded the configured pagination bound."}
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func hasControl(value string) bool {
	if !utf8.ValidString(value) {
		return true
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
