# Offline research source

The retained RD1 engine and independent validators are now versioned in this
repository. Recovery used the bound sources from study `rd1-261018-2123`, which
includes conservative TP50/75/90, puts-only comparisons, expanded-capital and
tax-reserve controls. The shared tax modules already on `main` matched exactly.
This source consolidation adds no strategy promotion, provider request or new
historical replay. The live release remains `owned-21590289f660`.

`internal/researchrd1` contains the offline account and replay model.
`cmd/autowheel-rd1-replay` consumes an explicitly supplied, hash-bound packet on
the private Lexar view. `internal/researchcampaign` retains only its exchange
calendar, settlement calendar and fee dependencies. Its small hash helper was
extracted unchanged from the original campaign checkpoint source. The older
R6 execution/pricing engine and legacy live-preset prototypes remain archived.
These packages have no broker client or production journal writer.

The `scripts/rd1_*.py` and `scripts/simple_research_*.py` files preserve the
independent financial, source, effect, availability, attempt and terminal checks
plus their capture/dispatch dependencies. The newer bound validator overrides
take precedence over the older dependency copies. `tax_analytics.py` applies the
existing versioned tax model after independent financial reconciliation.

The original admission fences are intentionally preserved: Linux-only private
Lexar storage, reviewed UUID `62F9-12AF`, create-only artifacts, explicit source
hashes, case reservations, budget limits and no automatic replay retries.
Dispatch scripts still describe their original frozen campaigns; copying their
source does not renew an expired authorization or create a new research plan.
Future experiments require their own explicit admission and binding. Build
executables outside the SSD's `noexec` view and keep data/results on Lexar.

Recovery provenance and file fingerprints are recorded in
[the source manifest](research-source-recovery-20261019.json). Its filename uses
the UTC recovery date. For this recovery revision, validate the copied snapshot
without executing providers or replays:

```sh
python3 scripts/check-research-recovery.py
python3 scripts/test-rd1-financial-verify.py
```

The fingerprints describe this recovery snapshot. A later deliberate source
change is a new revision; preserve the original binding and review its changed
semantics before using it for another experiment. Baseline synthetic tests are
retained from the saved local work, alongside the newer bound TP/capital/tax
tests. Synthetic checks establish accounting behavior, not historical returns
or market-data qualification.

The complete older local/MSI work is preserved on Lexar under
`reduced-data-rd1/rd1-261018-2123/main-sync-261019-0035`, including byte-verified
archives and file manifests. Local recovery references remain
`codex/pre-tp75-main-sync-261019-0035-local` and
`codex/pre-tp75-main-sync-261019-0035-msi`; MSI's previous committed history is
also retained in `codex/pre-tp75-main-base-main-sync-261019-0035-msi`.
Historical plans, host deployment state, old pricing experiments and the larger
menubar rewrite can be inspected there without restoring them over current
production sources. Credentials and market datasets remain outside Git.
