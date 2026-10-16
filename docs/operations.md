# Operations

AutoWheel starts read-only. Keep config, OAuth tokens, journals, account discovery, and activation records in private directories outside the repository. Config/token/state files require private permissions; symbolic links are rejected for sensitive reads. One journal lock protects each daemon state directory.

Build and initialize using the README. `check-config` validates local configuration without broker access or trading effects. `doctor` authenticates and checks broker compatibility; `audit-account` is read-only and writes private evidence. The account must explicitly support Agentic trading, cash-only buying power, and Level 2+ options.

## Connect and run

Complete Robinhood's [Agentic Trading onboarding](https://robinhood.com/us/en/support/articles/agentic-trading-overview/) and [connection setup](https://robinhood.com/us/en/support/articles/trading-with-your-agent/). The official MCP endpoint is `https://agent.robinhood.com/mcp/trading`. External agents use the designated Agentic account; an explicit cash account with Level 2+ options is required.

```sh
umask 077
mkdir -p "$HOME/.local/state/autowheel"
bin/autowheel login --state "$HOME/.local/state/autowheel"
bin/autowheel doctor --config "$HOME/.config/autowheel/config.json" \
  --state "$HOME/.local/state/autowheel"
```

Open the authorization URL printed by `login` and authenticate in your browser. OAuth uses PKCE and a loopback callback on port 8765 (`--callback-port` overrides it). For a remote daemon, forward that port to the browser machine. Stop any daemon holding the same journal lock before running setup commands.

`doctor` saves private discovery to `$HOME/.local/state/autowheel/mcp-accounts.json`. Copy the exact Agentic account number into the private config's `account_number`, select symbols and cash reserve, then repeat `doctor`. See [the example config](../deploy/config.example.json). Tokens stay in the private state directory.

```sh
bin/autowheel check-config --config "$HOME/.config/autowheel/config.json"
bin/autowheel run --config "$HOME/.config/autowheel/config.json" \
  --state "$HOME/.local/state/autowheel"
```

Status is at `http://127.0.0.1:9841/status`; the read-only weekly P&L calendar is at `/calendar`. These commands do not authorize trading. In the optional macOS menubar app, **Open Weekly P&L Calendar** opens a bundled offline copy or the daemon page. Use **Live / Simulation**, quarter navigation and **Add reports…** to inspect results. Weekly P&L measures equity changes, not premiums; blocked reports and missing valuations stay unavailable. Imported reports are local and never uploaded. Attached private reports retain their recorded fee assumptions; the latest 2026 revision uses dated Robinhood fees and no price allowance. Legacy estimates remain labelled. Reports are excluded from source and daemon assets. Live P&L remains unavailable until verified performance history exists.

The loopback operator API checks its Host header. Every native control request requires an empty POST body, no Origin header, `X-AutoWheel-Control: confirm`, and `Authorization: Bearer <installation token>`. The daemon creates a random 256-bit credential in the private state directory's `operator-token.json` on its first `run`, while holding the journal lock. It never publishes the credential in status or logs. Accepted controls are logged without credentials. Missing, malformed, public, or symlink credential files block control; an invalid existing file also blocks daemon startup. The public confirmation header alone cannot authorize any mutation.

For the menubar app, securely copy that JSON file to `~/Library/Application Support/AutoWheel/operator-token.json` on the Mac. Create the directory with mode 0700 and the file with mode 0600, owned by the Mac user; keep the credential outside the application bundle and source checkout. Use the existing authenticated SSH connection for transfer, and never print the token or put it in command arguments. Status/calendar reads continue without a token. Existing menubar binaries require the updated client and this pairing before controls work.

The credential authenticates possession by the trusted operating-system user; it does not isolate malicious code already running as that user or root. Keep the listener on loopback and remote access within the authenticated SSH tunnel. To rotate, pause trading, stop the daemon, replace the credential with a new random 64-character lowercase hexadecimal string encoded as a JSON string (or remove it and restart to generate one), and securely pair the Mac again. Each mutation re-reads the file, so removal or replacement immediately rejects the previous token. Rotation itself does not cancel accepted orders or revoke an existing activation record.

## Trading authorization

The normal `run` command has no order transport. `--live` is rejected for daemon operation. A reviewed deployment may use `--live-capable`, which permits readiness checks and the native activation control but does not itself authorize orders. Activation requires fresh broker facts qualified for the explicitly configured order-coverage policy, no unresolved intent, compatible quotes/review, a known exchange calendar, and verified account isolation. Restricted standard-order readiness discloses unknown advanced/OCO visibility in the native menu; see [readiness](readiness.md).

The private `LIVE.json` record binds the release label, executable SHA-256, configuration digest, and any coordinator permission epoch. Old records without an executable digest cannot authorize trading. Rebuilds and configuration changes require fresh activation. A pause persists `HALT` and removes activation. Pausing blocks new submissions; orders already accepted by the broker continue.

Selection changes first persist a pause and revoke activation, then save the preset. Restart and verify the loaded preset before activating again. Production retains the $80,000 collateral limit and three managed symbols; offline research limits do not change live configuration.

New configurations explicitly select `conservative-roi-v1`; `balanced-roi-v1` is the optional higher put-delta profile. Existing explicit selection policies remain supported. Every native policy now limits new put collateral plus the configured entry fee to `max_collateral_cents / max_symbols`, floored to cents. With the default three slots and $80,000 ceiling this is $26,666.66 per symbol, including fees. The fixed limit does not grow with profits. Selection filters oversized strikes before ranking, and submission planning repeats the check. Existing owned stock can still receive covered calls above this entry limit; it is never sold or appropriated to satisfy the cap. Executable-bound activation must be renewed after installing this change.

The offline 2026 runner applies the same limit with `--shared-portfolio --available-presets --limit-symbol-collateral`. It reports the derived amount; omitting the flag is an explicitly labeled historical counterfactual. `--include-adaptive` adds a research-only comparator. Complete daily valuations and qualified execution/assignment assumptions are still required before treating a simulation as production evidence.

## Existing strategy ownership

Independent strategies require distinct account bindings. AutoWheel verifies the TradePulse account binding before live operation. A same-account exclusive handover additionally requires flat peer lifecycles, a native stopped-state seal matched to the account/database, an inactive persistently masked peer service, and a lifetime operational lock. A paused peer is not retired. Do not adopt or liquidate peer shares to satisfy these checks.

Shared trading requires the qualified single execution coordinator, imported exact ownership, one cash ledger, restricted strategy capabilities, release/policy binding, and retired direct financial writes. The bundled observer service fails closed before that qualification.

## Recovery and deployment

An intent and its unique `ref_id` are saved before submission. A timeout or malformed receipt leaves the intent unresolved and is never retried automatically. Investigate the original order with broker facts; `recover` binds an operator-confirmed broker ID without submitting another order. `adopt` requires exact broker-confirmed settled inventory and explicit provenance; it does not authorize external or peer holdings.

Under `exclusive_standard`, completed order ownership remains in `completed_orders` after expiration or call-away. Preserve it across restarts and recovery. The journal refuses more than 1,024 retained receipts; archival requires a separately reviewed ownership-preserving migration. Do not delete receipts or move the native-retirement cutoff to bypass outside-activity checks. Older binaries do not understand this journal extension; any rollback stays paused and requires explicit schema compatibility review.

The operator-only `tools/tradepulse-retirement` helper requires a sibling TradePulse checkout for its native state models. It observes broker facts by default; `--apply-journal` requests native recovery under an exclusive daemon lock with fresh account-bound audit evidence. It never submits or cancels orders and cannot authorize AutoWheel trading.

Deployment/service scripts are installation-specific examples. Review their SSH target, user, filesystem paths, privilege requirements, and service settings before running them. Deployments revoke activation. Protect credentials during transfer and retain peer wind-down protections. The macOS menu app uses a local SSH tunnel and fixed service commands; it requires matching host settings.

## Take profit on an owned put

The menubar's **Close for Profit · SYMBOL** action supports one exactly owned short put at a time in the exclusive standard-order account. This is a single submission action with no additional confirmation dialog. The app obtains a fresh authenticated preview of the actual contract, opening fill, buyback ask, fees and collateral release, then submits one day limit order capped at that ask. Quotes and account facts are refreshed before submission; higher asks, ambiguous ownership, assignment activity, working orders or a loss after the configured opening/closing fee allowances block it. Closing existing collateral above a fixed entry cap is permitted because it reduces exposure.

During an authorized live session, profit taking reuses the release/config-bound authorization. Live remains enabled and the next cycle can enter after broker reconciliation; no per-cycle activation is needed. Normal service restarts retain that same authorization. An explicit pause, lost account isolation, or a release/config change still fences execution. A pause during a live close preview cannot be overwritten by the close action.

When taking profit from a paused deployment, native authorization is restricted to the single-use close preview and is revoked after the attempt; entries remain paused. A restart never resumes a close-only request. In either mode, the intent and ref_id are persisted before transport; timeouts and uncertain receipts are never retried. Refresh status and investigate the retained intent before taking further action. The existing `recover` command can bind an exact broker-confirmed close receipt without submitting another order.

A slot clears only after the broker confirms a fully filled owned close and the corresponding option is absent with no stock or pending assignment. Until then it remains reserved. Completed close receipts remain in the ownership ledger. Normal expiration still uses the existing settlement wait. A new cycle requires fresh broker cash and readiness; buyback does not promise immediate available cash.

## Available-cash allocation

For a specific exit-before-entry instruction, `priority_profit_close` binds the symbol, exact option ID, original `opened_at`, and `max_limit_cents` per share. It does not activate trading. Under an authorized live session, the daemon submits that profitable close before any new entry, then waits for the confirmed fill and disappearance of the short position. Reentry uses a fresh account snapshot and the normal selection rules. Paused deployments show the close as a read-only candidate.

A changed lifecycle, stale or missing data, a quote above the cap, or an unresolved submission blocks the priority and all entries. An unfilled terminal attempt is not resubmitted automatically. Filled completion is retained in the ownership journal, so ordinary restarts and later cycles do not require another close action or repeat the target against a new position. The added `filled` receipt field requires this release or a compatible newer reader after the first new terminal receipt; rollback must preserve the journal and use a compatible binary.

`max_collateral_cents: 0` removes the fixed account ceiling and its per-symbol division. Each put still needs its entire strike collateral plus the configured fee, funded from fresh broker available cash less `cash_reserve_cents`. Already-held collateral is not subtracted from available cash again. No margin, pending release or expected premium is treated as available cash. Positive ceilings retain their fixed account and equal-slot limits. Shared coordinator deployments retain their separately qualified fixed policy.

The daemon filters unaffordable contracts before ranking, then refreshes cash after scans and again after broker review. It shows read-only candidates while paused. The three-symbol and one-contract rules, conservative ranking and liquidity filters remain in force; all available cash is a funding limit, not a requirement to spend every dollar or loosen selection rules.

The journal's new closing fields are incompatible with old binaries once a close intent or receipt has been saved. Preserve the complete journal and stay paused during any rollback; never delete close receipts to make an older release accept it.
