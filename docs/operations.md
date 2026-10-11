# Operations

AutoWheel starts read-only. Keep config, OAuth tokens, journals, account discovery, and activation records in private directories outside the repository. Config/token/state files require private permissions; symbolic links are rejected for sensitive reads. One journal lock protects each daemon state directory.

Build and link Robinhood using the README. `check-config` validates local configuration without broker access or trading effects. `doctor` authenticates and checks broker compatibility; `audit-account` is read-only and writes private evidence. The account must explicitly support Agentic trading, cash-only buying power, and Level 2+ options.

The loopback operator API checks its Host header. Native control requests require an empty POST body, no Origin header, and `X-AutoWheel-Control: confirm`. Access to the local machine or SSH forwarding is a trusted operator boundary. Keep the listener on loopback.

## Trading authorization

The normal `run` command has no order transport. `--live` is rejected for daemon operation. A reviewed deployment may use `--live-capable`, which permits readiness checks and the native activation control but does not itself authorize orders. Activation requires a complete fresh broker snapshot, no unresolved intent, compatible quotes/review, a known exchange calendar, and verified account isolation.

The private `LIVE.json` record binds the release label, executable SHA-256, configuration digest, and any coordinator permission epoch. Old records without an executable digest cannot authorize trading. Rebuilds and configuration changes require fresh activation. A pause persists `HALT` and removes activation. Pausing blocks new submissions; orders already accepted by the broker continue.

Selection changes first persist a pause and revoke activation, then save the preset. Restart and verify the loaded preset before activating again. Production retains the $80,000 collateral limit and three managed symbols; offline research limits do not change live configuration.

## Existing strategy ownership

Independent strategies require distinct account bindings. AutoWheel verifies the TradePulse account binding before live operation. A same-account exclusive handover additionally requires flat peer lifecycles, a native stopped-state seal matched to the account/database, an inactive persistently masked peer service, and a lifetime operational lock. A paused peer is not retired. Do not adopt or liquidate peer shares to satisfy these checks.

Shared trading requires the qualified single execution coordinator, imported exact ownership, one cash ledger, restricted strategy capabilities, release/policy binding, and retired direct financial writes. The bundled observer service fails closed before that qualification.

## Recovery and deployment

An intent and its unique `ref_id` are saved before submission. A timeout or malformed receipt leaves the intent unresolved and is never retried automatically. Investigate the original order with broker facts; `recover` binds an operator-confirmed broker ID without submitting another order. `adopt` requires exact broker-confirmed settled inventory and explicit provenance; it does not authorize external or peer holdings.

Deployment/service scripts are installation-specific examples. Review their SSH target, user, filesystem paths, privilege requirements, and service settings before running them. Deployments revoke activation. Protect credentials during transfer and retain peer wind-down protections. The macOS menu app uses a local SSH tunnel and fixed service commands; it requires matching host settings.
