# ArchiveBridge

Turn Google Photos Takeout parts into a local archive with original files, preserved sidecars, dates, album relationships, and an honest integrity report.

[Download](https://github.com/Pastalikek65/archivebridge/releases) · [Quick start](#get-started) · [Türkçe](docs/quickstart-tr.md)

ArchiveBridge is for people who want to keep or move their photos without losing track of what transferred. It works locally, without a Google login, subscription, paid API, or upload service. The portable Windows/Linux command includes a read-only localhost viewer.

![ArchiveBridge browsing the included synthetic Takeout example](docs/demo.png)

The real local viewer shows preserved album memberships and dates, with conflicting metadata left unresolved. Try this same synthetic archive using the commands below.

## Get started

Download a package from [Releases](https://github.com/Pastalikek65/archivebridge/releases), verify its `SHA256SUMS.txt`, and extract it. Windows users can replace `./archivebridge` below with `.\archivebridge.exe`.

```sh
./archivebridge plan --source takeout-part-1.zip --source takeout-part-2.zip --output plan.json
./archivebridge export --plan plan.json --out my-photo-archive
./archivebridge verify --archive my-photo-archive
./archivebridge serve --archive my-photo-archive
```

Open `http://127.0.0.1:4175` to browse the archive, filter albums and metadata status, download originals, or run integrity verification. Stop the viewer with Ctrl+C. The viewer does not modify the archive.

To inspect before writing a plan, use `inspect --source ...`. To continue a gracefully interrupted transfer or repeat it, use `resume --plan plan.json --out my-photo-archive`. Existing files are hashed before reuse; a mismatched file is reported, never silently overwritten. All data commands support `--json`.

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

See [supported formats and limits](docs/support.md), [the command guide](docs/cli.md), [architecture](docs/architecture.md), and [Türkçe hızlı başlangıç](docs/quickstart-tr.md). [Verification](docs/verification.md) records tested release scope; [the roadmap](docs/roadmap.md) distinguishes current capabilities from planned work.

## Build from source

Go 1.27 is required to build; downloaded packages require no Go, Python or Node installation.

```sh
go test ./...
go build -trimpath -o archivebridge ./cmd/archivebridge
```

The browser test and packaging tools are development dependencies, separate from the application. See [contributing](CONTRIBUTING.md). Application code is Apache-2.0; bundled Go runtime/standard-library notices are in [third_party](third_party/README.md).
