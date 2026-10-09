# Command guide

This guide describes the ArchiveBridge 1.0 command set. Check the installed binary with `archivebridge --version --json`. The [Releases page](https://github.com/Pastalikek65/archivebridge/releases) lists available packages, matching checksums, and evidence for each version and platform. A release's evidence applies only to the exact version and package it names. Immich import requires ArchiveBridge 1.0.0 or later and Immich server version 3.3.1 exactly.

`--source` may be repeated to select all parts from one Takeout export. Quote paths containing spaces. Plans and output archives must use new paths; keep original sources available for compare and resume.

| Command | Result |
| --- | --- |
| `inspect --source part.zip [--source part2.tar.gz] [--json]` | Read-only preview of selected files, dates, relationships, and unresolved information. |
| `plan --source part.zip --output plan.json [--json]` | Save a versioned plan to a new file. |
| `export --plan plan.json --out archive-dir [--json]` | Write verified originals and sidecars into a new archive directory. |
| `resume --plan plan.json --out archive-dir [--json]` | Continue the same plan after checking existing stored files for safe reuse. |
| `verify --archive archive-dir [--json]` | Check stored files, relationships, and ownership against the local manifest. |
| `compare --plan plan.json --archive archive-dir [--json]` | Reinspect original sources and compare the plan, manifest, relationships, and stored bytes. |
| `serve --archive archive-dir [--listen 127.0.0.1:4175]` | Start the read-only local viewer. |
| `--version [--json]` | Report the application version and recorded build commit. |

Append `--json` for machine-readable results. Results use a versioned JSON envelope; diagnostics go to stderr, and invalid input or failed checks return a nonzero exit. Human output identifies the selected-source scope and unresolved items. `compare` requires the original source archives at their recorded paths. A `matched` result means those selected sources agree with the plan and archive and the stored files pass verification; it does not prove that every item in an online account was exported.

Export accepts a new directory or one owned by the exact same plan. It rejects unrelated files, symbolic links, changed sources, changed stored files, unsupported plan versions, and unsafe archive paths. Resume rehashes existing stored files before reuse. Do not remove ownership, lock, or staging files to bypass an error.

The archive stores content under `media/sha256` and raw JSON under `sidecars/sha256`. Original names and occurrences are recorded in `manifest.json`; the viewer downloads media using the original names. Stored paths are generated and are not renamed into guessed dates or folders. Filesystem modification times are separate from photo dates in the manifest.

## Import into Immich 3.3.1

The Immich commands are available in ArchiveBridge 1.0.0 and later. They support Immich server version 3.3.1 exactly. Start with the local, read-only plan and resolve or explicitly skip unresolved dates before importing.

| Command | Result |
| --- | --- |
| `immich plan --archive DIR [--skip-unresolved] [--json]` | Offline plan based on the local manifest and integrity verification. Missing, malformed, ambiguous, or conflicting dates block the default plan. |
| `immich import --archive DIR --server ORIGIN --report NEW [--api-key-env NAME] [--skip-unresolved] [--resume OLD] [--allow-http-loopback] [--timeout DURATION] [--json]` | Explicitly upload originals for the selected archive, preserve album relationships, and write a durable report. `NEW` must not exist. |
| `immich verify --archive DIR --server ORIGIN --api-key-env NAME --report INPUT --output NEW [--timeout DURATION] [--json]` | Read-only remote verification of originals, dates, and exact album membership. `NEW` must not exist. |

The API key comes from the variable named by `--api-key-env`; the default is `ARCHIVEBRIDGE_IMMICH_API_KEY`. Never put the key on the command line. Before a mutation, the adapter checks the server version, authenticated account, and these seven permissions: `asset.upload`, `asset.read`, `asset.download`, `album.read`, `album.create`, `albumAsset.create`, and `user.read`. The set excludes `asset.update` and asset-delete permissions.

Use HTTPS unless you explicitly opt into HTTP for a literal loopback IP (`127.0.0.1` or `::1`). Hostnames such as `localhost` are not accepted for the HTTP exception. Redirects and proxy use are unsupported. The default timeout is 30 minutes; `--timeout` can set a longer duration up to 24 hours. Cancellation or timeout is recorded in the report; inspect it before resuming.

By default, unresolved dates block the import. `--skip-unresolved` explicitly leaves those occurrences untransferred and records each skip. For each ready upload, ArchiveBridge sends the original media bytes with a generated minimal date-only XMP sidecar carrying the known Takeout UTC date. This also supplies Immich's required `fileCreatedAt` and `fileModifiedAt`; it does not reconstruct the source filesystem modification time. The report's `dateTransferPolicy` is `takeout-date-authoritative-generated-xmp-v1`. ArchiveBridge waits for Immich 3.3.1's metadata worker and requires `fileCreatedAt`, `fileModifiedAt`, and `ExifDateTimeOriginal` to match the source date, then verifies remote SHA-256 and size. Uploads are not marked ready and album creation or membership changes do not begin until these checks pass. Existing assets with conflicting dates are refused without metadata changes.

If cancellation or the bounded metadata-worker wait expires after Immich accepts an upload, the partial report retains the accepted remote asset ID. Use explicit `--resume` to reconcile that asset; do not start a new import to bypass the partial state. Raw Takeout JSON sidecars remain local, and their caption and GPS fields are not mapped to Immich. Original media bytes and embedded EXIF remain unchanged.

An import report is created before the first remote mutation and checkpointed before and after operations. Resume reads `OLD` and writes a separate new `NEW` report. It requires the same archive manifest, server origin and version, and authenticated account; API-key rotation is allowed for the same account. Uncertain operations are reconciled before retry. Keep reports private: they contain source names, album details, server origin, account ID, and operation state, but not the API key, its environment-variable name, raw server responses, or email address. `immich verify` is read-only. No remote deletion command is provided.

### JSON and privacy

All commands accept `--json`. Immich plan and operation reports include the `dateTransferPolicy` value `takeout-date-authoritative-generated-xmp-v1` so readers can identify the date rule used. Success writes a JSON object to stdout; diagnostics go to stderr. Error envelopes have a stable code and sanitized message. Local plans include source paths, and remote reports include selected source names and account details. Keep both private. Verification covers the local archive or selected remote import scope, not the complete contents of an account.
