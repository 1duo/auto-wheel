# Readiness

Readiness is evidence for the installed integration, not an order approval. The daemon always repeats cash, ownership, quote freshness, trading-window, broker review, and submission-fence checks before an order.

Required conditions:

- An explicit active Agentic cash account with Level 2+ options; no leverage, unproven balances, pending settlement, or unknown cash obligations.
- Account-scoped holdings and fully paginated standard order history, linked crypto coverage when applicable, and trade-approval coverage. The default full-coverage policy also requires advanced/OCO order coverage.
- Exact standard equity deliverables (100 shares), supported contracts and quantities, fresh usable quotes, and exact fee/collateral review.
- No unresolved submission, unknown option exposure, conflicting equity activity, or appropriated external inventory.
- Verified separate-account isolation, qualified exclusive peer retirement, or a qualified single execution coordinator. Observation alone grants no execution capability.
- A reviewed exchange calendar and a fresh readiness/snapshot pair for native activation.

The activation record binds the executable SHA-256 and configuration as well as the release label. Missing, malformed, stale, or mismatched evidence fails closed. Activation is explicitly requested through the native control after readiness and authenticated with the private installation credential; login, doctor, read-only scans, and observer deployment cannot activate trading.

Nearest-expiry quote collection rejects an empty listed contract set, malformed instruments, missing quote/Greek/OI/tick fields, impossible deltas, and crossed books for the entire symbol scan. Known zero liquidity is an ineligible observation, not missing data. This completeness check does not prove an independent provider timestamp for delta or OI: production still receives those fields with the provider's quote `updated_at`, whose Greek/publication-clock meaning requires separate qualification.

The daemon permits at most three managed symbols, one contract each. Reconciliation rejects duplicate option rows and checks owned contract kind, retained expiry, and put strike; pending intents additionally require the exact candidate contract metadata. These checks run before consuming an intent or changing ownership. Filled puts retain expiry and strike across restart; inventory is adopted only after settled broker facts and ownership evidence. Absence alone cannot prove expiration before the contract expiry day ends.

New put collateral and its configured entry fee must also fit the fixed account ceiling divided by the configured managed slot count. This is $26,666.66 with the default $80,000 ceiling and three slots. The guard is repeated during selection and pre-submission planning. It does not force liquidation or block covered exits for previously owned oversized inventory.

Historical simulation proofs qualify their modeled assumptions only. They do not prove broker acceptance, complete provider tool coverage, or live release readiness.

An explicit `order_coverage_policy: "exclusive_standard"` uses the existing MCP readers without an advanced-order reader. It requires native exclusive retirement, with `exclusive_orders_since` exactly equal to the verified seal's `sealed_at`; moving the cutoff forward is rejected. Only AutoWheel's durable single-leg, one-contract sell-to-open receipts may appear in option history. All equity orders must be terminal with creation and last-transaction timestamps before that cutoff. Outside stock, unsupported portfolio categories, pending approvals and ambiguous facts block execution. A fresh account read after review must still admit the proposed order.

This is restricted readiness, not complete account visibility. `snapshot_complete` remains false, `execution_snapshot_usable` reports supported fact collection, and the native menu displays the coverage warning. Crypto order creation and update timestamps must also precede native retirement. The account must stay exclusive to AutoWheel: no manual, contingent or other-strategy orders. An unseen dormant advanced order cannot be disproved by this policy; observed interference blocks subsequent execution. Broker review is not a reservation or an atomic account-wide transaction.
