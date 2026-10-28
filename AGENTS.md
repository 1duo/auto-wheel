# AutoWheel

Keep the wheel small: one contract per symbol, at most five managed symbols, one account. The user explicitly authorized five slots for the independent Moomoo deployment; existing policy-specific limits remain bound to their own policy.
Use Go 1.26+ and the Modern Go Guidelines CLI. No AI model in the trading loop.
Money and quantities use checked integers; decimal parsing must be exact.
Broker facts override local state. Missing or ambiguous data blocks orders.
Never retry an order submission automatically. Persist its ref_id before sending.
Paused is the deployment default. Native activation requires authenticated readiness and a release/config-bound record. External stock is quarantined, never appropriated. Never put credentials in source or logs.
TradePulse owns the existing account/stock lifecycles. Independent strategy writers require distinct account bindings. Sharing is allowed only through the qualified single execution coordinator: exact imported ownership, one cash ledger, restricted strategy capabilities, release/policy binding, compatible rollback, and retirement of direct financial writes. Observation deployment does not authorize shared trading. Never bypass these gates or appropriate peer shares.
The current authorized production goal retires TradePulse for now and makes conservative AutoWheel the sole writer. A same-account exclusive handover requires TradePulse lifecycles flat, its native stopped-state seal verified against the account and database, its system service inactive and persistently masked, and AutoWheel holding its operational lock for its lifetime. A paused peer is not retired. Preserve software-stop protection until the wind-down is resolved; do not liquidate peer holdings or treat them as AutoWheel inventory. Keep the paused historical acquisition and shared-migration work off this goal's critical path.
Use only targeted safety tests when code changes require verification.
