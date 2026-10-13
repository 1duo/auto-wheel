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

The loopback operator API checks its Host header. Native control requests require an empty POST body, no Origin header, and `X-AutoWheel-Control: confirm`. Access to the local machine or SSH forwarding is a trusted operator boundary. Keep the listener on loopback.

## Trading authorization

The normal `run` command has no order transport. `--live` is rejected for daemon operation. A reviewed deployment may use `--live-capable`, which permits readiness checks and the native activation control but does not itself authorize orders. Activation requires fresh broker facts qualified for the explicitly configured order-coverage policy, no unresolved intent, compatible quotes/review, a known exchange calendar, and verified account isolation. Restricted standard-order readiness discloses unknown advanced/OCO visibility in the native menu; see [readiness](readiness.md).

The private `LIVE.json` record binds the release label, executable SHA-256, configuration digest, and any coordinator permission epoch. Old records without an executable digest cannot authorize trading. Rebuilds and configuration changes require fresh activation. A pause persists `HALT` and removes activation. Pausing blocks new submissions; orders already accepted by the broker continue.

Selection changes first persist a pause and revoke activation, then save the preset. Restart and verify the loaded preset before activating again. Production retains the $80,000 collateral limit and three managed symbols; offline research limits do not change live configuration.

## Existing strategy ownership

Independent strategies require distinct account bindings. AutoWheel verifies the TradePulse account binding before live operation. A same-account exclusive handover additionally requires flat peer lifecycles, a native stopped-state seal matched to the account/database, an inactive persistently masked peer service, and a lifetime operational lock. A paused peer is not retired. Do not adopt or liquidate peer shares to satisfy these checks.

Shared trading requires the qualified single execution coordinator, imported exact ownership, one cash ledger, restricted strategy capabilities, release/policy binding, and retired direct financial writes. The bundled observer service fails closed before that qualification.

## Recovery and deployment

An intent and its unique `ref_id` are saved before submission. A timeout or malformed receipt leaves the intent unresolved and is never retried automatically. Investigate the original order with broker facts; `recover` binds an operator-confirmed broker ID without submitting another order. `adopt` requires exact broker-confirmed settled inventory and explicit provenance; it does not authorize external or peer holdings.

Under `exclusive_standard`, completed order ownership remains in `completed_orders` after expiration or call-away. Preserve it across restarts and recovery. The journal refuses more than 1,024 retained receipts; archival requires a separately reviewed ownership-preserving migration. Do not delete receipts or move the native-retirement cutoff to bypass outside-activity checks. Older binaries do not understand this journal extension; any rollback stays paused and requires explicit schema compatibility review.

The operator-only `tools/tradepulse-retirement` helper requires a sibling TradePulse checkout for its native state models. It observes broker facts by default; `--apply-journal` requests native recovery under an exclusive daemon lock with fresh account-bound audit evidence. It never submits or cancels orders and cannot authorize AutoWheel trading.

Deployment/service scripts are installation-specific examples. Review their SSH target, user, filesystem paths, privilege requirements, and service settings before running them. Deployments revoke activation. Protect credentials during transfer and retain peer wind-down protections. The macOS menu app uses a local SSH tunnel and fixed service commands; it requires matching host settings.
