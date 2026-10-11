# Full-plan statistics audit

`scripts/audit-all-acquired-statistics.py` audits every acquired operation in an
immutable journal snapshot against the frozen 543-query statistics plan and its
126 exact session dates. Missing operations remain explicit. Reserved or
unrelated operations cannot enter the audited set. Duplicate operations and a
proof hash reused for multiple operations fail the run; identical copies of one
proof are deduplicated.

Discovery combines qualification-manifest proofs with the owned acquisition
controller's proof directory. Every selected operation must pass the original
`validate_batch_acquisition` chain: reservation, canonical query, acknowledgement,
job, receipt, complete file set, byte counts, chronology and native EOF evidence.
The runner reads the journal snapshot, never the live journal or credentials.

The accepted 44-partition native and Go summaries serve as a sealed cache. All
their code, decoder, binary, definitions, proofs, vectors, reference views and
outputs must remain hash-identical. Cached views are compared again at all 75
frozen closed cuts. New partitions run both native audit passes, independent
seventeen-field reconstruction, Python OI projection, the previously validated
Go exporter and complete Go/Python view comparison. The runner supports the full
543-member plan without changing the frozen strategy or replay engine.

The cumulative qualification metadata has a separate 32 MiB strict JSON limit;
the financial-proof reader retains its 4 MiB bound. The exact accepted decoder
venv executable may be a symlink; its name and bytes are pinned. Data paths
remain canonical. Licensed rows and quantities stay in private MSI outputs.

The report contains per-date expected/acquired/audited counts and exact missing
query filenames. `coverage.full_plan_audited` becomes true only after all 543
members have been audited. This is statistics-plan coverage. Full source
qualification and performance flags stay false until the other causal, term,
Greek, execution, lifecycle and independent review gates pass.
