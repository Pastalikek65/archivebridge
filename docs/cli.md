# Command guide

Version note: the latest published package is MVP 0.1.0. It supports the baseline commands below; `compare` and recovery after abrupt process termination are beta 0.2.0 source features and are not in the published 0.1.0 package. MVP `resume` continues after graceful cancellation. Beta recovery behavior remains unqualified pending cross-platform CI and review.

`--source` can be repeated for all parts in one selected Takeout export. Paths containing spaces must be quoted. Write your plan and portable archive outside the source files; keep the selected source parts available when exporting or resuming.

| Command | Result |
| --- | --- |
| `inspect --source part.zip [--source part2.tar.gz]` | Read-only preview of files, dates, relationships and unresolved information. |
| `plan --source part.zip --output plan.json` | Save a version-1 plan to a new file; existing files are not overwritten. |
| `export --plan plan.json --out archive-dir` | Copy verified originals and sidecars into a new, owned archive directory. |
| `resume --plan plan.json --out archive-dir` | Continue the same plan; rehash existing stored files before reuse. |
| `verify --archive archive-dir` | Check the archive's stored files, relationships and ownership against its manifest. |
| `compare --plan plan.json --archive archive-dir` | Re-inspect the original sources and compare the plan, manifest, relationships and stored bytes. |
| `serve --archive archive-dir [--listen 127.0.0.1:4175]` | Read-only localhost viewer; `localhost` is normalized to `127.0.0.1`. |
| `--version [--json]` | Report the application version and recorded build commit. |

Append `--json` for machine-readable results. Each JSON result has `schemaVersion`, `status` and `command`; failures return a nonzero exit and a versioned error. A failed verification retains its report and individual issues. Human output lists selected-source scope and unresolved items.

`compare` requires the source archives referenced by the plan to remain available at their original paths. A `matched` result means those selected sources were freshly inspected and agreed with the plan and archive manifest, and the exported files passed verification. A mismatch exits nonzero with a versioned report. It does not establish that every item in an online account was exported.

Export requires a new directory or one owned by the exact same plan. It refuses unrelated files, symbolic links, changed sources, changed stored files, unsupported plan versions and unsafe archive paths. Never change your original sources to make a failed plan pass; inspect the new selection and write a new plan instead.

The portable archive stores content under `media/sha256` and raw JSON under `sidecars/sha256`. Original names and all occurrences are in `manifest.json`; the viewer downloads media using their original names. Stored files are not renamed into a guessed date or folder. File-system modification times are not the manifest's photo timestamps.

The 0.2.0 beta uses an exclusive persistent writer lock and a plan-bound staging checkpoint to resume after abrupt process termination. Its native CI is designed to exercise an actual killed child and a new child resuming the same archive; qualification remains pending complete cross-platform acceptance and review. Do not remove ownership, lock or staging files to bypass an error.
