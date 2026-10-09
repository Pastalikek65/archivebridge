package immich

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fakeAccountID = "11111111-1111-4111-8111-111111111111"
	fakeAssetID   = "22222222-2222-4222-8222-222222222222"
	fakeAlbumID   = "33333333-3333-4333-8333-333333333333"
)

type fakeAsset struct {
	id                   string
	sha1                 string
	sha256               string
	bytes                []byte
	created              string
	modified             string
	exifDateTimeOriginal string
	metadataProcessed    bool
	metadataPendingReads int
	owner                string
	trashed              bool
}

type fakeUpload struct {
	created, modified, filename, checksum string
	assetData, sidecarData                []byte
	sidecarFilename, sidecarContentType   string
	fieldOrder                            []string
}

type fakeAlbum struct {
	id          string
	name        string
	description string
	owner       string
	members     map[string]bool
}

type fakeImmich struct {
	mu                     sync.Mutex
	assets                 map[string]fakeAsset
	bySHA1                 map[string]string
	albums                 map[string]*fakeAlbum
	writes                 int
	requests               []string
	readDates              []string
	uploads                []fakeUpload
	exifDate               string
	metadataDelayReads     int
	dateAfterOriginal      string
	ignoreGeneratedSidecar bool
	dropExifAfterOriginal  bool
	wrongChecksum          bool
}

func newFakeImmich() *fakeImmich {
	return &fakeImmich{assets: map[string]fakeAsset{}, bySHA1: map[string]string{}, albums: map[string]*fakeAlbum{}}
}

func (f *fakeImmich) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	route := strings.TrimPrefix(r.URL.Path, "/api")
	if r.Header.Get("x-api-key") != testAPIKey {
		http.Error(w, "missing key", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && route == "/server/version":
		writeFakeJSON(w, http.StatusOK, map[string]any{"major": 3, "minor": 3, "patch": 1, "prerelease": nil})
	case r.Method == http.MethodGet && route == "/api-keys/me":
		writeFakeJSON(w, http.StatusOK, map[string]any{"permissions": RequiredPermissions()})
	case r.Method == http.MethodGet && route == "/users/me":
		writeFakeJSON(w, http.StatusOK, map[string]any{"id": fakeAccountID, "email": testAPIKey})
	case r.Method == http.MethodGet && route == "/albums":
		f.mu.Lock()
		albums := make([]map[string]any, 0, len(f.albums))
		for _, album := range f.albums {
			albums = append(albums, albumJSON(album))
		}
		f.mu.Unlock()
		sort.Slice(albums, func(i, j int) bool { return albums[i]["id"].(string) < albums[j]["id"].(string) })
		writeFakeJSON(w, http.StatusOK, albums)
	case r.Method == http.MethodPost && route == "/assets/bulk-upload-check":
		var input bulkCheckRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Assets) != 1 {
			http.Error(w, "bad check", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		assetID := f.bySHA1[input.Assets[0].Checksum]
		f.mu.Unlock()
		item := map[string]any{"id": input.Assets[0].ID, "action": "accept"}
		if assetID != "" {
			item["action"], item["assetId"], item["reason"] = "reject", assetID, "duplicate"
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{"results": []any{item}})
	case r.Method == http.MethodPost && route == "/assets":
		upload, err := readUploadMultipart(r)
		if err != nil {
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		if upload.filename == "" || upload.checksum == "" {
			http.Error(w, "missing upload metadata", http.StatusBadRequest)
			return
		}
		s1 := sha1.Sum(upload.assetData)
		s256 := sha256.Sum256(upload.assetData)
		if base64.StdEncoding.EncodeToString(s1[:]) != upload.checksum {
			http.Error(w, "checksum header mismatch", http.StatusBadRequest)
			return
		}
		exifDate := f.exifDate
		if generatedDate := testXMPDateTimeOriginal(upload.sidecarData); generatedDate != "" && !f.ignoreGeneratedSidecar {
			exifDate = generatedDate
		}
		f.mu.Lock()
		f.writes++
		id := fakeAssetID
		for f.assets[id].id != "" {
			id = "44444444-4444-4444-8444-444444444444"
		}
		asset := fakeAsset{
			id: id, sha1: base64.StdEncoding.EncodeToString(s1[:]), sha256: hex.EncodeToString(s256[:]),
			bytes: append([]byte{}, upload.assetData...), created: upload.created, modified: upload.modified,
			exifDateTimeOriginal: exifDate, metadataPendingReads: f.metadataDelayReads, owner: fakeAccountID,
		}
		f.assets[id] = asset
		f.bySHA1[asset.sha1] = id
		f.uploads = append(f.uploads, upload)
		f.mu.Unlock()
		writeFakeJSON(w, http.StatusCreated, map[string]any{"id": id, "status": "created"})
	case r.Method == http.MethodPost && route == "/albums":
		var input createAlbumDTO
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "bad album", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.writes++
		id := fakeAlbumID
		for f.albums[id] != nil {
			id = "55555555-5555-4555-8555-555555555555"
		}
		album := &fakeAlbum{id: id, name: input.AlbumName, description: input.Description, owner: fakeAccountID, members: map[string]bool{}}
		f.albums[id] = album
		encoded := albumJSON(album)
		f.mu.Unlock()
		writeFakeJSON(w, http.StatusCreated, encoded)
	case strings.HasPrefix(route, "/assets/") && strings.HasSuffix(route, "/original") && r.Method == http.MethodGet:
		id := strings.TrimSuffix(strings.TrimPrefix(route, "/assets/"), "/original")
		f.mu.Lock()
		asset, ok := f.assets[id]
		wrong := f.wrongChecksum
		if ok && f.dateAfterOriginal != "" {
			asset.created = f.dateAfterOriginal
			asset.modified = f.dateAfterOriginal
			asset.exifDateTimeOriginal = f.dateAfterOriginal
			asset.metadataProcessed = true
			f.assets[id] = asset
		} else if ok && f.dropExifAfterOriginal {
			asset.exifDateTimeOriginal = ""
			asset.metadataProcessed = true
			f.assets[id] = asset
		}
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		data := asset.bytes
		if wrong && len(data) > 0 {
			data = append([]byte{}, data...)
			data[0] ^= 0xff
		}
		_, _ = w.Write(data)
	case strings.HasPrefix(route, "/assets/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(route, "/assets/")
		f.mu.Lock()
		asset, ok := f.assets[id]
		if ok && !asset.metadataProcessed {
			if asset.metadataPendingReads > 0 {
				asset.metadataPendingReads--
			} else if asset.exifDateTimeOriginal != "" {
				asset.created = asset.exifDateTimeOriginal
				asset.modified = asset.exifDateTimeOriginal
				asset.metadataProcessed = true
			}
			f.assets[id] = asset
		}
		f.readDates = append(f.readDates, asset.created)
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		response := map[string]any{"id": asset.id, "checksum": asset.sha1, "fileCreatedAt": asset.created, "fileModifiedAt": asset.modified, "ownerId": asset.owner, "isTrashed": asset.trashed, "hasMetadata": asset.metadataProcessed}
		if asset.metadataProcessed && asset.exifDateTimeOriginal != "" {
			response["exifInfo"] = map[string]any{"dateTimeOriginal": asset.exifDateTimeOriginal}
		}
		writeFakeJSON(w, http.StatusOK, response)
	case r.Method == http.MethodPost && route == "/search/metadata":
		var input struct {
			Filter struct {
				AlbumIDs *struct {
					Any []string `json:"any"`
				} `json:"albumIds"`
			} `json:"filter"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "bad search", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		items := make([]fakeAsset, 0)
		if input.Filter.AlbumIDs != nil {
			for _, albumID := range input.Filter.AlbumIDs.Any {
				for id := range f.albums[albumID].members {
					items = append(items, f.assets[id])
				}
			}
		}
		f.mu.Unlock()
		seen := map[string]bool{}
		result := make([]map[string]any, 0, len(items))
		for _, asset := range items {
			if !seen[asset.id] {
				seen[asset.id] = true
				result = append(result, map[string]any{"id": asset.id, "checksum": asset.sha1, "fileCreatedAt": asset.created, "fileModifiedAt": asset.modified, "ownerId": asset.owner, "isTrashed": asset.trashed})
			}
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{"assets": map[string]any{"items": result, "nextCursor": nil}})
	case r.Method == http.MethodPut && strings.HasPrefix(route, "/albums/") && strings.HasSuffix(route, "/assets"):
		albumID := strings.TrimSuffix(strings.TrimPrefix(route, "/albums/"), "/assets")
		var input struct {
			IDs []string `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "bad membership", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		album := f.albums[albumID]
		if album == nil {
			f.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		f.writes++
		result := make([]map[string]any, 0, len(input.IDs))
		for _, id := range input.IDs {
			album.members[id] = true
			result = append(result, map[string]any{"id": id, "success": true})
		}
		f.mu.Unlock()
		writeFakeJSON(w, http.StatusOK, result)
	default:
		http.Error(w, "unexpected request", http.StatusNotFound)
	}
}

func writeFakeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func albumJSON(album *fakeAlbum) map[string]any {
	return map[string]any{
		"id": album.id, "albumName": album.name, "description": album.description,
		"albumUsers": []any{map[string]any{"role": "owner", "user": map[string]any{"id": album.owner}}},
	}
}

func readMultipart(r *http.Request) (created, modified, filename, checksum string, data []byte, err error) {
	upload, err := readUploadMultipart(r)
	if err != nil {
		return "", "", "", "", nil, err
	}
	return upload.created, upload.modified, upload.filename, upload.checksum, upload.assetData, nil
}

func readUploadMultipart(r *http.Request) (fakeUpload, error) {
	upload := fakeUpload{checksum: r.Header.Get("x-immich-checksum")}
	reader, err := r.MultipartReader()
	if err != nil {
		return fakeUpload{}, err
	}
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return fakeUpload{}, nextErr
		}
		value, readErr := io.ReadAll(part)
		formName := part.FormName()
		filename := part.FileName()
		contentType := part.Header.Get("Content-Type")
		_ = part.Close()
		if readErr != nil {
			return fakeUpload{}, readErr
		}
		upload.fieldOrder = append(upload.fieldOrder, formName)
		switch formName {
		case "fileCreatedAt":
			upload.created = string(value)
		case "fileModifiedAt":
			upload.modified = string(value)
		case "filename":
			upload.filename = string(value)
		case "assetData":
			upload.assetData = value
		case "sidecarData":
			upload.sidecarData = value
			upload.sidecarFilename = filename
			upload.sidecarContentType = contentType
		}
	}
	return upload, nil
}

func testXMPDateTimeOriginal(data []byte) string {
	return testXMPAttribute(data, "http://ns.adobe.com/exif/1.0/", "DateTimeOriginal")
}

func testXMPAttribute(data []byte, namespace, local string) string {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "Description" {
			continue
		}
		for _, attr := range start.Attr {
			if attr.Name.Space == namespace && attr.Name.Local == local {
				return attr.Value
			}
		}
	}
}

func TestImportVerifyAndRepeatPreserveIdentityAndAvoidDuplicateWrites(t *testing.T) {
	archive := datedArchive(t)
	fake := newFakeImmich()
	server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	defer server.Close()
	opts := Options{ServerURL: server.URL, APIKey: testAPIKey, AllowLoopbackHTTP: true}

	var progress []Report
	opts.Progress = func(report Report) error {
		progress = append(progress, report)
		return nil
	}
	imported, err := Import(context.Background(), archive, opts)
	if err != nil {
		t.Fatalf("Import: %v; report: %+v", err, imported)
	}
	if imported.Status != "completed" || len(imported.Files) != 1 || imported.Files[0].State != "uploaded" || len(imported.Contents) != 1 || !imported.Contents[0].OriginalSHA256Verified || imported.Contents[0].VerifiedOriginalBytes != imported.Contents[0].Bytes {
		t.Fatalf("import report does not attest to the uploaded original: %+v", imported)
	}
	if len(imported.Albums) != 1 || imported.Albums[0].State != "verified" || len(imported.Memberships) != 1 || imported.Memberships[0].State != "added" {
		t.Fatalf("source album relationship was not created and verified: %+v", imported)
	}
	if len(progress) < 5 || progress[0].ServerOrigin == "" {
		t.Fatalf("expected durable progress checkpoints with authenticated server identity: %+v", progress)
	}
	if fake.writes != 3 {
		t.Fatalf("expected one upload, album create, and membership write; got %d writes", fake.writes)
	}
	for _, request := range fake.requests {
		if !strings.Contains(request, " /api/") {
			t.Fatalf("authenticated Immich API request omitted the required /api prefix: %q", request)
		}
	}

	verified, err := VerifyRemote(context.Background(), archive, opts, *imported)
	if err != nil || verified.Status != "verified" || verified.Verification == nil || verified.Verification.ContentsChecked != 1 || verified.Verification.AlbumsChecked != 1 || verified.Verification.MembershipsChecked != 1 {
		t.Fatalf("VerifyRemote report=%+v err=%v", verified, err)
	}
	if fake.writes != 3 {
		t.Fatalf("read-only remote verification performed a mutation: %d writes", fake.writes)
	}

	// A repeated import reconciles the duplicate only after downloading and
	// hashing the complete original, then leaves the existing album untouched.
	second, err := Import(context.Background(), archive, Options{ServerURL: server.URL, APIKey: testAPIKey, AllowLoopbackHTTP: true})
	if err != nil || second.Status != "completed" || second.Contents[0].State != "reused" || second.Files[0].State != "reused" {
		t.Fatalf("repeat import failed to safely reuse verified content: report=%+v err=%v", second, err)
	}
	if fake.writes != 3 {
		t.Fatalf("idempotent repeat made remote writes: %d", fake.writes)
	}
}

func TestImportUsesGeneratedDateSidecarBeforeFilenameAndWaitsForWorker(t *testing.T) {
	archive := datedArchive(t)
	fake := newFakeImmich()
	fake.exifDate = "2020-01-02T03:04:05Z"
	fake.metadataDelayReads = 1
	server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	defer server.Close()

	report, err := Import(context.Background(), archive, Options{ServerURL: server.URL, APIKey: testAPIKey, AllowLoopbackHTTP: true})
	if err != nil || report == nil || report.Status != "completed" {
		t.Fatalf("Import did not complete after Immich accepted the generated source date: report=%+v err=%v", report, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.uploads) != 1 {
		t.Fatalf("expected one upload, got %d", len(fake.uploads))
	}
	upload := fake.uploads[0]
	if len(upload.sidecarData) == 0 || len(upload.sidecarData) > 2048 || upload.sidecarFilename != "photo.jpg.xmp" || upload.sidecarContentType != "application/xml" {
		t.Fatalf("upload did not include a bounded XML XMP date sidecar: %+v", upload)
	}
	if len(upload.fieldOrder) < 3 || upload.fieldOrder[0] != "sidecarData" || indexOf(upload.fieldOrder, "sidecarData") > indexOf(upload.fieldOrder, "filename") || indexOf(upload.fieldOrder, "filename") > indexOf(upload.fieldOrder, "assetData") {
		t.Fatalf("XMP must precede filename and original media multipart fields: %v", upload.fieldOrder)
	}
	wantDate := "2023-11-14T22:13:19Z"
	if got := testXMPAttribute(upload.sidecarData, "http://ns.adobe.com/xap/1.0/", "CreateDate"); got != wantDate {
		t.Fatalf("generated XMP CreateDate=%q, want %q", got, wantDate)
	}
	if got := testXMPAttribute(upload.sidecarData, "http://ns.adobe.com/photoshop/1.0/", "DateCreated"); got != wantDate {
		t.Fatalf("generated XMP DateCreated=%q, want %q", got, wantDate)
	}
	if got := testXMPDateTimeOriginal(upload.sidecarData); got != wantDate {
		t.Fatalf("generated XMP DateTimeOriginal=%q, want %q", got, wantDate)
	}
	asset := fake.assets[fakeAssetID]
	if asset.created != wantDate || asset.modified != wantDate || asset.exifDateTimeOriginal != wantDate {
		t.Fatalf("server metadata worker did not retain source date: %+v", asset)
	}
	if len(fake.readDates) < 4 {
		t.Fatalf("expected metadata polling and post-download identity recheck, got %d asset reads: %v", len(fake.readDates), fake.readDates)
	}
	if !report.Contents[0].OriginalSHA256Verified || report.Contents[0].VerifiedOriginalBytes != report.Contents[0].Bytes {
		t.Fatalf("source bytes were not verified after generated metadata transfer: %+v", report.Contents[0])
	}
	if report.DateTransferPolicy != dateTransferPolicy {
		t.Fatalf("report omitted the date transfer policy: %q", report.DateTransferPolicy)
	}
	if fake.writes != 3 || len(fake.albums) != 1 {
		t.Fatalf("date-faithful import did not complete expected album writes: writes=%d albums=%d", fake.writes, len(fake.albums))
	}
}

func TestImportRejectsConflictingProcessedExifDateWithoutAlbumWrites(t *testing.T) {
	archive := datedArchive(t)
	fake := newFakeImmich()
	fake.exifDate = "2020-01-02T03:04:05Z"
	fake.ignoreGeneratedSidecar = true
	server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	defer server.Close()
	report, err := Import(context.Background(), archive, Options{ServerURL: server.URL, APIKey: testAPIKey, AllowLoopbackHTTP: true})
	if err == nil || report == nil || report.Status == "completed" || report.Status == "completed_with_skips" {
		t.Fatalf("conflicting processed Exif date was accepted: report=%+v err=%v", report, err)
	}
	if report.Contents[0].RemoteAssetID == "" || report.Contents[0].State != "failed" {
		t.Fatalf("report did not preserve the accepted but unverified remote asset: %+v", report.Contents[0])
	}
	if fake.writes != 1 || len(fake.albums) != 0 {
		t.Fatalf("conflicting date allowed album mutations: writes=%d albums=%d", fake.writes, len(fake.albums))
	}
}

func TestImportCancellationAfterUploadPreservesAssetIdentity(t *testing.T) {
	archive := datedArchive(t)
	fake := newFakeImmich()
	fake.metadataDelayReads = 100
	server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	report, err := Import(ctx, archive, Options{
		ServerURL: server.URL, APIKey: testAPIKey, AllowLoopbackHTTP: true,
		Progress: func(snapshot Report) error {
			if len(snapshot.Contents) == 1 && snapshot.Contents[0].RemoteAssetID != "" {
				cancel()
			}
			return nil
		},
	})
	if err == nil || report == nil || report.Status != "interrupted" || report.Contents[0].RemoteAssetID == "" || report.Contents[0].State != "in_flight" {
		t.Fatalf("cancellation lost accepted upload identity or claimed completion: report=%+v err=%v", report, err)
	}
	if fake.writes != 1 || len(fake.albums) != 0 {
		t.Fatalf("canceled metadata wait allowed album mutations: writes=%d albums=%d", fake.writes, len(fake.albums))
	}
}

func TestResumeWaitsForAcceptedUploadMetadataBeforeAlbumWrites(t *testing.T) {
	archive := datedArchive(t)
	plan, err := Plan(context.Background(), archive, false)
	if err != nil {
		t.Fatal(err)
	}
	media, err := os.ReadFile(filepath.Join(archive, filepath.FromSlash(plan.Files[0].OutputPath)))
	if err != nil {
		t.Fatal(err)
	}
	sha1Sum := sha1.Sum(media)
	sha256Sum := sha256.Sum256(media)
	fake := newFakeImmich()
	fake.assets[fakeAssetID] = fakeAsset{
		id: fakeAssetID, sha1: base64.StdEncoding.EncodeToString(sha1Sum[:]), sha256: hex.EncodeToString(sha256Sum[:]),
		bytes: media, created: plan.Files[0].Date, modified: plan.Files[0].Date,
		exifDateTimeOriginal: plan.Files[0].Date, metadataPendingReads: 2, owner: fakeAccountID,
	}
	fake.bySHA1[base64.StdEncoding.EncodeToString(sha1Sum[:])] = fakeAssetID
	server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	defer server.Close()
	origin, err := normalizeServerOrigin(server.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	previous := reportFromPlan(plan, "import", "interrupted")
	previous.ServerOrigin, previous.ServerVersion, previous.AccountID = origin, SupportedServerVersion, fakeAccountID
	previous.Files[0].State, previous.Files[0].RemoteAssetID = "in_flight", fakeAssetID
	previous.Contents[0].State, previous.Contents[0].RemoteAssetID = "in_flight", fakeAssetID
	report, err := Import(context.Background(), archive, Options{
		ServerURL: server.URL, APIKey: testAPIKey, AllowLoopbackHTTP: true, Resume: previous,
	})
	if err != nil || report == nil || report.Status != "completed" || report.Contents[0].State != "reused" {
		t.Fatalf("valid in-flight upload did not resume after metadata processing: report=%+v err=%v", report, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.assets[fakeAssetID].metadataProcessed || len(fake.readDates) < 5 {
		t.Fatalf("resume skipped bounded metadata processing before album writes: processed=%v reads=%d dates=%v", fake.assets[fakeAssetID].metadataProcessed, len(fake.readDates), fake.readDates)
	}
	if len(fake.uploads) != 0 || fake.writes != 2 || len(fake.albums) != 1 {
		t.Fatalf("resume should reuse the accepted asset and perform only album writes: uploads=%d writes=%d albums=%d", len(fake.uploads), fake.writes, len(fake.albums))
	}
}

func TestUploadedMetadataWaitHasBoundedTimeout(t *testing.T) {
	fake := newFakeImmich()
	fake.assets[fakeAssetID] = fakeAsset{
		id: fakeAssetID, sha1: base64.StdEncoding.EncodeToString(make([]byte, sha1.Size)),
		created: "2023-11-14T22:13:19Z", modified: "2023-11-14T22:13:19Z", owner: fakeAccountID,
	}
	server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	defer server.Close()
	client, _, err := newAPIClient(Options{ServerURL: server.URL, APIKey: testAPIKey, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	err = client.waitForUploadedMetadataWithBounds(context.Background(), fakeAssetID, fakeAccountID, "2023-11-14T22:13:19Z", "", 15*time.Millisecond, time.Millisecond)
	if err == nil || errorCode(err) != "REMOTE_METADATA_TIMEOUT" {
		t.Fatalf("unprocessed metadata did not hit its bounded timeout: %v", err)
	}
}

func TestImportFailsIfDateChangesAfterOriginalDownload(t *testing.T) {
	archive := datedArchive(t)
	fake := newFakeImmich()
	fake.dateAfterOriginal = "2020-01-02T03:04:05Z"
	server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	defer server.Close()
	report, err := Import(context.Background(), archive, Options{ServerURL: server.URL, APIKey: testAPIKey, AllowLoopbackHTTP: true})
	if err == nil || report == nil || report.Status == "completed" || report.Status == "completed_with_skips" {
		t.Fatalf("import completed after remote source date drifted during original verification: report=%+v err=%v", report, err)
	}
	if report.Contents[0].RemoteAssetID == "" {
		t.Fatal("partial report lost the accepted remote asset identity")
	}
	if fake.writes != 1 || len(fake.albums) != 0 {
		t.Fatalf("date drift allowed follow-on album mutations: writes=%d albums=%d", fake.writes, len(fake.albums))
	}
}

func TestImportFailsIfExifWitnessDisappearsDuringOriginalDownload(t *testing.T) {
	archive := datedArchive(t)
	fake := newFakeImmich()
	fake.dropExifAfterOriginal = true
	server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	defer server.Close()
	report, err := Import(context.Background(), archive, Options{ServerURL: server.URL, APIKey: testAPIKey, AllowLoopbackHTTP: true})
	if err == nil || report == nil || report.Status == "completed" || report.Status == "completed_with_skips" {
		t.Fatalf("import completed after the remote date witness disappeared: report=%+v err=%v", report, err)
	}
	if report.Contents[0].RemoteAssetID == "" || report.Contents[0].State != "failed" {
		t.Fatalf("report did not preserve the accepted asset with an unverified date witness: %+v", report.Contents[0])
	}
	if fake.writes != 1 || len(fake.albums) != 0 {
		t.Fatalf("missing date witness allowed album mutations: writes=%d albums=%d", fake.writes, len(fake.albums))
	}
}

func indexOf(items []string, value string) int {
	for index, item := range items {
		if item == value {
			return index
		}
	}
	return len(items)
}

func TestImportFailsClosedWhenDuplicateOriginalDiffers(t *testing.T) {
	archive := datedArchive(t)
	plan, err := Plan(context.Background(), archive, false)
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeImmich()
	media, err := os.ReadFile(filepath.Join(archive, filepath.FromSlash(plan.Files[0].OutputPath)))
	if err != nil {
		t.Fatal(err)
	}
	h1 := sha1.Sum(media)
	h256 := sha256.Sum256(media)
	fake.assets[fakeAssetID] = fakeAsset{id: fakeAssetID, sha1: base64.StdEncoding.EncodeToString(h1[:]), sha256: hex.EncodeToString(h256[:]), bytes: append([]byte{}, media...), created: plan.Files[0].Date, modified: plan.Files[0].Date, exifDateTimeOriginal: plan.Files[0].Date, metadataProcessed: true, owner: fakeAccountID}
	fake.bySHA1[base64.StdEncoding.EncodeToString(h1[:])] = fakeAssetID
	fake.wrongChecksum = true
	server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	defer server.Close()
	report, importErr := Import(context.Background(), archive, Options{ServerURL: server.URL, APIKey: testAPIKey, AllowLoopbackHTTP: true})
	if importErr == nil || report == nil || report.Status != "failed" || !strings.Contains(importErr.Error(), "complete source SHA-256") {
		t.Fatalf("duplicate original mismatch was accepted: report=%+v err=%v", report, importErr)
	}
	if fake.writes != 0 {
		t.Fatalf("mismatched duplicate led to mutation: %d writes", fake.writes)
	}
}

func TestImportCheckpointFailureStopsBeforeMutation(t *testing.T) {
	archive := datedArchive(t)
	fake := newFakeImmich()
	server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	defer server.Close()
	var callbacks int
	report, err := Import(context.Background(), archive, Options{
		ServerURL: server.URL, APIKey: testAPIKey, AllowLoopbackHTTP: true,
		Progress: func(Report) error {
			callbacks++
			return errors.New("disk full")
		},
	})
	if err == nil || report == nil || report.Status != "failed" || errorCode(err) != "PROGRESS_CALLBACK_FAILED" {
		t.Fatalf("checkpoint failure did not stop import: report=%+v err=%v", report, err)
	}
	if fake.writes != 0 || callbacks != 1 {
		t.Fatalf("mutation occurred despite failed initial checkpoint: writes=%d callbacks=%d", fake.writes, callbacks)
	}
}

func TestImportReportsAmbiguousUploadForExplicitReconciliation(t *testing.T) {
	archive := datedArchive(t)
	fake := newFakeImmich()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.TrimPrefix(r.URL.Path, "/api") == "/assets" {
			_, _, _, checksum, data, err := readMultipart(r)
			if err != nil {
				http.Error(w, "bad upload", http.StatusBadRequest)
				return
			}
			s1 := sha1.Sum(data)
			s256 := sha256.Sum256(data)
			fake.mu.Lock()
			fake.assets[fakeAssetID] = fakeAsset{id: fakeAssetID, sha1: base64.StdEncoding.EncodeToString(s1[:]), sha256: hex.EncodeToString(s256[:]), bytes: data, created: "2023-11-14T22:13:20Z", modified: "2023-11-14T22:13:20Z", owner: fakeAccountID}
			fake.bySHA1[checksum] = fakeAssetID
			fake.mu.Unlock()
			// Simulate a response lost after Immich has stored the original.
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("httptest server does not support connection hijacking")
				return
			}
			conn, _, hijackErr := hijacker.Hijack()
			if hijackErr != nil {
				t.Errorf("hijack upload response: %v", hijackErr)
				return
			}
			_ = conn.Close()
			return
		}
		fake.serveHTTP(w, r)
	}))
	defer server.Close()
	report, err := Import(context.Background(), archive, Options{ServerURL: server.URL, APIKey: testAPIKey, AllowLoopbackHTTP: true})
	if err == nil || report == nil || report.Status != "needs_reconciliation" || report.Contents[0].State != "unknown" || errorCode(err) != "NETWORK_ERROR" {
		t.Fatalf("ambiguous upload was reported as a safe retry: report=%+v err=%v", report, err)
	}
}

func TestResumeBindsArchiveOriginVersionAccountAndReportInventory(t *testing.T) {
	archive := datedArchive(t)
	plan, err := Plan(context.Background(), archive, false)
	if err != nil {
		t.Fatal(err)
	}
	previous := reportFromPlan(plan, "import", "interrupted")
	previous.ServerOrigin, previous.ServerVersion, previous.AccountID = "https://immich.example", SupportedServerVersion, fakeAccountID
	previous.Files[0].RemoteAssetID = fakeAssetID
	previous.Files[0].State = "unknown"
	previous.Contents[0].RemoteAssetID = fakeAssetID
	previous.Contents[0].State = "unknown"
	previous.Albums[0].RemoteAlbumID = fakeAlbumID
	previous.Albums[0].OwnerID = fakeAccountID
	current := reportFromPlan(plan, "import", "in_progress")
	current.ServerOrigin, current.ServerVersion, current.AccountID = previous.ServerOrigin, previous.ServerVersion, previous.AccountID
	if err := validateResume(previous, current, plan); err != nil {
		t.Fatalf("valid same-source report rejected: %v", err)
	}
	previous.Files[0].SourceEntry = "Takeout/other.png"
	if err := validateResume(previous, current, plan); !errors.Is(err, ErrResumeMismatch) {
		t.Fatalf("hostile resume occurrence was accepted: %v", err)
	}
}

func TestClientNormalizesOriginAndRejectsHTTPExceptLiteralOptedLoopback(t *testing.T) {
	for _, tc := range []struct {
		url     string
		allow   bool
		want    string
		wantErr bool
	}{
		{url: "https://EXAMPLE.invalid:443/", want: "https://example.invalid"},
		{url: "https://EXAMPLE.invalid:443/api", want: "https://example.invalid"},
		{url: "https://example.invalid/v1", wantErr: true},
		{url: "http://127.0.0.1:2283", allow: true, want: "http://127.0.0.1:2283"},
		{url: "http://localhost:2283", allow: true, wantErr: true},
		{url: "http://127.0.0.1:2283", wantErr: true},
		{url: "https://user:pass@example.invalid", wantErr: true},
	} {
		got, err := normalizeServerOrigin(tc.url, tc.allow)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeServerOrigin(%q) unexpectedly succeeded", tc.url)
			}
		} else if err != nil || got != tc.want {
			t.Errorf("normalizeServerOrigin(%q)=%q,%v want %q", tc.url, got, err, tc.want)
		}
	}
}

func TestAlbumMarkerDoesNotMatchForeignOrTitleOnlyAlbum(t *testing.T) {
	planned := []AlbumResult{{SourceAlbumID: "source-album-id", Title: "Family", Marker: albumMarker("source-album-id")}}
	foreign := remoteAlbumDTO{ID: fakeAlbumID, Name: "Family", Description: planned[0].Marker, AlbumUsers: []remoteAlbumUser{{Role: "owner"}}}
	foreign.AlbumUsers[0].User.ID = "99999999-9999-4999-8999-999999999999"
	byTitle := remoteAlbumDTO{ID: "44444444-4444-4444-8444-444444444444", Name: "Family", Description: "a user album", AlbumUsers: []remoteAlbumUser{{Role: "owner"}}}
	byTitle.AlbumUsers[0].User.ID = fakeAccountID
	resolved, err := resolveAlbums([]remoteAlbumDTO{foreign, byTitle}, planned, fakeAccountID)
	if err != nil || len(resolved) != 1 || resolved[0].remoteID != "" {
		t.Fatalf("foreign marker/title-only album was reused: %+v, %v", resolved, err)
	}
}
