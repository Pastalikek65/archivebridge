# Verification

ArchiveBridge MVP 0.1.0 has published Windows/Linux packages with version-specific acceptance evidence. This checkout is beta 0.2.0: added compare and recovery after abrupt process termination are awaiting complete Windows/Linux CI and review for the exact source and packages. Unit tests, local synthetic runs and development builds do not qualify the beta.

`verify` checks exported bytes and structure against the unsigned manifest. It does not authenticate the manifest or compare a full Google account. MVP 0.1.0 does not claim forced-termination recovery; beta 0.2.0 is testing that behavior and remains unqualified pending its platform gates. Neither version includes a completed Immich adapter. Supported scope and limits are in [support](support.md).

Each published package includes SHA-256 checksums and versioned evidence binding its source, platform packages, acceptance results and known limitations. No pilot, download count or broad real-user success is claimed.
