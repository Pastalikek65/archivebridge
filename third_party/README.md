# Included components

ArchiveBridge uses the Go standard library and no external Go modules. Its distributed binaries include the Go runtime and standard library under the BSD-style Go license preserved in `GO-LICENSE`.

Application code, synthetic examples, and the embedded viewer are original ArchiveBridge work under Apache-2.0. No Google or Immich application code is bundled. The planned Immich adapter will use a separately operated server's HTTP API; that server has its own license and terms.

The pinned Playwright 1.64.0 developer dependency is Apache-2.0. Playwright and its downloaded Chromium browser are used by verification tools and are not included in release packages. Node and Python are external developer tools; users of the native application do not need them. The exact developer dependency lock is in `scripts/browser-tools/package-lock.json` in the source repository.
