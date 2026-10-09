# Security policy

Report security issues privately through this repository's GitHub security reporting page when enabled. Do not attach personal Takeout exports, local plans, account tokens or private metadata to a public issue.

ArchiveBridge handles local untrusted archive structures with bounded parsing, generated output paths and integrity checks. It relies on trusted filesystem ancestors and a trusted local browser/OS; it is not a sandbox for hostile concurrent file replacement. Portable manifests are unsigned, and metadata can contain personal information. Only explicit user commands should transfer data to a future external adapter.

Security support scope follows the latest qualified release. Versions and platforms still under development are not represented as qualified. The repository's release evidence states outstanding limitations.
