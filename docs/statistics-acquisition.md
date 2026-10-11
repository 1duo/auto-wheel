# Historical statistics acquisition

The research spending authorization is uncapped. The trading collateral ceiling
and production readiness gates are independent and remain in force.

`scripts/acquire-selective-statistics-plan.py` consumes the immutable cycle24
543-query statistics plan for the 126-session study. The plan selects historical
contract identities for acquisition; it does not establish causal listing or
tradability. Each query is a complete UTC day with at most 2,000 raw OSI symbols.

The runner verifies every planned query and the accepted Go research binaries
before starting. It skips an acquired canonical operation and refuses a selected
operation with any other prior financial result. A new private output directory
holds the journal baseline, run plan and durable per-query intent.

For each new partition, the existing Go batch CLI obtains a fresh exact quote,
persists the spending reservation and sends once. The runner then observes the
acknowledged prepaid job through read-only retrieval calls. When ready, it
downloads into a fresh private directory and invokes the existing offline Go
finalizer with the pinned native decoder. Only a complete manifest, native EOF,
record count and original evidence binding permit the journal status `acquired`.
The original hold and all financial fields remain unchanged.

CLI output, provider identifiers, signed transport metadata and licensed source
records stay private on MSI. User-visible output contains aggregate counts,
query-plan dates, costs and hashes. Any financial, observation, download or
verification failure stops the original runner and preserves its evidence. It does
not retry financial requests or release spending holds. A new invocation skips
completed acquisitions; a previously acknowledged pending job requires explicit
inspection and prepaid retrieval, never another purchase.

Acquisition proves delivered bytes only. Causal OI publication, definition joins,
complete historical books, dated contract terms, causal Greeks, execution and
lifecycle qualification remain separate requirements before reporting P&L.

`scripts/acquire-prefetched-statistics-plan.py` runs the same pinned preflight
and CLIs, with a bounded window of 12 acknowledged jobs by default (maximum 24).
It serializes financial calls and spaces their starts by at least four seconds,
below the provider's documented 20 batch submissions per minute. Observations
are spaced by at least 20 seconds per job. A pending job has a six-hour local
deadline; reaching it stops the run and retains all existing jobs and holds.
This window concerns historical acquisitions, independently of the strategy's
three-symbol trading limit.

The prefetched runner checks the entire journal before each action. A submit
may append exactly one row; offline finalization may change only the matching
row's status, proof hash and delivered-byte count, even when it is not the last
row. The journal's response hash binds the cost quote. The acknowledgement's
response hash binds the submit body. These are separately checked against their
original evidence and must not be equated.

Cycle38 retained and inspected the first April8 acknowledgement after that hash
comparison defect stopped the controller. Recovery observes and downloads that
original prepaid job only; it makes no financial request. Eleven targeted
journal controls and the actual acknowledgement binding passed after the fix.
Full production or replay qualification is not implied by these checks.

The prefetched runner makes at most three attempts for the specific generic
read-only observation failure, with ten- and twenty-second pauses and separate
immutable evidence directories. Identity, manifest and other errors stop
immediately. Financial submissions, downloads and finalization are never
automatically retried. Three focused fault controls cover successful recovery,
immediate identity-error rejection and exhaustion of the read-attempt limit.

`scripts/recover-prepaid-statistics-run.py` retrieves acknowledged jobs from an
interrupted run without submitting or quoting. It verifies the original journal
prefix, exact query/operation bindings and pinned Go binaries. Offline proofs
permit only the matching held row's acquisition fields to change. The cycle38
window stopped on a generic observation failure with all12 acknowledgements and
holds preserved; this separate consumer resumes those existing jobs before new
financial work continues.

`scripts/audit-acquired-statistics-scope.py` consumes the immutable cycle40
journal snapshot and its 44 acquired statistics partitions. It does not touch
the live journal or make network, credential or financial calls. Each partition
gets a full native V1 vector audit and a separate field-for-field native read.
The receipt, exact frozen query, decoder version, count, hashes and EOF remain
bound to the acquired original.

The provisional OI views cover all 74 frozen five-minute decisions and the
closing valuation. They retain every requested raw identity and original
nanosecond clocks, use the latest captured copy per publisher, and never sum
copies. A genuine zero remains zero. Missing OI remains absent; disagreement,
unsupported updates or flags, bad event/reference clocks and an unknown
definition join withhold a usable quantity. A delete supersedes the previous
copy instead of leaving an old value usable. An undefined reference stays null
and does not invent a reporting date.

The views and licensed vectors remain private on MSI. A new summary is written
only after the complete bounded batch and source-stability checks pass. Six
clock/quantity/update controls and eight projection boundary controls cover
future captures, equal-time closed cuts, zero, conflicting copies, deletion,
later recovery, absent publication and a missing reference. These controls
and native comparisons establish this component's behavior; the 44-partition
scope is incomplete relative to the 543-partition full-window plan. The views
remain unqualified for replay, with no historical performance claim.
