# ArchiveBridge

Turn Google Photos Takeout parts into a local archive with original files, preserved sidecars, dates, album relationships, and an honest integrity report.

[Download](https://github.com/Pastalikek65/archivebridge/releases) · [Quick start](#get-started) · [Türkçe](docs/quickstart-tr.md)

ArchiveBridge is for people who want to keep or move their photos without losing track of what transferred. It works locally, without a Google login, subscription, paid API, or upload service. The portable Windows/Linux command includes a read-only localhost viewer.

The latest published release is MVP 0.1.0. This checkout contains 0.2.0 beta source; its new `compare` command and recovery after abrupt process termination are not in the published 0.1.0 packages. Beta cross-platform checks and review are pending, so a local source build is not a qualified release.

![ArchiveBridge 0.2.0 beta source viewer browsing the included synthetic Takeout example](docs/demo.png)

The real local viewer shows preserved album memberships and dates, with conflicting metadata left unresolved. Try this same synthetic archive using the commands below.

## Get started

Download the published MVP 0.1.0 package from [Releases](https://github.com/Pastalikek65/archivebridge/releases), verify its `SHA256SUMS.txt`, and extract it. Windows users can replace `./archivebridge` below with `.\archivebridge.exe`.

```sh
./archivebridge plan --source takeout-part-1.zip --source takeout-part-2.zip --output plan.json
./archivebridge export --plan plan.json --out my-photo-archive
./archivebridge verify --archive my-photo-archive
./archivebridge serve --archive my-photo-archive
```

Open `http://127.0.0.1:4175` to browse the archive, filter albums and metadata status, download originals, or run integrity verification. Stop the viewer with Ctrl+C. The viewer does not modify the archive.

To inspect before writing a plan, use `inspect --source ...`. MVP 0.1.0 `resume` continues work after graceful cancellation or repeats an export; existing files are hashed before reuse. A mismatched file is reported, never silently overwritten. All data commands support `--json`.

`compare` and recovery after abrupt process termination are beta 0.2.0 source features and require building this checkout; they are not available in the published MVP 0.1.0 download. To try them, install Go 1.27.2 and build from this checkout. On Linux, run:

```sh
go build -trimpath -o archivebridge ./cmd/archivebridge
./archivebridge compare --plan plan.json --archive my-photo-archive
```

On Windows PowerShell, include the executable extension:

```powershell
go build -trimpath -o archivebridge.exe ./cmd/archivebridge
.\archivebridge.exe compare --plan plan.json --archive my-photo-archive
```

The beta remains unqualified until its Windows/Linux CI and review pass for the exact source and packages.

## Try the included example

The package includes two small Takeout-shaped archives made entirely from synthetic images and metadata:

```sh
./archivebridge plan --source examples/sample/takeout-part-1.zip --source examples/sample/takeout-part-2.zip --output sample-plan.json
./archivebridge export --plan sample-plan.json --out sample-archive
./archivebridge verify --archive sample-archive
./archivebridge serve --archive sample-archive
```

The example contains six media occurrences, four distinct media contents, seven sidecars and three albums. One media file has conflicting sidecars; it remains visibly unresolved. Sharing identical stored bytes retains every source occurrence and album relationship. No source file is removed or changed.

## What is preserved

- Original supported photo/video bytes and every JSON sidecar, with SHA-256 identities.
- Source occurrences and album membership, including media appearing in several albums.
- Supported sidecar timestamps as separate UTC manifest fields; original embedded metadata remains in the unchanged original bytes.
- Missing, ambiguous, malformed and unsupported information as visible transfer notes.

ZIP and TAR.GZ parts are read together. Matching uses exact supported same-folder filenames and metadata titles. Archive naming variants outside those rules remain unresolved. Unknown archive members are listed rather than presented as transferred. The result covers only the parts you selected, not your entire account.

Integrity verification checks stored bytes against an unsigned manifest. It does not authenticate that manifest. Plans contain private local source paths; portable manifests omit them. Keep plans and exported metadata private.

Beta recovery retains an owned checkpoint after abrupt process termination and resumes it under an exclusive writer lock. The beta acceptance harness exercises a real child-process kill and recovery on Windows and Linux; those results remain subject to the pending CI run and review. Keep original sources available and do not remove ArchiveBridge ownership or staging files by hand.

See [supported formats and limits](docs/support.md), [the command guide](docs/cli.md), [architecture](docs/architecture.md), and [Türkçe hızlı başlangıç](docs/quickstart-tr.md). [Verification](docs/verification.md) records tested release scope; [the roadmap](docs/roadmap.md) distinguishes current capabilities from planned work.

## Build from source

Go 1.27.2 is required to build this beta checkout; downloaded packages require no Go, Python or Node installation.

```sh
go test ./...
go build -trimpath -o archivebridge ./cmd/archivebridge
```

On Windows, use `go build -trimpath -o archivebridge.exe ./cmd/archivebridge` so the command has the expected executable name.

The browser test and packaging tools are development dependencies, separate from the application. See [contributing](CONTRIBUTING.md). Application code is Apache-2.0; bundled Go runtime/standard-library notices are in [third_party](third_party/README.md).
