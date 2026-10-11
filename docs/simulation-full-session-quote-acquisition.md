# Full-session quote acquisition

This research controller preserves the full study: 543 option book partitions
and 126 stock days, covering April 6–October 2, 2026 and all 15 frozen symbols.
The real dry-run found four already acquired books and 665 remaining queries.
Acquisition does not qualify a replay or produce performance.

Only one financial writer may run. The book controller refuses execution
while predecessor PID 3940348 exists, requires all 543 statistics operations
to be acquired, and holds an exclusive private book-controller lock. Main
must also confirm the original tool handle is terminal before launching it.
Go metadata commands take the journal's exclusive lock; use them inside the
owned controller, not concurrently with another writer.

The controller retains each new canonical intent and original quote, submit
acknowledgement, response, native acquisition proof and private logs. It never
resubmits a selected prior non-acquired operation. Unknown earlier holds remain
unchanged. Financial actions are serial; the pending window is bounded at 12.

Before transfer, the read-only Go planner validates the job's compressed file
list. Both staging and archive space must fit that exact total with six GiB
remaining. The planned downloader validates the refreshed list and requires
the same names, sizes and hashes, allowing reordered files and refreshed
delivery URLs. It checks six GiB of kernel-reported disk room before transfer.
Both planned and refreshed originals remain private, including rejected
responses. Existing eight-GiB file and 64-GiB job limits remain.

Downloads and offline finalization run serially. After the finalizer exits,
the existing archive helper checks the new source is quiescent, copies it to
the private external archive and exposes a read-only owner-only view at the
original path. Recovery verification precedes release of only the verified
duplicate backup. Proof and receipt bindings and unrelated journal fields are
checked throughout. Helpers retain their original durability, permission,
mount and recovery checks.

Financial sends, downloads, finalization and archive operations are not
automatically retried. Only failed read transports have bounded retries with
fresh evidence paths. An interruption retains existing evidence and holds for
explicit inspection. The controller does not restart itself.

Validation covers the exact full-scope dry-run, real live-predecessor refusal,
17 boundary controls, targeted Go metadata/transfer tests and two narrow
offline CLI builds. Independent reviews and main source inspection support
the accepted components. A new full paid acquisition/archive workflow has not
yet run; the existing archive components were validated on actual stock data.
Total compressed six-month fit remains unproved and is not inferred from one
day's ratio. Every job has explicit capacity checks.

Production trading remains read-only and halted. Historical quotes still
need independent normalization, dated terms and dividend/Greek inputs,
lifecycle integration and full frozen completion checks before any reported
performance.
