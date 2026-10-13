# AutoWheel

Deterministic cash-secured puts → assigned shares → covered calls through Robinhood's official Agentic Trading MCP. No AI in the trading loop.

One cash account, at most three managed symbols, one contract per symbol. Defaults: conservative put delta 0.05–0.15 and an $80,000 collateral ceiling. Requires Go 1.26.9+; `go.mod` selects Go 1.27.2.

Selected ten: NVDA, IREN, AMD, AMZN, GOOGL, AVGO, NBIS, COIN, MSTR, HOOD. See [selection method and limits](docs/universe.md).

```sh
go build -trimpath -o bin/autowheel ./cmd/autowheel
umask 077
bin/autowheel init --config "$HOME/.config/autowheel/config.json"
```

Follow [setup and operations](docs/operations.md) to connect Robinhood and run. Deployment defaults to read-only. Live trading requires authenticated readiness, verified account isolation or peer retirement, and native activation bound to the executable and config. Ambiguous broker facts block orders; submissions are never retried automatically.

Local status: `http://127.0.0.1:9841/status`. Weekly P&L calendar: `/calendar`, also available from the optional macOS menubar app.

[Readiness](docs/readiness.md) · [Strategy guide](docs/strategy.html) · [Simulation](docs/simulation-design.md) · [Research](docs/multiyear-research.md)

Credentials, account state, personal reports and licensed datasets stay outside Git. Simulated returns are not broker performance.
