# Source review — 2026-10-11

Reviewed the daemon, strategy, journal, OAuth/MCP transport, account-isolation/retirement gates, coordinator accounting, simulation boundaries, acquisition interfaces, and native operator controls. Source-wide verification covered 162 Go files, 174 Python utilities, five shell scripts, and five Swift files. Critical execution and accounting paths received manual review and focused regression checks; syntax/static analysis is not a line-by-line audit of every dated research utility.

## Findings resolved

| Priority | Finding | Resolution |
| --- | --- | --- |
| High | Unchecked account-capital totals could overflow under malformed large holdings and manufacture put capacity. | Checked share multiplication and every commitment addition; out-of-range stock, pending put, and unsettled put exposure blocks planning. |
| High | Activation depended on a release label, which development builds reuse. | Bind activation to the executable SHA-256 as well as release/config; missing or mismatched executable identity disables it. |
| High | Go 1.26.8 was affected by standard-library advisories in the Go vulnerability database. | Require Go 1.26.9 and select patched Go 1.27.2 by default; update the checksum-pinned installer and reject older reviewed patch levels. Final source scan has zero findings. |
| Medium | New configuration selected balanced rather than conservative. | `init` and the example now use the exact conservative 0.05–0.15 put band; account remains unconfigured and trading unauthorized. |
| Medium | The synthetic replay's only liquid symbol had left the default universe, producing zero trades. | Align invented identities with the current universe and retain accounting, Greek, omission, execution, and three-slot assertions. Historical inputs and performance reports were not changed. |
| Publication | Narrow ignores and deployment transfer exclusions could include local private artifacts. | Extend ignores, make both transfer scripts honor them, and publish an allowlisted source export with fresh Git history and redacted secret scans. |

## Verification

- All packages pass `go vet ./...` with Go 1.27.2 and minimum Go 1.26.9.
- 89 distinct focused tests pass: activation/isolation/retirement, durable intent/no replay, broker schema/decimal/receipt/settlement checks, private journal, coordinator admission/uncertainty, capital bounds, causal replay, accounting, and shared-portfolio fairness.
- The README build/init/check-config flow passes in isolated private scratch. The generated configuration is conservative and private; initialization refuses overwrite.
- Darwin/arm64 analysis passes for daemon, broker, and runtime packages. All five Swift files parse; the installed menu app was not rebuilt or exercised.
- All Go sources are formatted; all Python and shell sources pass syntax checks. Local links in the publication resolve.
- `govulncheck v1.9.0` on Go 1.27.2 reports zero findings against the database timestamp in [verification](review-verification.json). The scanner was rebuilt with the target compiler to parse its standard library correctly.
- Gitleaks v8.30.1 scans the source export, compressed fixtures, and new Git history with redaction before publication. Synthetic broker/schema and retirement fixtures are retained; personal evidence is excluded.

## Boundaries retained

Broker facts override local state. A unique submission reference is durable before transport, and uncertain sends are never retried automatically. External/peer shares remain quarantined. A paused TradePulse instance is not retired; exclusive handover still requires the matched stopped-state seal, flat lifecycles, inactive persistently masked service, and lifetime lock. Coordinator observation cannot grant shared execution.

Missing advanced/OCO coverage or other provider facts blocks readiness. No real order, assignment, or live acceptance was tested by this review. Deployment scripts/services and the menu app contain installation-specific host/user/path settings and require adaptation. No deployment, activation, peer-service change, or liquidation was performed.

The weekly portfolio ranks projected ROI from decision-time information and freezes admissions before fills. Retrospective symbol selection, current cash seeded into past dates, and modeled execution remain disclosed limitations. The offline $150,000 ceiling does not change the production $80,000 cap.

The new repository contains source, generic documentation, and synthetic fixtures. OAuth credentials, private configs/state, account/deployment reports, market-data captures, generated research results/binaries, and the prior local Git history are excluded.
