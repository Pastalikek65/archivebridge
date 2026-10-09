package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
)

// Compare re-inspects the selected original archives, compares that result
// with the supplied plan and archive manifest, and verifies all exported
// content. A matched result only covers the selected sources and manifest;
// it is not an account-wide or provider-authenticity claim.
func Compare(ctx context.Context, plan *Plan, archiveDir string) (*CompareReport, error) {
	if err := validatePlan(plan, true); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := filepath.Abs(archiveDir)
	if err != nil {
		return nil, err
	}
	out = filepath.Clean(out)
	rootInfo, err := os.Lstat(out)
	if err != nil {
		return nil, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, errors.New("archive root must be a real directory")
	}

	report := &CompareReport{
		Status:          "matched",
		PlanID:          plan.ID,
		SourcesChecked:  len(plan.Sources),
		MediaChecked:    len(plan.Files),
		SidecarsChecked: len(plan.Sidecars),
		AlbumsChecked:   len(plan.Albums),
		Mismatches:      []CompareMismatch{},
	}
	add := func(code, entry, details string) {
		report.Status = "mismatched"
		report.Mismatches = append(report.Mismatches, CompareMismatch{Code: code, EntryPath: entry, Details: details})
	}

	paths := make([]string, len(plan.Sources))
	initialSourceEligible := make([]bool, len(plan.Sources))
	for i, src := range plan.Sources {
		paths[i] = src.Path
		st, statErr := os.Lstat(src.Path)
		if statErr != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() || st.Size() != src.Bytes {
			add("source_identity", "", fmt.Sprintf("Selected source %d is missing or its file identity changed.", i+1))
		} else {
			initialSourceEligible[i] = true
		}
	}

	manifest, manifestErr := ReadManifest(out)
	if manifestErr != nil {
		add("manifest_invalid", "", "The archive manifest is missing, invalid, or does not describe a valid bridge plan.")
	} else {
		report.ArchivePlanID = manifest.PlanID
		if manifest.PlanID != plan.ID {
			add("manifest_plan_mismatch", "", "The archive manifest belongs to a different inspection plan.")
		}
		comparePlanViews(manifestFromPlan(plan), manifest, add)
	}

	fresh, inspectErr := Inspect(ctx, paths, DefaultLimits())
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if inspectErr != nil {
		add("source_reinspection_failed", "", "The selected original archives could not be re-inspected.")
	} else {
		if fresh.ID != plan.ID {
			add("source_plan_mismatch", "", "Fresh inspection of the selected originals differs from the supplied plan.")
		}
		comparePlanViews(manifestFromPlan(plan), manifestFromPlan(fresh), add)
		if manifestErr == nil && fresh.ID != manifest.PlanID {
			add("manifest_source_mismatch", "", "Fresh inspection of the selected originals differs from the exported manifest.")
		}
	}

	verified, verifyErr := Verify(ctx, out)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if verifyErr != nil || verified == nil || verified.Status != "ok" {
		add("archive_verify_failed", "", "Exported content or archive structure did not pass verification.")
		if verified != nil {
			for _, issue := range verified.Issues {
				add("verify_"+issue.Code, issue.EntryPath, issue.Details)
			}
		}
	}
	// Inspect and Verify read sources over time. Recheck at the end so a source
	// changed during comparison cannot be reported as still matching.
	for i, src := range plan.Sources {
		matches, checkErr := sourceIdentityMatches(ctx, src)
		if checkErr != nil {
			return nil, checkErr
		}
		if initialSourceEligible[i] && !matches {
			add("source_identity", "", fmt.Sprintf("Selected source %d changed while comparison was running.", i+1))
		}
	}
	return report, nil
}

func sourceIdentityMatches(ctx context.Context, src Source) (bool, error) {
	st, err := os.Lstat(src.Path)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() || st.Size() != src.Bytes {
		return false, nil
	}
	sha, size, err := hashFile(ctx, src.Path)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	if err != nil || size != src.Bytes || sha != src.SHA256 {
		return false, nil
	}
	return true, nil
}

// comparePlanViews compares portable plan data without considering private
// source paths. It reports category-level differences for each affected item.
func comparePlanViews(expected, actual *Manifest, add func(code, entry, details string)) {
	if expected.SchemaVersion != actual.SchemaVersion {
		add("schema_mismatch", "", "Plan and archive manifest use different schema versions.")
	}
	if expected.PlanID != actual.PlanID {
		add("plan_id_mismatch", "", "Plan identifiers differ.")
	}
	if len(expected.Sources) != len(actual.Sources) {
		add("source_mismatch", "", "Source archive identities differ.")
	}
	for i := 0; i < len(expected.Sources) && i < len(actual.Sources); i++ {
		if expected.Sources[i] != actual.Sources[i] {
			add("source_mismatch", "", fmt.Sprintf("Selected source %d identity differs.", i+1))
		}
	}

	expectedMedia := make(map[string]MediaOccurrence, len(expected.Files))
	actualMedia := make(map[string]MediaOccurrence, len(actual.Files))
	for _, v := range expected.Files {
		expectedMedia[v.ID] = v
	}
	for _, v := range actual.Files {
		actualMedia[v.ID] = v
	}
	compareIDSet(expectedMedia, actualMedia, func(id string, before, after MediaOccurrence, okBefore, okAfter bool) {
		entry := before.EntryPath
		if !okBefore {
			entry = after.EntryPath
		}
		if !okBefore || !okAfter {
			add("media_content_mismatch", entry, "A media occurrence is missing from one plan view.")
			return
		}
		beforeAlbums, afterAlbums := before.AlbumIDs, after.AlbumIDs
		before.AlbumIDs, after.AlbumIDs = nil, nil
		if !reflect.DeepEqual(before, after) {
			add("media_content_mismatch", entry, "Media identity or metadata differs between plan views.")
		}
		if !reflect.DeepEqual(beforeAlbums, afterAlbums) {
			add("relationship_mismatch", entry, "Media album relationships differ between plan views.")
		}
	})

	expectedSidecars := make(map[string]Sidecar, len(expected.Sidecars))
	actualSidecars := make(map[string]Sidecar, len(actual.Sidecars))
	for _, v := range expected.Sidecars {
		expectedSidecars[v.ID] = v
	}
	for _, v := range actual.Sidecars {
		actualSidecars[v.ID] = v
	}
	compareIDSet(expectedSidecars, actualSidecars, func(_ string, before, after Sidecar, okBefore, okAfter bool) {
		entry := before.EntryPath
		if !okBefore {
			entry = after.EntryPath
		}
		if !okBefore || !okAfter || before != after {
			add("sidecar_mismatch", entry, "A preserved sidecar identity differs between plan views.")
		}
	})

	expectedAlbums := make(map[string]Album, len(expected.Albums))
	actualAlbums := make(map[string]Album, len(actual.Albums))
	for _, v := range expected.Albums {
		expectedAlbums[v.ID] = v
	}
	for _, v := range actual.Albums {
		actualAlbums[v.ID] = v
	}
	compareIDSet(expectedAlbums, actualAlbums, func(_ string, before, after Album, okBefore, okAfter bool) {
		if !okBefore || !okAfter || !reflect.DeepEqual(before, after) {
			entry := before.Folder
			if !okBefore {
				entry = after.Folder
			}
			add("relationship_mismatch", entry, "Album descriptors or media relationships differ between plan views.")
		}
	})

	if !reflect.DeepEqual(expected.Issues, actual.Issues) {
		add("issue_mismatch", "", "Inspection issue records differ between plan views.")
	}
	if expected.Stats != actual.Stats {
		add("statistics_mismatch", "", "Plan counts or byte statistics differ between plan views.")
	}
}

func compareIDSet[T any](expected, actual map[string]T, visit func(id string, before, after T, okBefore, okAfter bool)) {
	seen := make(map[string]bool, len(expected)+len(actual))
	for id := range expected {
		seen[id] = true
	}
	for id := range actual {
		seen[id] = true
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		before, okBefore := expected[id]
		after, okAfter := actual[id]
		visit(id, before, after, okBefore, okAfter)
	}
}
