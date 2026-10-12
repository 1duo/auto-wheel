# AutoWheel

A deterministic, cash-secured options wheel for Robinhood's official Agentic Trading MCP. No AI model participates in trading decisions. Linux or macOS; Go 1.26.9 minimum, with the patched Go 1.27.2 toolchain selected automatically by default.

One account, one contract per symbol, at most three managed symbols. New configurations use the conservative preset (put delta 0.05–0.15) and an $80,000 collateral ceiling. The $150,000 research ceiling is an offline simulation setting.

## Build and configure

```sh
git clone https://github.com/1duo/auto-wheel.git
cd auto-wheel
go build -trimpath -o bin/autowheel ./cmd/autowheel

umask 077
mkdir -p "$HOME/.config/autowheel" "$HOME/.local/state/autowheel"
bin/autowheel init --config "$HOME/.config/autowheel/config.json"
```

Keep configuration and state outside the checkout. Edit the generated config to choose your symbols and cash reserve; leave `account_number` empty until discovery. See [the example](deploy/config.example.json).

## Link Robinhood

Complete Robinhood's [Agentic Trading onboarding](https://robinhood.com/us/en/support/articles/agentic-trading-overview/) and obtain Level 2+ options approval for a cash account. The official MCP endpoint is `https://agent.robinhood.com/mcp/trading`; external agents trade through the designated Agentic account. See [Robinhood's connection guide](https://robinhood.com/us/en/support/articles/trading-with-your-agent/).

```sh
bin/autowheel login --state "$HOME/.local/state/autowheel"
bin/autowheel doctor --config "$HOME/.config/autowheel/config.json" \
  --state "$HOME/.local/state/autowheel"
```

Open the authorization URL printed by `login` and authenticate with Robinhood in your browser. OAuth uses PKCE and a loopback callback on port 8765 (`--callback-port` overrides it). For a remote daemon, forward that port to your local browser machine before login.

`doctor` saves account discovery privately to `$HOME/.local/state/autowheel/mcp-accounts.json`. Copy the exact Agentic account number into the private config's `account_number`, then run `doctor` again. Tokens are stored in `robinhood.token.json` with private permissions; never commit tokens, passwords, account exports, or state.

## Run in read-only mode

```sh
bin/autowheel check-config --config "$HOME/.config/autowheel/config.json"
bin/autowheel run --config "$HOME/.config/autowheel/config.json" \
  --state "$HOME/.local/state/autowheel"
```

Status is at `http://127.0.0.1:9841/status`. These commands do not authorize trading. Missing broker schemas, advanced/OCO order coverage, or ambiguous account facts block execution; authentication alone is insufficient.

Live operation additionally requires authenticated readiness, verified account ownership/isolation, and explicit native activation bound to the executable and configuration. Existing TradePulse lifecycles must retain their protections until a qualified retirement or separate-account arrangement is proven. See [operations](docs/operations.md) and [readiness](docs/readiness.md). Deployment scripts and the optional macOS menu app contain installation-specific host/service settings; adapt and review them before use.

## Source map

- `cmd/autowheel`, `internal/wheel`, `internal/robinhood`, `internal/runtime`: daemon, strategy, OAuth/MCP, durable journal.
- `internal/coordinator`, `internal/accountservice`, `internal/shared*`: guarded account-coordination work; observation does not authorize shared trading.
- Remaining commands, simulation/market-data packages, and `scripts/`: offline research and acquisition tools. Vendor credentials and licensed datasets are supplied separately.

[Simulation design](docs/simulation-design.md) explains causal data and execution assumptions. Retrospective universe selection and modeled fills are disclosed; simulated returns are not broker performance. This repository excludes personal account reports, market-data captures, and local Git history.

## 2024–2025 research

See [strategy presets and offline simulation](docs/multiyear-research.md). The
new presets are research candidates; switching always pauses and requires fresh
readiness after restart. Licensed data and account balances stay outside Git.
