# Verification and release evidence

Use the [Releases page](https://github.com/Pastalikek65/archivebridge/releases) to find available package versions, matching SHA-256 checksums, and their evidence. Verify the exact package against the checksum published with the same version before extracting it. Evidence binds a particular version, source commit, platform, and package; it must not be carried forward to another version or build. Historical 0.2.0 evidence does not qualify the 1.0 Immich adapter.

## Local archive checks

`archivebridge verify --archive DIR` checks stored files and archive structure against the local manifest. The manifest is unsigned, so verification establishes consistency with the manifest but not its authenticity. `archivebridge compare --plan PLAN --archive DIR` additionally reopens the original source parts recorded in the plan and checks the selected-source identities, occurrences, relationships, and stored bytes. Neither command proves that the selected parts comprise an entire online account.

The viewer's verification action checks the local archive. It does not make remote changes. Keep plans and archives private: plans include local paths, and archives include original media, sidecars, names, and album relationships.

## Immich checks

The Immich adapter targets server version 3.3.1 exactly. Its preflight verifies the local archive, server version, account, and required permissions before the first mutation. For a new upload, ArchiveBridge sends the original bytes with generated minimal date-only XMP carrying the supported Takeout UTC date. It waits for Immich's metadata worker and checks `fileCreatedAt`, `fileModifiedAt`, and `ExifDateTimeOriginal` against that date, then verifies full original bytes using SHA-256 and size before the upload is marked ready or album changes begin. Existing assets with conflicting dates are refused without metadata updates. `archivebridge immich verify` performs read-only remote checks. Reports cover only selected archive contents and do not prove whole-account completeness.

Import reports are durable local operation records and identify the `dateTransferPolicy` as `takeout-date-authoritative-generated-xmp-v1`. Resume requires the same archive manifest, server origin and version, and account; uncertain operations are reconciled before retry. If cancellation or the bounded metadata-worker wait expires after upload acceptance, the partial report retains the accepted remote asset ID for explicit resume. Reports omit the API key and raw server responses, but include source names, album details, server origin, account identity, and operation state. Store them privately.

## Scope of performance and compatibility evidence

Release evidence describes only the exact platform packages and tests named for that version. The 0.2.0 recovery evidence used a 128 MiB ZIP_STORED synthetic source whose large member repeats a deterministic 16 KiB byte block. It measures bounded native reads, writes, interruption, and resume for that fixture; it is not representative of JPEG compression and does not predict throughput on real photo libraries. Unit tests, development binaries, and local synthetic runs are useful checks but are not a substitute for a release's own platform evidence.
