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
	"fmt"
	"hash"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Pastalikek65/archivebridge/internal/bridge"
)

const (
	maxUploadBytes  = int64(32 << 30)
	maxImportItems  = 100_000
	maxDateXMPBytes = 2048
)

// Import transfers a verified portable archive to one explicitly selected
// Immich server. It performs all local and remote read-only checks before its
// first write, never updates or deletes existing remote metadata, and returns
// a partial report whenever a mutation may have reached the server.
func Import(ctx context.Context, archive string, opts Options) (*Report, error) {
	plan, err := Plan(ctx, archive, opts.SkipUnresolved)
	if err != nil {
		if isContextStop(err) {
			return interruptedReport(reportFromPlan(nil, "import", "interrupted"), err)
		}
		if errors.Is(err, ErrUnresolved) {
			return reportFromPlan(plan, "import", "blocked"), err
		}
		return nil, err
	}
	if plan.Status == "blocked" {
		return reportFromPlan(plan, "import", "blocked"), &Error{Code: "UNRESOLVED_METADATA", Message: "Archive has media with unresolved source dates; explicitly skip unresolved items to continue.", cause: ErrUnresolved}
	}
	if err := ctx.Err(); err != nil {
		return reportFromPlan(plan, "import", "interrupted"), localError("CANCELED", "Import was canceled before remote access.", err)
	}
	base, err := filepathAbs(archive)
	if err != nil {
		return nil, localError("ARCHIVE_INVALID", "Portable archive could not be resolved safely.", err)
	}
	if len(plan.Files) > maxImportItems || len(plan.Sidecars) > maxImportItems || len(plan.Albums) > maxImportItems {
		return nil, &Error{Code: "ARCHIVE_LIMIT", Message: "Portable archive exceeds the configured import item bound."}
	}

	report := reportFromPlan(plan, "import", "in_progress")
	identities := make(map[string]localContentIdentity, plan.UniqueContents)
	for _, file := range plan.Files {
		if file.State != "ready" {
			continue
		}
		if _, ok := identities[file.SHA256]; ok {
			continue
		}
		identity, hashErr := hashPortableMedia(ctx, base, file.OutputPath, file.SHA256, file.Bytes)
		if hashErr != nil {
			if isContextStop(hashErr) {
				return interruptedReport(report, hashErr)
			}
			report.Status = "failed"
			report.Issues = append(report.Issues, Issue{Code: "LOCAL_MEDIA_CHANGED", Details: "A local media file changed or failed its manifest hash check.", OccurrenceID: file.OccurrenceID, SourceEntry: file.SourceEntry})
			return report, localError("LOCAL_MEDIA_CHANGED", "A local media file changed or failed its manifest hash check.", hashErr)
		}
		identities[file.SHA256] = identity
	}
	if err := verifyLocalSnapshot(ctx, base, plan); err != nil {
		if isContextStop(err) {
			return interruptedReport(report, err)
		}
		report.Status = "failed"
		return report, localError("ARCHIVE_CHANGED", "Portable archive changed during import preflight.", err)
	}
	readyCount := 0
	for _, file := range report.Files {
		if file.State != "skipped" {
			readyCount++
		}
	}
	hasEmptyAlbum := false
	for _, album := range report.Albums {
		if len(album.OccurrenceIDs) == 0 {
			hasEmptyAlbum = true
			break
		}
	}
	if readyCount == 0 && !hasEmptyAlbum {
		for i := range report.Contents {
			report.Contents[i].State = "skipped"
		}
		for i := range report.Albums {
			report.Albums[i].State = "skipped"
			for _, occurrenceID := range report.Albums[i].OccurrenceIDs {
				if file := occurrenceByID(report.Files, occurrenceID); file != nil {
					addMembership(report, &report.Albums[i], file, "skipped")
				}
			}
		}
		if plan.UnresolvedCount > 0 {
			report.Status = "completed_with_skips"
		} else {
			report.Status = "completed"
		}
		if err := emitProgress(opts.Progress, *report); err != nil {
			return failReport(report, callbackError(err))
		}
		return report, nil
	}

	client, origin, err := newAPIClient(opts)
	if err != nil {
		report.Status = "failed"
		return report, err
	}
	serverVersion, accountID, err := client.preflight(ctx)
	if err != nil {
		if isContextStop(err) {
			return interruptedReport(report, err)
		}
		report.Status = "failed"
		report.Issues = append(report.Issues, Issue{Code: errorCode(err), Details: "Immich read-only preflight failed before any remote mutation."})
		return report, err
	}
	report.ServerOrigin, report.ServerVersion, report.AccountID = origin, serverVersion, accountID
	if opts.Resume != nil {
		if err := validateResume(opts.Resume, report, plan); err != nil {
			report.Status = "failed"
			return report, err
		}
		mergeResumeHints(report, opts.Resume)
	}

	// Establish all read-only remote facts before writes: album ownership/marker
	// identity, current exact membership, duplicate hints, and full original
	// verification for every reusable content object.
	albums, err := client.getAlbums(ctx)
	if err != nil {
		return failReport(report, err)
	}
	remoteAlbums, err := resolveAlbums(albums, report.Albums, accountID)
	if err != nil {
		return failReport(report, err)
	}
	if opts.Resume != nil {
		previousAlbums := make(map[string]string, len(opts.Resume.Albums))
		for _, album := range opts.Resume.Albums {
			previousAlbums[album.SourceAlbumID] = album.RemoteAlbumID
		}
		for i := range report.Albums {
			previousID := previousAlbums[report.Albums[i].SourceAlbumID]
			if previousID != "" && (i >= len(remoteAlbums) || !strings.EqualFold(previousID, remoteAlbums[i].remoteID)) {
				return failReport(report, &Error{Code: "RESUME_ALBUM_MISSING", Message: "A source-owned album from the resume report no longer matches its exact source marker; v1 will not create a replacement automatically.", cause: ErrResumeMismatch})
			}
		}
	}
	for i := range report.Contents {
		content := &report.Contents[i]
		identity, ok := identities[content.SHA256]
		if !ok {
			content.State = "skipped"
			continue
		}
		if opts.Resume != nil {
			if old := previousContent(opts.Resume, content.SHA256); old != nil && old.RemoteAssetID != "" {
				content.RemoteAssetID = old.RemoteAssetID
				content.State = "in_flight"
				appendStateToOccurrences(report, content)
				if err := emitProgress(opts.Progress, *report); err != nil {
					return failReport(report, callbackError(err))
				}
				asset, verifyErr := client.verifyRemoteAsset(ctx, old.RemoteAssetID, *content, identity, accountID, dateForContent(report.Files, content.OccurrenceIDs))
				if verifyErr != nil {
					if isContextStop(verifyErr) {
						return interruptedReport(report, verifyErr)
					}
					content.State = "failed"
					appendStateToOccurrences(report, content)
					return failReport(report, verifyErr)
				}
				content.RemoteAssetID = asset.ID
				content.State = "reused"
				content.OriginalSHA256Verified = true
				content.VerifiedOriginalBytes = identity.bytes
				appendStateToOccurrences(report, content)
				continue
			}
		}
		candidateID := deterministicAssetID(plan.PlanID, content.SHA256)
		check, checkErr := client.bulkUploadCheck(ctx, candidateID, identity.sha1)
		if checkErr != nil {
			return failReport(report, checkErr)
		}
		if check.Action == "reject" {
			if check.Reason == "unsupported-format" {
				content.State = "failed"
				report.Issues = append(report.Issues, Issue{Code: "UNSUPPORTED_REMOTE_FORMAT", Details: "Immich rejected this media format before upload.", OccurrenceID: firstOccurrence(content.OccurrenceIDs)})
				return failReport(report, &Error{Code: "UNSUPPORTED_REMOTE_FORMAT", Message: "Immich rejected a media file format before upload."})
			}
			content.RemoteAssetID = check.AssetID
			content.State = "in_flight"
			appendStateToOccurrences(report, content)
			if err := emitProgress(opts.Progress, *report); err != nil {
				return failReport(report, callbackError(err))
			}
			asset, verifyErr := client.verifyRemoteAsset(ctx, check.AssetID, *content, identity, accountID, dateForContent(report.Files, content.OccurrenceIDs))
			if verifyErr != nil {
				if isContextStop(verifyErr) {
					return interruptedReport(report, verifyErr)
				}
				content.State = "failed"
				appendStateToOccurrences(report, content)
				return failReport(report, verifyErr)
			}
			content.RemoteAssetID = asset.ID
			content.State = "reused"
			content.OriginalSHA256Verified = true
			content.VerifiedOriginalBytes = identity.bytes
			appendStateToOccurrences(report, content)
		}
	}
	for i := range remoteAlbums {
		if remoteAlbums[i].remoteID == "" {
			continue
		}
		members, memberErr := client.searchAlbumMembers(ctx, remoteAlbums[i].remoteID)
		if memberErr != nil {
			return failReport(report, memberErr)
		}
		expected := make(map[string]bool)
		album := &report.Albums[remoteAlbums[i].index]
		for _, occurrenceID := range album.OccurrenceIDs {
			file := occurrenceByID(report.Files, occurrenceID)
			if file == nil || file.State == "skipped" {
				continue
			}
			content := contentForOccurrence(report.Contents, occurrenceID)
			if content != nil && content.RemoteAssetID != "" {
				expected[strings.ToLower(content.RemoteAssetID)] = true
			}
		}
		for _, memberID := range members {
			if !expected[memberID] {
				return failReport(report, &Error{Code: "ALBUM_MEMBERSHIP_CONFLICT", Message: "A source-owned Immich album contains an unexpected asset; v1 will not remove or alter it."})
			}
		}
		for id := range expected {
			remoteAlbums[i].existingMembers[id] = true
		}
	}

	if err := emitProgress(opts.Progress, *report); err != nil {
		return failReport(report, callbackError(err))
	}

	// Upload one representative of each not-yet-present SHA-256 group. The
	// candidate UUID is deterministic for the plan/content pair, making explicit
	// resume able to reconcile a response lost after the server accepted bytes.
	for i := range report.Contents {
		content := &report.Contents[i]
		if content.State == "reused" || content.State == "skipped" {
			continue
		}
		identity := identities[content.SHA256]
		occurrence := occurrenceByID(report.Files, firstReadyOccurrence(report.Files, content.OccurrenceIDs))
		if occurrence == nil {
			return failReport(report, &Error{Code: "REPORT_INVALID", Message: "Import report lost a source occurrence."})
		}
		content.State = "in_flight"
		if err := emitProgress(opts.Progress, *report); err != nil {
			content.State = "pending"
			return failReport(report, callbackError(err))
		}
		asset, uploadErr := client.upload(ctx, base, *occurrence, identity)
		if uploadErr != nil {
			if isAmbiguousMutation(uploadErr) {
				content.State = "unknown"
				report.Status = "needs_reconciliation"
				appendStateToOccurrences(report, content)
				_ = emitProgress(opts.Progress, *report)
				return report, uploadErr
			}
			content.State = "failed"
			appendStateToOccurrences(report, content)
			return failReport(report, uploadErr)
		}
		content.RemoteAssetID = asset.ID
		content.State = "in_flight"
		appendStateToOccurrences(report, content)
		if err := emitProgress(opts.Progress, *report); err != nil {
			return failReport(report, callbackError(err))
		}
		remoteAsset, verifyErr := client.verifyRemoteAsset(ctx, asset.ID, *content, identity, accountID, dateForContent(report.Files, content.OccurrenceIDs))
		if verifyErr != nil {
			if isContextStop(verifyErr) {
				content.State = "in_flight"
				appendStateToOccurrences(report, content)
				return interruptedReport(report, verifyErr)
			}
			content.State = "failed"
			appendStateToOccurrences(report, content)
			return failReport(report, verifyErr)
		}
		content.RemoteAssetID = remoteAsset.ID
		content.State = "uploaded"
		content.OriginalSHA256Verified = true
		content.VerifiedOriginalBytes = identity.bytes
		appendStateToOccurrences(report, content)
		if err := emitProgress(opts.Progress, *report); err != nil {
			return failReport(report, callbackError(err))
		}
	}

	// Create only albums with at least one selected occurrence. Existing albums
	// are reused only when the exact source-ID marker is owned by this account.
	for i := range report.Albums {
		album := &report.Albums[i]
		if album.State == "skipped" {
			continue
		}
		entry := remoteAlbumFor(remoteAlbums, i)
		if entry == nil {
			return failReport(report, &Error{Code: "ALBUM_STATE_INVALID", Message: "Album reconciliation state is missing."})
		}
		if entry.remoteID != "" {
			album.RemoteAlbumID, album.OwnerID, album.State = entry.remoteID, accountID, "reused"
			continue
		}
		if !albumHasReadyMembers(report.Files, album.OccurrenceIDs) {
			album.State = "skipped"
			continue
		}
		album.State = "in_flight"
		if err := emitProgress(opts.Progress, *report); err != nil {
			album.State = "pending"
			return failReport(report, callbackError(err))
		}
		created, createErr := client.createAlbum(ctx, album.Title, album.Marker)
		if createErr != nil {
			if isAmbiguousMutation(createErr) {
				album.State = "unknown"
				report.Status = "needs_reconciliation"
				_ = emitProgress(opts.Progress, *report)
				return report, createErr
			}
			album.State = "failed"
			return failReport(report, createErr)
		}
		if !validUUID(created.ID) || created.Name != album.Title || created.Description != album.Marker {
			album.State = "failed"
			return failReport(report, &Error{Code: "ALBUM_CREATE_INVALID", Message: "Immich created an album with unexpected identity metadata."})
		}
		album.RemoteAlbumID, album.State = strings.ToLower(created.ID), "in_flight"
		if err := emitProgress(opts.Progress, *report); err != nil {
			return failReport(report, callbackError(err))
		}
		refreshed, refreshErr := client.getAlbums(ctx)
		if refreshErr != nil {
			album.State = "unknown"
			report.Status = "needs_reconciliation"
			return report, refreshErr
		}
		ownerOK := false
		for _, candidate := range refreshed {
			if strings.EqualFold(candidate.ID, created.ID) && candidate.Description == album.Marker && candidate.Name == album.Title && strings.EqualFold(candidate.AlbumUsers[0].User.ID, accountID) {
				ownerOK = true
				break
			}
		}
		if !ownerOK {
			album.State = "failed"
			return failReport(report, &Error{Code: "ALBUM_OWNER_UNVERIFIED", Message: "Created album ownership could not be verified safely."})
		}
		album.RemoteAlbumID, album.OwnerID, album.State = strings.ToLower(created.ID), accountID, "created"
		if err := emitProgress(opts.Progress, *report); err != nil {
			return failReport(report, callbackError(err))
		}
	}

	// Add only absent memberships, then re-query and require an exact set. Never
	// issue a removal to repair an album containing unexpected assets.
	for albumIndex := range report.Albums {
		album := &report.Albums[albumIndex]
		if album.State == "skipped" || album.RemoteAlbumID == "" {
			continue
		}
		entry := remoteAlbumFor(remoteAlbums, albumIndex)
		existing := map[string]bool{}
		if entry != nil {
			for id := range entry.existingMembers {
				existing[id] = true
			}
		}
		missing := make([]string, 0)
		missingSet := map[string]bool{}
		wanted := make(map[string]bool)
		for _, occurrenceID := range album.OccurrenceIDs {
			file := occurrenceByID(report.Files, occurrenceID)
			if file == nil {
				return failReport(report, &Error{Code: "REPORT_INVALID", Message: "Album references a missing source occurrence."})
			}
			if file.State == "skipped" {
				addMembership(report, album, file, "skipped")
				continue
			}
			content := contentForOccurrence(report.Contents, occurrenceID)
			if content == nil || content.RemoteAssetID == "" {
				return failReport(report, &Error{Code: "REMOTE_ASSET_MISSING", Message: "A source album member has no verified remote asset."})
			}
			assetID := strings.ToLower(content.RemoteAssetID)
			wanted[assetID] = true
			addMembership(report, album, file, "pending")
			if !existing[assetID] && !missingSet[assetID] {
				missing = append(missing, assetID)
				missingSet[assetID] = true
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			for _, id := range missing {
				setMembershipState(report, album.SourceAlbumID, id, "in_flight")
			}
			if err := emitProgress(opts.Progress, *report); err != nil {
				setMembershipStates(report, album, missing, "pending")
				return failReport(report, callbackError(err))
			}
			if err := client.addAlbumAssets(ctx, album.RemoteAlbumID, missing); err != nil {
				if isAmbiguousMutation(err) {
					setMembershipStates(report, album, missing, "unknown")
					report.Status = "needs_reconciliation"
					_ = emitProgress(opts.Progress, *report)
					return report, err
				}
				setMembershipStates(report, album, missing, "failed")
				return failReport(report, err)
			}
			for _, occurrenceID := range album.OccurrenceIDs {
				file := occurrenceByID(report.Files, occurrenceID)
				if file == nil || file.State == "skipped" {
					continue
				}
				content := contentForOccurrence(report.Contents, occurrenceID)
				if content != nil && !existing[strings.ToLower(content.RemoteAssetID)] {
					setMembershipState(report, album.SourceAlbumID, content.RemoteAssetID, "added")
				}
			}
			if err := emitProgress(opts.Progress, *report); err != nil {
				return failReport(report, callbackError(err))
			}
		}
		actual, err := client.searchAlbumMembers(ctx, album.RemoteAlbumID)
		if err != nil {
			return failReport(report, err)
		}
		if !equalStringSet(actual, wanted) {
			return failReport(report, &Error{Code: "ALBUM_MEMBERSHIP_VERIFY_FAILED", Message: "Immich album membership did not match the selected source relationships after import."})
		}
		for _, occurrenceID := range album.OccurrenceIDs {
			file := occurrenceByID(report.Files, occurrenceID)
			if file == nil || file.State == "skipped" {
				continue
			}
			content := contentForOccurrence(report.Contents, occurrenceID)
			state := "added"
			if existing[strings.ToLower(content.RemoteAssetID)] {
				state = "already_present"
			}
			addMembership(report, album, file, state)
		}
		album.State = "verified"
	}

	report.Status = "completed"
	for _, file := range report.Files {
		if file.State == "skipped" {
			report.Status = "completed_with_skips"
			break
		}
	}
	if err := emitProgress(opts.Progress, *report); err != nil {
		return failReport(report, callbackError(err))
	}
	return report, nil
}

// VerifyRemote rechecks a prior import report against the same verified local
// archive and authenticated account. It performs no remote mutations.
func VerifyRemote(ctx context.Context, archive string, opts Options, prior Report) (*Report, error) {
	skipUnresolved := opts.SkipUnresolved || reportHasSkippedFile(prior.Files)
	plan, err := Plan(ctx, archive, skipUnresolved)
	if err != nil {
		if isContextStop(err) {
			return interruptedReport(reportFromPlan(nil, "verify", "interrupted"), err)
		}
		return nil, err
	}
	if plan.Status == "blocked" {
		return reportFromPlan(plan, "verify", "blocked"), &Error{Code: "UNRESOLVED_METADATA", Message: "Archive has unresolved media metadata outside the selected import scope.", cause: ErrUnresolved}
	}
	base, err := filepathAbs(archive)
	if err != nil {
		return nil, localError("ARCHIVE_INVALID", "Portable archive could not be resolved safely.", err)
	}
	report := reportFromPlan(plan, "verify", "in_progress")
	client, origin, err := newAPIClient(opts)
	if err != nil {
		return nil, err
	}
	version, account, err := client.preflight(ctx)
	if err != nil {
		return failReport(report, err)
	}
	if origin != prior.ServerOrigin || version != prior.ServerVersion || account != prior.AccountID {
		return nil, &Error{Code: "REPORT_SERVER_MISMATCH", Message: "Prior report belongs to a different Immich origin, version, or account.", cause: ErrResumeMismatch}
	}
	report.ServerOrigin, report.ServerVersion, report.AccountID = origin, version, account
	if err := validateResume(&prior, report, plan); err != nil {
		return nil, err
	}
	identities := make(map[string]localContentIdentity, len(plan.Files))
	contents := map[string]*ContentResult{}
	for i := range prior.Contents {
		contents[prior.Contents[i].SHA256] = &prior.Contents[i]
	}
	verification := &RemoteVerification{Status: "verified"}
	checkedContents := map[string]bool{}
	for _, file := range plan.Files {
		if file.State == "skipped" {
			verification.Status = "verified_with_skips"
			continue
		}
		if checkedContents[file.SHA256] {
			continue
		}
		identity, ok := identities[file.SHA256]
		if !ok {
			identity, err = hashPortableMedia(ctx, base, file.OutputPath, file.SHA256, file.Bytes)
			if err != nil {
				return failReport(report, localError("LOCAL_MEDIA_CHANGED", "A local media file failed its manifest hash check.", err))
			}
			identities[file.SHA256] = identity
		}
		content := contents[file.SHA256]
		if content == nil || !validUUID(content.RemoteAssetID) {
			return failReport(report, &Error{Code: "REMOTE_ASSET_MISSING", Message: "Prior report has no verified remote asset identity for selected content."})
		}
		asset, verifyErr := client.verifyRemoteAsset(ctx, content.RemoteAssetID, *content, identity, account, file.Date)
		if verifyErr != nil {
			verification.Status = "failed"
			return failReport(report, verifyErr)
		}
		_ = asset
		checkedContents[file.SHA256] = true
		verification.ContentsChecked++
		verification.BytesChecked += identity.bytes
	}
	albums, err := client.getAlbums(ctx)
	if err != nil {
		return failReport(report, err)
	}
	remoteAlbums, err := resolveAlbums(albums, prior.Albums, account)
	if err != nil {
		return failReport(report, err)
	}
	for index, album := range prior.Albums {
		if album.State == "skipped" || album.RemoteAlbumID == "" {
			continue
		}
		entry := remoteAlbumFor(remoteAlbums, index)
		if entry == nil || entry.remoteID == "" || !strings.EqualFold(entry.remoteID, album.RemoteAlbumID) {
			return failReport(report, &Error{Code: "REMOTE_ALBUM_MISSING", Message: "A source album marker no longer resolves to the report's owned Immich album."})
		}
		actual, err := client.searchAlbumMembers(ctx, album.RemoteAlbumID)
		if err != nil {
			return failReport(report, err)
		}
		wanted := make(map[string]bool)
		for _, occurrenceID := range album.OccurrenceIDs {
			if occurrenceByID(prior.Files, occurrenceID).State == "skipped" {
				continue
			}
			content := contentForOccurrence(prior.Contents, occurrenceID)
			if content == nil || content.RemoteAssetID == "" {
				return failReport(report, &Error{Code: "REPORT_INVALID", Message: "Prior report lost an album member asset identity."})
			}
			wanted[strings.ToLower(content.RemoteAssetID)] = true
		}
		if !equalStringSet(actual, wanted) {
			return failReport(report, &Error{Code: "ALBUM_MEMBERSHIP_VERIFY_FAILED", Message: "Remote source album membership does not match the prior report."})
		}
		verification.AlbumsChecked++
		verification.MembershipsChecked += len(wanted)
	}
	verification.Status = "verified"
	if plan.Status == "ready_with_skips" || prior.Status == "completed_with_skips" {
		verification.Status = "verified_with_skips"
	}
	verifiedReport := cloneReport(prior)
	verifiedReport.Mode = "verify"
	verifiedReport.Status = verification.Status
	verifiedReport.Verification = verification
	verifiedReport.Issues = append([]Issue{}, report.Issues...)
	return &verifiedReport, nil
}

func reportFromPlan(plan *PlanReport, mode, status string) *Report {
	if plan == nil {
		return &Report{SchemaVersion: ReportSchemaVersion, Mode: mode, Status: status, DateTransferPolicy: dateTransferPolicy, Files: []OccurrenceResult{}, Sidecars: []PlannedSidecar{}, Contents: []ContentResult{}, Albums: []AlbumResult{}, Memberships: []MembershipResult{}, Issues: []Issue{}}
	}
	r := &Report{
		SchemaVersion: ReportSchemaVersion, Mode: mode, Status: status,
		PlanID: plan.PlanID, ManifestSHA256: plan.ManifestSHA256,
		DateTransferPolicy:   plan.DateTransferPolicy,
		FileModifiedAtNotice: plan.FileModifiedAtNotice,
		Files:                []OccurrenceResult{}, Sidecars: append([]PlannedSidecar{}, plan.Sidecars...),
		Contents: []ContentResult{}, Albums: []AlbumResult{}, Memberships: []MembershipResult{}, Issues: append([]Issue{}, plan.Issues...),
	}
	bySHA := map[string]*ContentResult{}
	contentHasReadyOccurrence := map[string]bool{}
	for _, file := range plan.Files {
		state := "pending"
		if file.State == "skipped" || file.State == "blocked" {
			state = "skipped"
		}
		r.Files = append(r.Files, OccurrenceResult{OccurrenceID: file.OccurrenceID, SourceEntry: file.SourceEntry, OutputPath: file.OutputPath, SHA256: file.SHA256, Bytes: file.Bytes, Date: file.Date, SourceAlbumIDs: append([]string{}, file.SourceAlbumIDs...), State: state, Reason: file.Reason})
		content := bySHA[file.SHA256]
		if content == nil {
			content = &ContentResult{SHA256: file.SHA256, Bytes: file.Bytes, OccurrenceIDs: []string{}, State: "pending"}
			bySHA[file.SHA256] = content
		}
		content.OccurrenceIDs = append(content.OccurrenceIDs, file.OccurrenceID)
		if file.State != "skipped" && file.State != "blocked" {
			contentHasReadyOccurrence[file.SHA256] = true
		}
	}
	shas := make([]string, 0, len(bySHA))
	for sha := range bySHA {
		shas = append(shas, sha)
	}
	sort.Strings(shas)
	for _, sha := range shas {
		content := bySHA[sha]
		sort.Strings(content.OccurrenceIDs)
		if !contentHasReadyOccurrence[sha] {
			content.State = "skipped"
		}
		r.Contents = append(r.Contents, *content)
	}
	for _, album := range plan.Albums {
		state := "pending"
		if album.State == "skipped" {
			state = "skipped"
		}
		r.Albums = append(r.Albums, AlbumResult{SourceAlbumID: album.SourceAlbumID, Title: album.Title, Folder: album.Folder, Marker: albumMarker(album.SourceAlbumID), OccurrenceIDs: append([]string{}, album.OccurrenceIDs...), State: state})
	}
	for i := range r.Albums {
		album := &r.Albums[i]
		for _, occurrenceID := range album.OccurrenceIDs {
			if file := occurrenceByID(r.Files, occurrenceID); file != nil {
				state := "pending"
				if file.State == "skipped" {
					state = "skipped"
				}
				r.Memberships = append(r.Memberships, MembershipResult{SourceAlbumID: album.SourceAlbumID, OccurrenceID: occurrenceID, State: state})
			}
		}
	}
	return r
}

func (c *apiClient) verifyRemoteAsset(ctx context.Context, id string, content ContentResult, local localContentIdentity, account string, expectedDate string) (assetDTO, error) {
	if err := c.waitForUploadedMetadata(ctx, id, account, expectedDate, local.sha1); err != nil {
		return assetDTO{}, err
	}
	asset, err := c.getAsset(ctx, id)
	if err != nil {
		return assetDTO{}, err
	}
	if err := validateRemoteAssetFacts(asset, id, account, expectedDate, local.sha1, true); err != nil {
		return assetDTO{}, err
	}
	checksum, err := base64Decode(asset.Checksum)
	if err != nil || !bytes.Equal(checksum, mustDecodeSHA1(local.sha1)) {
		return assetDTO{}, &Error{Code: "REMOTE_ASSET_CHECKSUM_MISMATCH", Message: "Immich asset metadata checksum does not match the selected source bytes."}
	}
	resp, err := c.request(ctx, http.MethodGet, "/assets/"+url.PathEscape(id)+"/original", nil, "", http.StatusOK)
	if err != nil {
		return assetDTO{}, err
	}
	h := sha256.New()
	n, copyErr := io.Copy(h, io.LimitReader(resp.Body, local.bytes+1))
	closeErr := resp.Body.Close()
	if copyErr != nil || closeErr != nil || n != local.bytes || hex.EncodeToString(h.Sum(nil)) != content.SHA256 {
		return assetDTO{}, &Error{Code: "REMOTE_ORIGINAL_MISMATCH", Message: "Downloaded Immich original does not match the complete source SHA-256 and byte count."}
	}
	// Metadata workers can finish or drift while the original is being read.
	// Re-fetch after the streaming hash so verification describes one stable
	// account/date/checksum identity before any album writes are allowed.
	after, err := c.getAsset(ctx, id)
	if err != nil {
		return assetDTO{}, err
	}
	if err := validateRemoteAssetFacts(after, id, account, expectedDate, local.sha1, true); err != nil {
		return assetDTO{}, err
	}
	return after, nil
}

func validateRemoteAssetFacts(asset assetDTO, id, account, expectedDate, expectedSHA1 string, requireExifDate bool) error {
	if !strings.EqualFold(asset.ID, id) || !strings.EqualFold(asset.OwnerID, account) || asset.IsTrashed {
		return &Error{Code: "REMOTE_ASSET_OWNER_MISMATCH", Message: "Remote media is not an active asset owned by the authenticated account."}
	}
	created, createdErr := time.Parse(time.RFC3339Nano, asset.FileCreatedAt)
	modified, modifiedErr := time.Parse(time.RFC3339Nano, asset.FileModifiedAt)
	wanted, wantedErr := time.Parse(time.RFC3339Nano, expectedDate)
	if createdErr != nil || modifiedErr != nil || wantedErr != nil || !created.Equal(wanted) || !modified.Equal(wanted) {
		return &Error{Code: "REMOTE_ASSET_DATE_MISMATCH", Message: "Remote media date does not match the unambiguous source date; v1 will not update existing metadata."}
	}
	if asset.ExifInfo != nil && asset.ExifInfo.DateTimeOriginal != "" {
		exifDate, exifErr := time.Parse(time.RFC3339Nano, asset.ExifInfo.DateTimeOriginal)
		if exifErr != nil || !exifDate.Equal(wanted) {
			return &Error{Code: "REMOTE_ASSET_DATE_MISMATCH", Message: "Remote media metadata date does not match the unambiguous source date; v1 will not update existing metadata."}
		}
	} else if requireExifDate {
		return &Error{Code: "REMOTE_METADATA_PENDING", Message: "Immich has not finished extracting the generated source date metadata."}
	}
	checksum, err := base64Decode(asset.Checksum)
	if err != nil || len(checksum) != sha1.Size || (expectedSHA1 != "" && !bytes.Equal(checksum, mustDecodeSHA1(expectedSHA1))) {
		return &Error{Code: "REMOTE_ASSET_CHECKSUM_MISMATCH", Message: "Immich asset metadata checksum does not match the selected source bytes."}
	}
	return nil
}

func (c *apiClient) waitForUploadedMetadata(ctx context.Context, id, account, expectedDate, expectedSHA1 string) error {
	return c.waitForUploadedMetadataWithBounds(ctx, id, account, expectedDate, expectedSHA1, remoteMetadataWait, remoteMetadataPoll)
}

func (c *apiClient) waitForUploadedMetadataWithBounds(ctx context.Context, id, account, expectedDate, expectedSHA1 string, waitLimit, pollInterval time.Duration) error {
	if waitLimit <= 0 || pollInterval <= 0 {
		return &Error{Code: "INVALID_METADATA_WAIT", Message: "Metadata wait bounds must be positive."}
	}
	waitCtx, cancel := context.WithTimeout(ctx, waitLimit)
	defer cancel()
	if _, err := time.Parse(time.RFC3339Nano, expectedDate); err != nil {
		return &Error{Code: "UPLOAD_DATE_INVALID", Message: "Selected upload date is invalid."}
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return localError("CANCELED", "Import was interrupted while waiting for Immich metadata processing.", ctx.Err())
			}
			return &Error{Code: "REMOTE_METADATA_TIMEOUT", Message: "Immich did not expose the processed source date before the bounded wait expired."}
		case <-timer.C:
		}
		asset, getErr := c.getAsset(waitCtx, id)
		if getErr != nil {
			if isContextStop(getErr) {
				if ctx.Err() != nil {
					return localError("CANCELED", "Import was interrupted while waiting for Immich metadata processing.", ctx.Err())
				}
				if waitCtx.Err() != nil {
					return &Error{Code: "REMOTE_METADATA_TIMEOUT", Message: "Immich did not expose the processed source date before the bounded wait expired."}
				}
				return getErr
			}
			return getErr
		}
		if !strings.EqualFold(asset.OwnerID, account) || asset.IsTrashed {
			return &Error{Code: "REMOTE_ASSET_OWNER_MISMATCH", Message: "Uploaded media is not an active asset owned by the authenticated account."}
		}
		if asset.HasMetadata {
			validationErr := validateRemoteAssetFacts(asset, id, account, expectedDate, expectedSHA1, true)
			if validationErr == nil {
				return nil
			}
			var validation *Error
			if !errors.As(validationErr, &validation) || validation.Code != "REMOTE_METADATA_PENDING" {
				return validationErr
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(pollInterval)
	}
}

func generatedDateXMP(date time.Time) ([]byte, error) {
	value := date.UTC().Format(time.RFC3339Nano)
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return nil, err
	}
	escaped := new(bytes.Buffer)
	if err := xml.EscapeText(escaped, []byte(value)); err != nil {
		return nil, err
	}
	stamp := escaped.String()
	var xmp strings.Builder
	xmp.WriteString(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description rdf:about="" xmlns:xmp="http://ns.adobe.com/xap/1.0/" xmlns:exif="http://ns.adobe.com/exif/1.0/" xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/" xmp:CreateDate="`)
	xmp.WriteString(stamp)
	xmp.WriteString(`" exif:DateTimeOriginal="`)
	xmp.WriteString(stamp)
	xmp.WriteString(`" photoshop:DateCreated="`)
	xmp.WriteString(stamp)
	xmp.WriteString(`"/></rdf:RDF></x:xmpmeta>`)
	generated := []byte(xmp.String())
	if len(generated) > maxDateXMPBytes {
		return nil, errors.New("generated date sidecar exceeds its bound")
	}
	return generated, nil
}

func (c *apiClient) upload(ctx context.Context, root string, file OccurrenceResult, identity localContentIdentity) (uploadResponseDTO, error) {
	if file.Bytes < 0 || file.Bytes > maxUploadBytes || file.Date == "" {
		return uploadResponseDTO{}, &Error{Code: "UPLOAD_INPUT_INVALID", Message: "Selected upload metadata is incomplete or exceeds the configured media bound."}
	}
	createdAt, err := time.Parse(time.RFC3339Nano, file.Date)
	if err != nil {
		return uploadResponseDTO{}, &Error{Code: "UPLOAD_DATE_INVALID", Message: "Selected upload date is invalid."}
	}
	opened, err := openPortableMedia(root, file.OutputPath, file.Bytes)
	if err != nil {
		return uploadResponseDTO{}, localError("LOCAL_MEDIA_CHANGED", "A local media file changed before upload.", err)
	}
	defer opened.Close()
	dateXMP, err := generatedDateXMP(createdAt)
	if err != nil {
		return uploadResponseDTO{}, &Error{Code: "UPLOAD_DATE_INVALID", Message: "Selected upload date could not be represented safely."}
	}
	pr, pw := io.Pipe()
	contentType := ""
	mw := multipart.NewWriter(pw)
	contentType = mw.FormDataContentType()
	writerDone := make(chan error, 1)
	filename := path.Base(strings.ReplaceAll(file.SourceEntry, "\\", "/"))
	if filename == "." || filename == "" || hasControl(filename) {
		filename = "media"
	}
	sidecarFilename := filename + ".xmp"
	go func() {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="sidecarData"; filename=%q`, sidecarFilename))
		header.Set("Content-Type", "application/xml")
		sidecarWriter, writeErr := mw.CreatePart(header)
		if writeErr == nil {
			_, writeErr = sidecarWriter.Write(dateXMP)
		}
		if writeErr == nil {
			writeErr = mw.WriteField("fileCreatedAt", createdAt.UTC().Format(time.RFC3339Nano))
		}
		if writeErr == nil {
			writeErr = mw.WriteField("fileModifiedAt", createdAt.UTC().Format(time.RFC3339Nano))
		}
		if writeErr == nil {
			writeErr = mw.WriteField("filename", path.Base(strings.ReplaceAll(file.SourceEntry, "\\", "/")))
		}
		var destination io.Writer
		if writeErr == nil {
			header := make(textproto.MIMEHeader)
			header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="assetData"; filename=%q`, filename))
			header.Set("Content-Type", "application/octet-stream")
			destination, writeErr = mw.CreatePart(header)
		}
		if writeErr == nil {
			h := sha256.New()
			sha1Hash := sha1.New()
			limited := io.LimitReader(&contextReader{ctx: ctx, r: opened}, file.Bytes+1)
			var count int64
			count, writeErr = io.Copy(io.MultiWriter(destination, h, sha1Hash), limited)
			if writeErr == nil && (count != file.Bytes || hex.EncodeToString(h.Sum(nil)) != file.SHA256 || base64Encode(sha1Hash.Sum(nil)) != identity.sha1) {
				writeErr = errors.New("media changed while streaming upload")
			}
		}
		if writeErr == nil {
			writeErr = mw.Close()
		}
		if writeErr != nil {
			_ = pw.CloseWithError(writeErr)
			writerDone <- writeErr
			return
		}
		writerDone <- pw.Close()
	}()
	checksumHeader := make(http.Header)
	checksumHeader.Set("x-immich-checksum", identity.sha1)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin+"/api/assets", pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		<-writerDone
		return uploadResponseDTO{}, &Error{Code: "REQUEST_INVALID", Message: "Immich upload request could not be constructed safely.", cause: err}
	}
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Content-Type", contentType)
	for name, values := range checksumHeader {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, requestErr := c.http.Do(req)
	if requestErr != nil {
		_ = pr.CloseWithError(requestErr)
		streamErr := <-writerDone
		if streamErr != nil && !errors.Is(streamErr, io.ErrClosedPipe) {
			return uploadResponseDTO{}, localError("UPLOAD_STREAM_FAILED", "Local media could not be streamed safely.", streamErr)
		}
		return uploadResponseDTO{}, &Error{Code: "NETWORK_ERROR", Message: "Immich upload outcome is unknown because the request was interrupted.", cause: requestErr}
	}
	streamErr := <-writerDone
	if streamErr != nil {
		_ = resp.Body.Close()
		return uploadResponseDTO{}, localError("UPLOAD_STREAM_FAILED", "Local media could not be streamed safely.", streamErr)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if resp.StatusCode >= http.StatusInternalServerError {
			return uploadResponseDTO{}, &Error{Code: "MUTATION_RESPONSE_UNKNOWN", Message: "Immich returned a server error after upload; the outcome must be reconciled before retry."}
		}
		return uploadResponseDTO{}, &Error{Code: "UPLOAD_REJECTED", Message: fmt.Sprintf("Immich rejected the upload with HTTP status %d.", resp.StatusCode)}
	}
	var uploaded uploadResponseDTO
	if err := decodeJSONResponse(resp, &uploaded); err != nil {
		return uploadResponseDTO{}, err
	}
	if !validUUID(uploaded.ID) || (uploaded.Status != "created" && uploaded.Status != "duplicate") {
		return uploadResponseDTO{}, &Error{Code: "UPLOAD_RESPONSE_INVALID", Message: "Immich returned an incomplete upload result; the outcome must be reconciled before retry."}
	}
	return uploaded, nil
}

func (c *apiClient) createAlbum(ctx context.Context, title, marker string) (remoteAlbumDTO, error) {
	if title == "" || hasControl(title) || marker == "" {
		return remoteAlbumDTO{}, &Error{Code: "ALBUM_INPUT_INVALID", Message: "Source album metadata is incomplete."}
	}
	var album remoteAlbumDTO
	if err := c.json(ctx, http.MethodPost, "/albums", createAlbumDTO{AlbumName: title, Description: marker}, &album, http.StatusCreated); err != nil {
		return remoteAlbumDTO{}, err
	}
	if !validUUID(album.ID) {
		return remoteAlbumDTO{}, &Error{Code: "ALBUM_CREATE_INVALID", Message: "Immich returned an invalid newly created album identity."}
	}
	return album, nil
}

func (c *apiClient) addAlbumAssets(ctx context.Context, albumID string, ids []string) error {
	if !validUUID(albumID) || len(ids) == 0 || len(ids) > searchPageSize {
		return &Error{Code: "ALBUM_MEMBERSHIP_INPUT_INVALID", Message: "Album membership request is incomplete or exceeds the configured bound."}
	}
	for _, id := range ids {
		if !validUUID(id) {
			return &Error{Code: "ALBUM_MEMBERSHIP_INPUT_INVALID", Message: "Album membership contains an invalid asset identity."}
		}
	}
	body, err := json.Marshal(map[string][]string{"ids": ids})
	if err != nil {
		return &Error{Code: "REQUEST_INVALID", Message: "Album membership request could not be encoded safely.", cause: err}
	}
	resp, err := c.request(ctx, http.MethodPut, "/albums/"+url.PathEscape(albumID)+"/assets", bytes.NewReader(body), "application/json", http.StatusOK)
	if err != nil {
		return err
	}
	var result []struct {
		ID      string `json:"id"`
		Success bool   `json:"success"`
	}
	if err := decodeJSONResponse(resp, &result); err != nil {
		return err
	}
	if len(result) != len(ids) {
		return &Error{Code: "ALBUM_MEMBERSHIP_RESPONSE_INVALID", Message: "Immich returned an incomplete album membership result."}
	}
	seen := map[string]bool{}
	for _, item := range result {
		id := strings.ToLower(item.ID)
		if !validUUID(id) || seen[id] || !item.Success {
			return &Error{Code: "ALBUM_MEMBERSHIP_RESPONSE_INVALID", Message: "Immich rejected or mismatched an album membership result."}
		}
		seen[id] = true
	}
	for _, id := range ids {
		if !seen[strings.ToLower(id)] {
			return &Error{Code: "ALBUM_MEMBERSHIP_RESPONSE_INVALID", Message: "Immich returned a membership result for the wrong asset."}
		}
	}
	return nil
}

func verifyLocalSnapshot(ctx context.Context, root string, plan *PlanReport) error {
	verified, err := bridge.Verify(ctx, root)
	if err != nil {
		return err
	}
	if verified == nil || verified.Status != "ok" {
		return errors.New("portable archive failed a repeat integrity check")
	}
	for _, file := range plan.Files {
		if file.State != "ready" {
			continue
		}
		if _, err := hashPortableMedia(ctx, root, file.OutputPath, file.SHA256, file.Bytes); err != nil {
			return err
		}
	}
	manifest, err := digestManifest(ctx, root)
	if err != nil || manifest != plan.ManifestSHA256 {
		return errors.New("manifest identity changed")
	}
	return nil
}

func resolveAlbums(remote []remoteAlbumDTO, planned []AlbumResult, account string) ([]resolvedAlbum, error) {
	result := make([]resolvedAlbum, len(planned))
	for i, album := range planned {
		result[i] = resolvedAlbum{index: i, existingMembers: map[string]bool{}}
		matches := make([]remoteAlbumDTO, 0, 1)
		for _, candidate := range remote {
			if candidate.Description != album.Marker || !strings.EqualFold(candidate.AlbumUsers[0].User.ID, account) {
				continue
			}
			matches = append(matches, candidate)
		}
		if len(matches) > 1 {
			return nil, &Error{Code: "OWNED_ALBUM_MARKER_AMBIGUOUS", Message: "Multiple account-owned Immich albums use the same source album marker."}
		}
		if len(matches) == 1 {
			if matches[0].Name != album.Title {
				return nil, &Error{Code: "OWNED_ALBUM_TITLE_CHANGED", Message: "A source-owned Immich album title differs from the selected source; v1 will not rename it."}
			}
			result[i].remoteID = strings.ToLower(matches[0].ID)
		}
	}
	return result, nil
}

type resolvedAlbum struct {
	index           int
	remoteID        string
	existingMembers map[string]bool
}

func remoteAlbumFor(albums []resolvedAlbum, index int) *resolvedAlbum {
	for i := range albums {
		if albums[i].index == index {
			return &albums[i]
		}
	}
	return nil
}

func albumMarker(id string) string { return "ArchiveBridge:sourceAlbum:v1:" + id }

func deterministicAssetID(planID, sha string) string {
	sum := sha256.Sum256([]byte("archivebridge-v1\x00" + planID + "\x00" + sha))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func validateResume(previous, current *Report, plan *PlanReport) error {
	if previous == nil || previous.SchemaVersion != ReportSchemaVersion || previous.Mode != "import" || previous.PlanID != current.PlanID || previous.ManifestSHA256 != current.ManifestSHA256 || previous.DateTransferPolicy != dateTransferPolicy || current.DateTransferPolicy != dateTransferPolicy || previous.ServerOrigin == "" || previous.ServerOrigin != current.ServerOrigin || previous.ServerVersion != SupportedServerVersion || previous.ServerVersion != current.ServerVersion || !validUUID(previous.AccountID) || !strings.EqualFold(previous.AccountID, current.AccountID) {
		return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report is malformed or does not identify this selected archive and supported server.", cause: ErrResumeMismatch}
	}
	if previous.Status != "in_progress" && previous.Status != "interrupted" && previous.Status != "needs_reconciliation" && previous.Status != "failed" && previous.Status != "completed" && previous.Status != "completed_with_skips" {
		return &Error{Code: "RESUME_STATUS_INVALID", Message: "Resume report is not in a resumable import state.", cause: ErrResumeMismatch}
	}
	if len(previous.Files) != len(current.Files) || len(previous.Sidecars) != len(current.Sidecars) || len(previous.Albums) != len(current.Albums) || len(previous.Contents) != len(current.Contents) {
		return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report item inventory does not match the verified archive.", cause: ErrResumeMismatch}
	}
	files := map[string]OccurrenceResult{}
	for _, file := range previous.Files {
		if file.OccurrenceID == "" || files[file.OccurrenceID].OccurrenceID != "" || file.State != "pending" && file.State != "in_flight" && file.State != "uploaded" && file.State != "reused" && file.State != "verified" && file.State != "skipped" && file.State != "unknown" && file.State != "failed" {
			return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report contains duplicate or unsupported occurrence state.", cause: ErrResumeMismatch}
		}
		files[file.OccurrenceID] = file
	}
	for _, fresh := range current.Files {
		old, ok := files[fresh.OccurrenceID]
		if !ok || old.SHA256 != fresh.SHA256 || old.Bytes != fresh.Bytes || old.OutputPath != fresh.OutputPath || old.SourceEntry != fresh.SourceEntry || old.Date != fresh.Date || !equalStringLists(old.SourceAlbumIDs, fresh.SourceAlbumIDs) || (old.State == "skipped") != (fresh.State == "skipped") {
			return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report occurrence identity differs from the verified archive.", cause: ErrResumeMismatch}
		}
		if old.RemoteAssetID != "" && !validUUID(old.RemoteAssetID) {
			return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report contains an invalid remote asset identity.", cause: ErrResumeMismatch}
		}
	}
	contents := map[string]ContentResult{}
	for _, old := range previous.Contents {
		if _, exists := contents[old.SHA256]; exists || len(old.SHA256) != 64 || old.Bytes < 0 || old.State != "pending" && old.State != "in_flight" && old.State != "uploaded" && old.State != "reused" && old.State != "verified" && old.State != "skipped" && old.State != "unknown" && old.State != "failed" {
			return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report content inventory is malformed.", cause: ErrResumeMismatch}
		}
		contents[old.SHA256] = old
	}
	for _, fresh := range current.Contents {
		old, ok := contents[fresh.SHA256]
		if !ok || old.Bytes != fresh.Bytes || !equalStringLists(old.OccurrenceIDs, fresh.OccurrenceIDs) || (old.RemoteAssetID != "" && !validUUID(old.RemoteAssetID)) {
			return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report content identity differs from the verified archive.", cause: ErrResumeMismatch}
		}
		mustSkip := true
		for _, occurrenceID := range fresh.OccurrenceIDs {
			if file := occurrenceByID(current.Files, occurrenceID); file != nil && file.State != "skipped" {
				mustSkip = false
				break
			}
		}
		if (old.State == "skipped") != mustSkip {
			return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report content skip state differs from the selected archive scope.", cause: ErrResumeMismatch}
		}
		if (old.State == "uploaded" || old.State == "reused" || old.State == "verified") && (old.RemoteAssetID == "" || !old.OriginalSHA256Verified || old.VerifiedOriginalBytes != old.Bytes) {
			return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report claims content verification without a complete original hash result.", cause: ErrResumeMismatch}
		}
	}
	for i, fresh := range current.Albums {
		old := previous.Albums[i]
		mustSkip := len(fresh.OccurrenceIDs) > 0 && !albumHasReadyMembers(current.Files, fresh.OccurrenceIDs)
		validAlbumState := old.State == "pending" || old.State == "in_flight" || old.State == "created" || old.State == "reused" || old.State == "verified" || old.State == "skipped" || old.State == "unknown" || old.State == "failed"
		if !validAlbumState || old.SourceAlbumID != fresh.SourceAlbumID || old.Title != fresh.Title || old.Folder != fresh.Folder || old.Marker != fresh.Marker || !equalStringLists(old.OccurrenceIDs, fresh.OccurrenceIDs) || (old.State == "skipped") != mustSkip || (old.RemoteAlbumID != "" && !validUUID(old.RemoteAlbumID)) || (old.OwnerID != "" && !validUUID(old.OwnerID)) || (old.OwnerID != "" && old.RemoteAlbumID == "") || (old.RemoteAlbumID != "" && old.OwnerID == "" && old.State != "in_flight" && old.State != "unknown") {
			return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report album identity differs from the verified archive.", cause: ErrResumeMismatch}
		}
		if old.RemoteAlbumID != "" && !strings.EqualFold(old.OwnerID, previous.AccountID) {
			return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report album owner does not match its authenticated account.", cause: ErrResumeMismatch}
		}
		if (old.State == "created" || old.State == "reused" || old.State == "verified") && old.RemoteAlbumID == "" {
			return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report claims a resolved album without a remote identity.", cause: ErrResumeMismatch}
		}
	}
	for i, fresh := range current.Sidecars {
		old := previous.Sidecars[i]
		if old.SidecarID != fresh.SidecarID || old.SourceEntry != fresh.SourceEntry || old.SHA256 != fresh.SHA256 || old.Bytes != fresh.Bytes || old.Transferred || old.State != "not_transferred" {
			return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report sidecar inventory differs from the verified archive.", cause: ErrResumeMismatch}
		}
	}
	if len(previous.Memberships) != len(current.Memberships) {
		return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report source-album relationship inventory differs from the verified archive.", cause: ErrResumeMismatch}
	}
	filesByID := make(map[string]OccurrenceResult, len(previous.Files))
	for _, file := range previous.Files {
		filesByID[file.OccurrenceID] = file
	}
	albumsByID := make(map[string]AlbumResult, len(previous.Albums))
	for _, album := range previous.Albums {
		albumsByID[album.SourceAlbumID] = album
	}
	contentsByOccurrence := make(map[string]ContentResult, len(previous.Files))
	for _, content := range previous.Contents {
		for _, occurrenceID := range content.OccurrenceIDs {
			contentsByOccurrence[occurrenceID] = content
		}
	}
	seenMemberships := map[string]bool{}
	for _, membership := range previous.Memberships {
		key := membership.SourceAlbumID + "\x00" + membership.OccurrenceID
		if seenMemberships[key] || membership.State != "pending" && membership.State != "in_flight" && membership.State != "added" && membership.State != "already_present" && membership.State != "verified" && membership.State != "skipped" && membership.State != "unknown" && membership.State != "failed" {
			return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report contains duplicate or unsupported album membership state.", cause: ErrResumeMismatch}
		}
		seenMemberships[key] = true
		album, albumOK := albumsByID[membership.SourceAlbumID]
		file, fileOK := filesByID[membership.OccurrenceID]
		if !albumOK || !fileOK || !stringInSlice(album.OccurrenceIDs, membership.OccurrenceID) || membership.SourceAlbumID == "" {
			return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report contains an unrelated album membership.", cause: ErrResumeMismatch}
		}
		content := contentsByOccurrence[membership.OccurrenceID]
		if content.SHA256 == "" || (membership.RemoteAssetID != "" && (!validUUID(membership.RemoteAssetID) || !strings.EqualFold(membership.RemoteAssetID, content.RemoteAssetID))) || (membership.RemoteAlbumID != "" && (!validUUID(membership.RemoteAlbumID) || !strings.EqualFold(membership.RemoteAlbumID, album.RemoteAlbumID))) {
			return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report membership remote identities do not match their source album and content.", cause: ErrResumeMismatch}
		}
		if (membership.State == "skipped") != (file.State == "skipped") {
			return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report membership skip state differs from its occurrence.", cause: ErrResumeMismatch}
		}
		if (membership.State == "added" || membership.State == "already_present" || membership.State == "verified") && (membership.RemoteAlbumID == "" || membership.RemoteAssetID == "") {
			return &Error{Code: "RESUME_REPORT_INVALID", Message: "Resume report claims a verified membership without remote identities.", cause: ErrResumeMismatch}
		}
	}
	_ = plan
	return nil
}

func mergeResumeHints(current, previous *Report) {
	current.ServerOrigin, current.ServerVersion, current.AccountID = previous.ServerOrigin, previous.ServerVersion, previous.AccountID
	oldContent := map[string]ContentResult{}
	for _, content := range previous.Contents {
		oldContent[content.SHA256] = content
	}
	for i := range current.Contents {
		if old, ok := oldContent[current.Contents[i].SHA256]; ok && old.RemoteAssetID != "" {
			current.Contents[i].RemoteAssetID = old.RemoteAssetID
			current.Contents[i].State = "pending"
		}
	}
	oldAlbums := map[string]AlbumResult{}
	for _, album := range previous.Albums {
		oldAlbums[album.SourceAlbumID] = album
	}
	for i := range current.Albums {
		if old, ok := oldAlbums[current.Albums[i].SourceAlbumID]; ok {
			current.Albums[i].RemoteAlbumID = old.RemoteAlbumID
			current.Albums[i].OwnerID = old.OwnerID
		}
	}
}

func errorCode(err error) string {
	var typed *Error
	if errors.As(err, &typed) && typed.Code != "" {
		return typed.Code
	}
	return "OPERATION_FAILED"
}

func callbackError(err error) error {
	return &Error{Code: "PROGRESS_CALLBACK_FAILED", Message: "Import checkpoint could not be persisted; no subsequent remote mutation was attempted.", cause: err}
}

func emitProgress(callback func(Report) error, report Report) error {
	if callback == nil {
		return nil
	}
	return callback(cloneReport(report))
}

func cloneReport(in Report) Report {
	out := in
	out.Files = append([]OccurrenceResult{}, in.Files...)
	for i := range out.Files {
		out.Files[i].SourceAlbumIDs = append([]string{}, in.Files[i].SourceAlbumIDs...)
	}
	out.Sidecars = append([]PlannedSidecar{}, in.Sidecars...)
	out.Contents = append([]ContentResult{}, in.Contents...)
	for i := range out.Contents {
		out.Contents[i].OccurrenceIDs = append([]string{}, in.Contents[i].OccurrenceIDs...)
	}
	out.Albums = append([]AlbumResult{}, in.Albums...)
	for i := range out.Albums {
		out.Albums[i].OccurrenceIDs = append([]string{}, in.Albums[i].OccurrenceIDs...)
	}
	out.Memberships = append([]MembershipResult{}, in.Memberships...)
	out.Issues = append([]Issue{}, in.Issues...)
	if in.Verification != nil {
		copy := *in.Verification
		out.Verification = &copy
	}
	return out
}

func failReport(report *Report, err error) (*Report, error) {
	if isContextStop(err) {
		return interruptedReport(report, err)
	}
	report.Status = "failed"
	report.Issues = append(report.Issues, Issue{Code: errorCode(err), Details: "Remote operation did not complete successfully; no automatic retry was attempted."})
	return report, err
}

func interruptedReport(report *Report, cause error) (*Report, error) {
	report.Status = "interrupted"
	report.Issues = append(report.Issues, Issue{Code: "CANCELED", Details: "Operation was canceled; the report preserves the last known state for explicit reconciliation."})
	return report, &Error{Code: "CANCELED", Message: "Operation was canceled.", cause: cause}
}

func isContextStop(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func isAmbiguousMutation(err error) bool {
	var typed *Error
	if !errors.As(err, &typed) {
		return true
	}
	return typed.Code == "NETWORK_ERROR" || typed.Code == "MUTATION_RESPONSE_UNKNOWN" || typed.Code == "UPLOAD_STREAM_FAILED" || typed.Code == "RESPONSE_READ_FAILED" || typed.Code == "RESPONSE_INVALID" || typed.Code == "RESPONSE_TOO_LARGE"
}

func firstOccurrence(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func firstReadyOccurrence(files []OccurrenceResult, ids []string) string {
	for _, id := range ids {
		file := occurrenceByID(files, id)
		if file != nil && file.State != "skipped" && file.State != "blocked" && file.Date != "" {
			return id
		}
	}
	return ""
}

func reportHasSkippedFile(files []OccurrenceResult) bool {
	for _, file := range files {
		if file.State == "skipped" {
			return true
		}
	}
	return false
}

func occurrenceByID(files []OccurrenceResult, id string) *OccurrenceResult {
	for i := range files {
		if files[i].OccurrenceID == id {
			return &files[i]
		}
	}
	return nil
}

func contentForOccurrence(contents []ContentResult, id string) *ContentResult {
	for i := range contents {
		for _, candidate := range contents[i].OccurrenceIDs {
			if candidate == id {
				return &contents[i]
			}
		}
	}
	return nil
}

func appendStateToOccurrences(report *Report, content *ContentResult) {
	for i := range report.Files {
		for _, id := range content.OccurrenceIDs {
			if report.Files[i].OccurrenceID == id && report.Files[i].State != "skipped" {
				report.Files[i].State = content.State
				report.Files[i].RemoteAssetID = content.RemoteAssetID
			}
		}
	}
}

func dateForContent(files []OccurrenceResult, ids []string) string {
	for _, id := range ids {
		if file := occurrenceByID(files, id); file != nil && file.Date != "" {
			return file.Date
		}
	}
	return ""
}

func albumHasReadyMembers(files []OccurrenceResult, ids []string) bool {
	if len(ids) == 0 {
		return true
	}
	for _, id := range ids {
		if file := occurrenceByID(files, id); file != nil && file.State != "skipped" {
			return true
		}
	}
	return false
}

func addMembership(report *Report, album *AlbumResult, file *OccurrenceResult, state string) {
	content := contentForOccurrence(report.Contents, file.OccurrenceID)
	assetID := ""
	if content != nil {
		assetID = content.RemoteAssetID
	}
	for i := range report.Memberships {
		membership := &report.Memberships[i]
		if membership.SourceAlbumID == album.SourceAlbumID && membership.OccurrenceID == file.OccurrenceID {
			membership.RemoteAlbumID, membership.RemoteAssetID, membership.State = album.RemoteAlbumID, assetID, state
			return
		}
	}
	report.Memberships = append(report.Memberships, MembershipResult{SourceAlbumID: album.SourceAlbumID, OccurrenceID: file.OccurrenceID, RemoteAlbumID: album.RemoteAlbumID, RemoteAssetID: assetID, State: state})
}

func setMembershipState(report *Report, sourceAlbumID, assetID, state string) {
	for i := range report.Memberships {
		membership := &report.Memberships[i]
		if membership.SourceAlbumID == sourceAlbumID && strings.EqualFold(membership.RemoteAssetID, assetID) {
			file := occurrenceByID(report.Files, membership.OccurrenceID)
			if file != nil && file.State != "skipped" {
				membership.State = state
			}
		}
	}
}

func setMembershipStates(report *Report, album *AlbumResult, assetIDs []string, state string) {
	for _, id := range assetIDs {
		setMembershipState(report, album.SourceAlbumID, id, state)
	}
}

func equalStringSet(actual []string, expected map[string]bool) bool {
	if len(actual) != len(expected) {
		return false
	}
	for _, value := range actual {
		if !expected[strings.ToLower(value)] {
			return false
		}
	}
	return true
}

func equalStringLists(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		if counts[value] == 0 {
			return false
		}
		counts[value]--
	}
	return true
}

func stringInSlice(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func filepathAbs(value string) (string, error) { return filepath.Abs(value) }

func base64Decode(value string) ([]byte, error) { return base64.StdEncoding.DecodeString(value) }
func base64Encode(value []byte) string          { return base64.StdEncoding.EncodeToString(value) }

func mustDecodeSHA1(value string) []byte {
	decoded, _ := base64.StdEncoding.DecodeString(value)
	return decoded
}

func newSHA1() hash.Hash { return sha1.New() }

func previousContent(report *Report, sha string) *ContentResult {
	for i := range report.Contents {
		if report.Contents[i].SHA256 == sha {
			return &report.Contents[i]
		}
	}
	return nil
}

func needsMetadataWait(state string) bool {
	return state == "in_flight" || state == "unknown" || state == "failed"
}
