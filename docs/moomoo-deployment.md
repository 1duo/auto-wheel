# Independent Moomoo deployment

The authorized goal is conservative AutoWheel with **five managed slots**, one contract per symbol, on a separate **US Moomoo account**, independently deployed on **MSI Linux**. The Mac menubar provides unified status and automatically refreshed holdings for both brokers. Existing Moomoo holdings remain quarantined and unclaimed.

## Current boundary — October 27, 2026 (afternoon)

MSI release **`449f1492aaed2e91`** is active with the production `ExecutionSource` registered and the five-slot conservative ASAP policy bound to the sealed US business account (`moomoo_complete` coverage, order cutoff `2026-10-27T17:46:33Z`). Status reports `live_capable=true`, `halted=true`, no `LIVE.json`. The account-bound trading consent was granted at 00:2x UTC on October 28; the saved grant is exactly `quote:read trade:read trade:write accid:<sealed account>`. The fixed controller then bound its fresh journal and **native readiness passes**. Native activation (the menubar Live switch) is the only remaining operator action. Robinhood remains live on `review-65bb40a1a7dd` with its process, release and configuration digest unchanged.

The read-only production-path audit (`audit-execution`) passed against the real account: fresh complete snapshot, settled cash after every obligation, quarantined external holdings, no managed positions or post-cutoff orders, and all 10 roster symbols selected, refreshed and reviewed with account capacity (`max_sell_short ≥ 1`) and fee bounds of $3.04–$3.06 within the $4.00 budget. No order was prepared or sent.

Bound policy: `conservative-rd1-floor-tp75-asap-v3`, the same researched roster as Robinhood (AMD, ORCL, INTC, CRM, BAC, XOM, WMT, BA, JNJ, CVX; source `99aa128a…`), $80,000 account and symbol ceilings, $0 reserve, $4.00 option fee budget per one-contract order, $10.00 delivery fee budget, 30-second quote age, 30-second refresh.

## Production execution source

`internal/moomoo/source.go` and `source_market.go` implement the source. Each rule is one-sided: an unrecognized fact blocks execution.

- **Settled cash, obligations removed exactly once.** Let `L = min(us_cash, us_avl_withdrawal_cash) − frozen_cash − pending_asset`, `R` = every short-put strike×100 (owned and external) plus every uncompleted-order reserve, and `H = us_cash − us_net_cash_power` (Moomoo's own visible holds). Then `AvailableCash = min(L, us_net_cash_power) − max(0, R − H)`. Moomoo enforces cash-secured puts against net cash power in this margin account (the `max_sell_short` reported for a sample put matched net cash power divided by the strike collateral, not the much larger margin buying power), so whatever it holds is not subtracted twice, and cash never exceeds its net cash power. If withdrawable cash also reflects the hold, the overlap only understates cash: the snapshot then funds no new risk. It fails only if obligations exceed the cash ledger itself, or any currency balance is negative; execution never borrows. The per-order `acctradinginfo` review is a second, broker-side cash-secured check.
- **Order coverage.** `/orders` documents *every uncompleted order* plus fills and cancels from the last 24 hours; `/orders_history` filters by creation time. `configure-execution` proves every order since 2000 terminal and sets the cutoff to that audit time. Each snapshot reads `/orders` fresh plus history since the cutoff (history reused at most 25 seconds). A pre-cutoff order still working or changed after the cutoff stays visible and freezes.
- **Quarantine.** Stock and options on symbols the journal does not manage go to `ExternalSymbols`, never `Shares`. External short puts reserve cash; an uncovered external short call, short stock, a non-USD position or an unparseable option code blocks.
- **Settlement legs.** This account's history shows Moomoo settles expirations as option `BUY_BACK` at $0 and assignments/call-aways as that leg plus a stock leg of exactly 100 shares at the strike, with the same whole-second clock and no remark. Only pairs for contracts this journal sold, created after its own opening, are recognized; anything else remains an outside order and freezes. Position `cost_price` is a running average, so an owned assignment's basis is the broker's assignment fill price.
- **Chain completeness.** The [option-chain reference](https://open.moomoo.com/api/quote/derivatives/option-chain) documents no pagination, only a 20-expiry cap; each 20-calendar-day subrange cannot exceed it. The live endpoint ignored the undocumented `limit=100` (returned 432 rows). A continuation page without pagination facts remains incomplete.
- **Ticks.** The snapshot omits a tick. US option classes use 1¢/1¢, 1¢/5¢ or 5¢/10¢ (below/at-or-above $3); a displayed NBBO price off the coarser grid proves a finer regime, otherwise the coarsest valid tick is used.
- **Account review.** `GET acctradinginfo` for the exact code and limit must return `max_sell_short ≥ 1` (opening) or `max_buy_back ≥ 1` (closing); deliverables must be standard 100-share contracts. Opening side `SELL_SHORT`, closing `BUY_BACK`, matching this account's history.
- **Fees.** Actual fee documents for this account: one-contract option orders $2.98–$3.02 ($1.99 commission, $0.30 platform, $0.65 contract, regulatory cents); assignment/expiration legs $0; call-away stock sales $0.11–$0.52. `configure-execution` re-verifies recent actual fees against the bound and binds their SHA256.
- **Rate limits.** Moomoo limits chain and trading reads to about 10 per 30 seconds. The cycle observes at most four roster symbols, rotating the start; the refresh interval is 30 seconds.
- **Trading consent.** Moomoo registration accepts only `accid:*`; the consent page binds one account into the token. Registration must be exactly `quote:read trade:read trade:write accid:*`, and the issued token must name exactly the sealed account or it is rejected and not saved.

Targeted tests (`go test -race ./internal/moomoo ./cmd/autowheel-moomoo`) cover the snapshot mapping, quarantine and obligations, settlement legs, an owned assignment through a real native ASAP cycle and the fixed controller, call-away, ticks, fees, capacity review, chain completeness, observation rotation and trading registration.

## Residual risks reviewed before activation

- **First fill confirms the cash representation.** No short put was open when this was qualified. After the first fill, compare `us_cash`, `us_net_cash_power`, `us_avl_withdrawal_cash` and `frozen_cash`; every possible representation is handled without over-crediting, but one of them (hold only in withdrawable cash) leaves no new risk funded until positions close.
- **Order-confirmation responses fence trading.** Moomoo documents placement responses -2100 (confirmation required) and -2101 (browser action). Both ask a human to act, so they stay unknown and stop trading until an operator confirms in the app that no such order exists. Other outcomes resolve automatically: a server-side rejection becomes a `FAILED` order carrying its reference; any other application error (`s:"error"` with an error code) is released only when a complete order audit at least 60 seconds later contains no order with that reference; a record proving no dispatch (prepared, denied before sending, or never written) releases its intent. Timeouts and 5xx responses are resolved only by their receipt.
- **Attempt retention.** Up to 1024 attempt records are kept; at that limit trading stops with an explicit archival message. The wheel's completed-order ledger has the same 1024 operator-archival limit.
- **Failed cycles block only themselves, as on Robinhood.** A read failure (network, maintenance, rate limit), ambiguity or reconciliation error shows `blocked` with its reason and publishes readiness false; the final pre-order guard requires fresh readiness, so nothing is sent until a later cycle re-reads and reconciles every fact. Live stays on. Unknown outcomes and outside activity still block every cycle until resolved. Only uncertain journal durability stops the daemon, which then restarts paused.
- **Restarts still require activation.** Unlike Robinhood, which keeps a valid release/config-bound record across restarts, every Moomoo process start pauses and needs Live switched on again (including after each deployment).
- **Order side.** Openings are `SELL_SHORT` and closes `BUY_BACK`, matching the account's own history and `acctradinginfo` (`max_sell_short`, `max_position_sell=0`, `max_buy_back`). The vendor SDK sample uses only BUY/SELL; no API-placed order exists in this account's history yet.
- **Roster window.** The bound roster applies through 2026-12-31; openings stop afterward until a new reviewed roster is bound.

## Remaining operator steps

1. **Trading consent — done.** For renewal: with `autowheel-moomoo.service` stopped and port 8766 forwarded, run `autowheel-moomoo login-trading` on MSI and approve the Moomoo page, choosing the same account. The service restarts afterward.
2. **Activation — pending.** When the menubar shows Moomoo ready, switch it to Live (`/control/live`). Every restart or operator pause requires a fresh activation; a failed cycle blocks only itself.

Manual orders in this account after the cutoff freeze AutoWheel until reviewed; keep the account exclusive to AutoWheel.

## Earlier boundary (superseded)

MSI release `081b535b4f55d26b` had native components but no registered source; the gaps it listed are resolved above.

## Implemented native execution components

- Separate trading OAuth registration, consent and refresh validation require exactly `quote:read trade:read trade:write accid:<sealed-business-ID>`. A trading credential has its own private file. Observation credentials retain their original read-only scope checks. No trading grant has been requested or saved in production.
- The pinned REST writer accepts one-contract US option LIMIT orders with explicit DAY or GTC duration and exact integer-cent prices. Redirects, HTTP replay and automatic retries are disabled. The final native guard runs after any token refresh and immediately before the request.
- The engine persists its reference before submission. The independent outbox persists terms/request hash and an attempt marker before dispatch, then saves bounded response evidence before releasing an acknowledgement. Transport errors, malformed responses and challenges retain unknown outcomes. Fresh exact receipt recovery may acknowledge an existing attempt; absence never authorizes replay or a replacement reference. Unresolved and orphaned attempts survive restart and fence further trading.
- The locked journal connects to the five-slot ASAP controller and resting fee-aware TP75 exits. At most one successful dispatch occurs per cycle. Account-wide coverage uses the separate `moomoo_complete` policy; Robinhood retirement/coordinator seals cannot satisfy it. External underlying symbols are excluded from entry and existing shares cannot be adopted.
- Authenticated native activation binds account, policy, release, configuration SHA256 and actual executable SHA256. Readiness must be fresh. Pause serializes with financial dispatch and revokes the activation record. Every process restart requires fresh native activation. Native control preparation is bounded by an eight-second deadline.
- Status publishes a copied execution snapshot, owned positions and durable intents separately from display-only observation. The menu requires the correct deployment, native execution, qualified fixed policy, complete coverage and fresh readiness/snapshot before offering Moomoo activation.

Targeted tests exercise these paths against synthetic qualified broker evidence and fake transports. They do **not** qualify production broker fields or demonstrate a real financial submission. See [the self-review](moomoo-review.md).

## Cash and market input implementation

`AuditAccount` now maps documented USD cash, USD withdrawable cash, funds on hold and assets in transit using checked integer cents. Every digit is validated; asset fractions are floored and deduction fractions are ceiled. The provisional liquidity component is `max(0, min(floor(us_cash), floor(us_avl_withdrawal_cash)) - ceil(frozen_cash) - ceil(pending_asset))`, with saturating subtraction. Holds/transit may overlap the withdrawal restriction; no undocumented offset is credited. This computation is a conservative input component, **not** `Snapshot.AvailableCash` and not proof of fully settled, unleveraged collateral.

Documented currency balances are checked separately. Missing, blank, invalid or newly introduced currency fields remain unknown; even a sub-cent negative balance is retained as a debit. The authenticated US response currently leaves `au_cash`, `kr_cash` and `my_cash` blank. These have not been treated as zero or as evidence of borrowing absence. Incomplete cash facts do not suppress independent holdings observation.

Private account evidence now retains lossless base64 source bytes as well as human-readable raw fields. This fixes a provenance issue: JSON pretty-printing can reformat a `RawMessage`, so a saved raw field alone cannot reproduce the original source hash. The cash input hash is verified against the lossless bytes. An independent decimal calculation against fresh authenticated evidence matched the Go mapping.

`audit-market --quote-codes <comma-separated-US-codes>` reads only two pinned, non-mutating POST resources: stock basic information and market snapshots. It verifies the sealed authorized US account first. The quote API uses `ret_code/data`, unlike the account API's `s/d`. Exact requested-code coverage is mandatory; missing, foreign, duplicate or aliased identities fail. Prices parse directly into cents, integer sizes/clocks retain their units, and source bytes/hashes survive private persistence. Redirects and replay remain disabled. Quoted or fractional quantities, sub-cent book prices and impossible/future timestamp units fail.

Authenticated reads for a stock and an option succeeded, including exact 100-share option metadata and underlying identity. Their snapshots omitted `price_spread`; it remains unknown. A reported zero spread is preserved as a fact, without inventing a positive execution tick. A generic snapshot update clock is not promoted to independent bid/ask venue clocks. Market evidence always remains `execution_qualified=false` until the remaining financial/contract gates are qualified.

## Option-chain input and identity selection

`audit-chain --chain-symbol AAPL` reads only the explicitly named US underlying through the existing read-only grant and sealed account. It derives the current Eastern session's complete 21–45-calendar-day policy window, then splits it into inclusive days 21–40 and 41–45. Each subrange can contain at most twenty expiry dates, respecting the [chain endpoint's twenty-expiry cap](https://open.moomoo.com/api/quote/derivatives/option-chain) without narrowing the strategy or assuming weekend rules.

The collector follows the [documented top-level quote pagination contract](https://open.moomoo.com/api/quote/pagination): an explicit `has_more=false` terminates a range. Opaque cursors are preserved, repeated/missing cursors and changing/contradictory totals fail, and bounds cover pages, bytes and identities. Missing pagination is retained as incomplete evidence and cannot produce a selection menu. All requested standard rows must have the exact underlying, side, expiry, 100-share lot and non-index classification. Decimal strikes are parsed into checked cents and must agree with the provider code's root/date/right/strike; no guessed wire-scale conversion is attempted.

Complete in-process collector results can supply a copied menu to the existing deterministic `contractpolicy.Choose`. It preserves nearest-to-30-day expiry, the conservative put/call strike rules and identity selection before liquidity/fee/permission review. An incomplete or stale result cannot select; a saved report's completion flag cannot recreate the in-process capability. This layer creates no claim, intent, owned position or financial authority.

The authenticated MSI audit returned **432 standard AAPL contracts** in two responses for **2026-11-17 through 2026-12-11**, with source hashes verified. Both responses omitted the documented terminal pagination object. Consequently `complete=false`, no qualified selection menu is available and execution remains unqualified. The public [vendor OpenD chain helper](https://github.com/MoomooOpen/moomoo-agent-hub/blob/main/Moomoo%20API%20Skills/moomooapi/scripts/quote/get_option_chain.py) uses a different SDK interface without cursor handling; it does not establish that this REST response is complete. The date-bounded full-array interpretation and the general REST pagination documentation require reconciliation rather than an assumed exception.

Targeted tests passed for full-window coverage, cursor/total limits, exact standard identity, existing conservative selection, stale/copied/persisted menu handling and CLI input validation. Account and trading credentials, existing holdings and Robinhood remain unchanged. Private chain evidence is `moomoo-option-chain-audit.json`; rollout checks are under `~/.local/state/autowheel-moomoo-chain-rollout/`.

## What the official sources establish

The agent researched these sources directly; the user does not need to locate API documentation or repeat sign-in.

| Area | Verified source finding | Remaining qualification |
| --- | --- | --- |
| USD collateral | [REST funds](https://open.moomoo.com/api/trading/account/get-funds) names `us_cash` as USD cash and `us_avl_withdrawal_cash` as USD withdrawable cash. Both are present in the authenticated evidence. The [US account definitions](https://www.moomoo.com/us/support/topic4_247) distinguish settlement-affected withdrawable cash from maximum withdrawal, which may include financing/unsettled amounts. The [US withdrawal guide](https://www.moomoo.com/us/learn/how-to-withdraw-money-from-moomoo-in-us) identifies withdrawable cash with settled funds. | A conservative calculation must also reserve all existing obligations, pending activity and outside option exercise exposure, with exact currency/decimal handling. `available_funds`, maximum withdrawal and buying power are not executable cash proofs. The cash-input layer now floors assets, ceils deductions, and records unavailable/debit currency fields. Its liquidity component is not an executable collateral budget; complete obligation reconciliation remains unimplemented. |
| Assignment and settlement | [REST cash logs](https://open.moomoo.com/api/trading/account/get-cash-logs) provide descriptions, times, cash changes, balances and symbols. The bounded authenticated collector works and preserves raw bytes. | Reviewed REST schemas do not establish stable assignment/exercise/expiration identities or related stock/cash settlement reconciliation. Option disappearance, balance changes and guessed description parsing cannot establish owned assignment. |
| OpenD cash-flow alternative | [OpenD cash-flow limitations](https://openapi.moomoo.com/moomoo-api-doc/en/trade/get-acc-cash-flow.html) explicitly exclude **Moomoo US accounts**. | This endpoint cannot unblock the US deployment. The prior suggestion to complete OpenD login for that evidence was incorrect. The optional gateway remains stopped; no additional OpenD authentication is required for current work. |
| Options permission | [US options levels](https://www.moomoo.com/us/support/topic4_316) cover cash-secured puts and covered calls. A signed-in portal displayed an existing options level without any settings change. | That portal account identifier has not been authoritatively mapped to the sealed REST business account. The authorized-account response and [maximum-quantity API](https://open.moomoo.com/api/trading/trade/get-max-qty) do not by themselves prove the exact account's strategy permission. |
| Contracts, quotes, ticks and fees | [Basic information](https://open.moomoo.com/api/quote/basic-data/stock-basicinfo), [option chains](https://open.moomoo.com/api/quote/derivatives/option-chain), [snapshots](https://open.moomoo.com/api/quote/realtime/market-snapshot) and historical order-fee samples are available. | The basic-information and snapshot reader is implemented and authenticated stock/option samples passed. The chain collector now exists, but the authenticated REST response omitted terminal pagination facts, so its menu remains unqualified. Signal, permission and fee integration remains unfinished. Complete identity menus, executable increment rules, quote-clock meaning and prospective total fee bounds still need qualification. An omitted `price_spread` cannot be replaced with an assumed tick. |
| Order obligations and recovery | Scoped recent/history pagination and exact UUID receipt recovery are implemented. The [order API](https://open.moomoo.com/api/trading/trade/place-order) supports the encoded limit order path. | Complete coverage of dormant conditional, attached, parked, algorithmic, multi-leg and other-market obligations still needs a supported interpretation. Transport acknowledgement establishes an order ID, not a fill or ownership. |

Fresh authenticated discovery after the native rollout still returned **105 MCP tools**, with no additional assignment/settlement reader identified. There is no MCP financial-tool invocation in the observer. Native financial transport uses the separately scoped REST client only after every execution gate passes.

There is **no requirement to manually exit existing holdings**. Liquidating them would not resolve missing API semantics. Their underlying symbols and obligations must remain quarantined/reserved; they are never available for AutoWheel covered calls. No liquidation or account-setting change was performed.

## Deployment and native control

| Component | Robinhood | Moomoo |
| --- | --- | --- |
| MSI service | `autowheel.service` | `autowheel-moomoo.service` |
| MSI listener | `127.0.0.1:9841` | `127.0.0.1:9842` |
| Mac tunnel | `127.0.0.1:19841` | `127.0.0.1:19842` |
| MSI config/state/release stem | `autowheel` | `autowheel-moomoo` |
| Mac operator credential | `operator-token.json` | `moomoo-operator-token.json` |
| OAuth callback | `8765` | `8766` |

Each Moomoo process holds its own lifetime state lock and sealed business-account binding. Credentials and evidence remain private on MSI. Account IDs, token contents, balances and raw holdings are not included in source or deployment logs.

`scripts/deploy-moomoo-msi.sh` uploads unique source trees, builds only Moomoo, checks source/binary hashes, serializes rollout and installs an immutable release. The current observation-only migration preserves the explicit account, symbols, capital ceiling and reserve while recording the five-slot target and durable pause. It refuses a trading journal or activation record; it must not be used to migrate an already trading deployment. Failed rollout restores the previous release/unit/configuration while retaining pause. No deployment script activates trading.

The current policy support was brought forward from the source archive bundled with Robinhood release `review-65bb40a1a7dd`, SHA256 `8efbe0fcc81f4cf532bad796147de117419ee28fed346631219e0b0cdf6f51ed`. Legacy fixed policies retain their own slot/digest limits. No Robinhood financial evidence, account, journal or activation was imported.

Native `/control/live` performs fresh preparation and creates activation only after authenticated readiness. `/control/halt` durably pauses and revokes activation; it does not cancel accepted broker orders. `/control/resume` can resume observation but cannot create live permission. Unknown submission outcomes never clear through pause, resume, restart or credential refresh.

## Maintenance and evidence

The existing browser OAuth grant already authenticates the gateway-free REST/MCP observer. Initial consent/account binding is complete. Do not repeat it to resolve financial semantics or substitute a universal portal account number for the sealed business ID.

Administrative commands take the same state lock as the service. Stop **only** `autowheel-moomoo.service`, run the installed command on MSI, and restart that service in a guaranteed cleanup path:

- `doctor`: fresh authorized accounts, exact funds/positions, observation and MCP tool schemas, saved privately.
- `audit-chain --chain-symbol <ticker>`: the complete conservative date window in bounded subranges, with explicit pagination and identity checks. An incomplete packet is retained as evidence and cannot supply a native menu.
- `audit-market --quote-codes <codes>`: bounded, exact-identity basic-information and snapshot inputs for explicit US instruments, saved to `moomoo-market-audit.json`; no chain/tick/permission claim.
- `audit-orders --orders-since <RFC3339-time>`: bounded recent/history pagination for the explicit past interval; no assumed complete history.
- `audit-transactions --transactions-since <RFC3339-time>`: bounded update-time fill history and cash logs, preserving exact original payload bytes and hashes. A capped/partial run is rejected.
- `login-trading`: separate exact-account trading consent. It currently refuses the unqualified configuration before opening any consent page. Use it only after the production financial policy/source is qualified; it does not itself activate trading.

Read-only evidence remains under `~/.local/state/autowheel-moomoo/`. Native rollout verification is under the separate private `~/.local/state/autowheel-moomoo-native-rollout/` directory. No support message has been sent and no production order has been placed by this integration.

The optional OpenD diagnostic installation remains isolated under `/opt/autowheel-moomoo-opend/10.11.7108` and `/var/lib/autowheel-moomoo-opend`, using a dedicated OS identity and loopback port 11112 when deliberately launched. Its stopped qualification service is not a prerequisite or a settlement-data solution for Moomoo US. The Python collector permits initialization/read protocol IDs only and cannot unlock or trade.
