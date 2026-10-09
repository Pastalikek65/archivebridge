# Roadmap

MVP 0.1.0 is published for Windows x64 and Linux x64. It provides multipart ZIP/TAR.GZ inspection, explicit plans, original/sidecar export, manifest integrity verification, repeatable export and a read-only local viewer. It records supported dates and album relationships and exposes unresolved information. Its release evidence applies only to that version and the documented platform scope.

Beta 0.2.0 adds original-source/archive comparison and recovery after abrupt process termination. Its Windows/Linux acceptance workflow uses an actual terminated exporter, a second process resuming the same owned archive, published 0.1.0 compatibility packages and measured native-process workloads. This beta remains unqualified until the complete cross-platform workflow and review pass for the exact source and packages. Do not treat local development results as release evidence.

The next product goal adds an explicit Immich adapter, verifying uploaded originals and multi-album membership on a synthetic self-hosted server. Immich transfer is not in MVP or beta. Unsupported metadata fields and untested server versions will remain visible. No deletion of originals or promise of finding every Takeout naming variation is planned.

Later possibilities include additional source formats, optional metadata transformations and filesystems without hard-link support. These are not current capabilities.
