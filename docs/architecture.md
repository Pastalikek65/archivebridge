# Architecture and archive contract

ArchiveBridge reads selected Google Photos Takeout parts and writes a portable local archive. The command line and local viewer use the same Go core. Importing into Immich is a separate, explicit action.

## Local archive flow

The reader streams ZIP and TAR.GZ members; it does not extract archive paths directly onto the filesystem. Source files are read-only. A plan records selected source parts and their identities, each media occurrence, raw sidecars, supported dates, album relationships, and unresolved issues. Plans contain local source paths and should be kept private.

SHA-256 identifies source parts and media content. When multiple occurrences have identical media bytes, the archive can store one content file while preserving every occurrence and its album relationships. Raw JSON sidecars are retained separately. Unsupported members and ambiguous metadata are reported rather than silently treated as media or attached to a guessed album.

The `inspect`, `plan`, `export`, `resume`, `verify`, `compare`, and `serve` commands operate on the selected archive scope. `compare` rereads the original source parts and checks them against the plan and exported archive. Export and resume use an exclusive writer lock and a plan-bound staging checkpoint. Verification checks stored bytes against the manifest. The manifest is unsigned, so this verifies consistency with that manifest but does not authenticate who created it.

The core API includes `Inspect`, `WritePlan`, `ReadPlan`, `Export`, `Verify`, `Compare`, and `ReadManifest`. Parsing and copying are bounded, content paths are generated from hashes, and unsafe archive member paths are rejected. Trusted local path ancestors are required; the application is not a sandbox against a hostile process concurrently changing the filesystem.

## Immich transfer flow

The Immich adapter supports server version 3.3.1 exactly. It verifies the selected local archive before network access, then checks the server version, current account, and API-key permissions before making changes. It uploads original media, verifies full original bytes, and checks exact membership for every selected source album. Remote verification is read-only. There is no remote-delete command, and reports describe only the selected archive rather than the contents of an entire Google account.

The API key is read from an environment variable and is never stored in a report or accepted as a command-line value. The key needs these permissions: `asset.upload`, `asset.read`, `asset.download`, `album.read`, `album.create`, `albumAsset.create`, and `user.read`. The adapter does not request `asset.update` or asset-delete permissions. HTTPS is the normal transport. HTTP requires an explicit option and a literal loopback IP address; redirects and proxy use are not supported.

An import report is created before the first remote mutation and checkpointed around each operation. Resume binds the report to the archive manifest, server version, and account. If a network result is uncertain, the adapter reconciles remote state before any retry; it does not blindly repeat an uncertain mutation. Keep reports private because they include source names, album details, server origin, account ID, and operation state.

Google Takeout does not provide the original filesystem modification time. For a ready upload, ArchiveBridge attaches a generated minimal date-only XMP sidecar with the known Takeout UTC date and sets Immich's required `fileCreatedAt` and `fileModifiedAt` fields to that date. The Immich 3.3.1 metadata worker must process the date; ArchiveBridge checks the returned `fileCreatedAt`, `fileModifiedAt`, and `ExifDateTimeOriginal`, along with the complete original bytes, before it marks the upload ready or writes album membership. If cancellation or the bounded worker wait expires after Immich accepted the media, the partial journal retains its remote asset ID for explicit resume. Existing assets with conflicting dates are refused without changing their metadata. The date is not a recovered filesystem timestamp. Raw Takeout JSON sidecars remain in the local archive and are not uploaded; the adapter does not map sidecar caption or GPS fields into Immich. Original media bytes, including embedded EXIF, remain unchanged. Plans and reports expose `dateTransferPolicy` with value `takeout-date-authoritative-generated-xmp-v1`.

## Source metadata and uncertainty

Google notes that Takeout download time can differ from an item's original timestamp; see [Google Photos export help](https://support.google.com/photos/answer/3024190?hl=en-GB). ArchiveBridge uses supported sidecar metadata and verified matching rules. Unsupported naming variations, missing dates, invalid dates, and ambiguous or conflicting matches remain visible for review. It does not infer a date from download time or claim completeness beyond the selected source parts.

Supported formats, resource limits, and release-specific verification scope are described in [supported inputs and limits](support.md) and [verification](verification.md). Evidence for one version or package applies only to that exact release and platform.
