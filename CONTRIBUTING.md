# Contributing

Use synthetic examples, never personal photo exports or real account credentials. Keep data-preservation rules explicit and add a regression that reproduces a bug before fixing it. An ambiguous metadata match must remain unresolved; do not weaken checks to make a fixture pass.

With Go 1.27:

```sh
go test ./...
go vet ./...
go build -trimpath -o archivebridge ./cmd/archivebridge
```

Keep `internal/bridge` responsible for archive and relationship integrity, `cmd/archivebridge` for command behavior and `internal/webui` for the read-only local viewer. Browser and packaging tools are developer-only and are not application runtime requirements. Release candidates must be checked using the actual distributed files on both declared platforms.

Describe the user-visible problem, a synthetic reproduction, the resulting behavior and verification in pull requests. Changes to versioned plans/manifests must include compatibility behavior and documentation. Apache-2.0 applies to contributions; preserve third-party notices.
