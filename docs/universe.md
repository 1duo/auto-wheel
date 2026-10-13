# Selected universe

The default universe contains ten symbols: **NVDA, IREN, AMD, AMZN, GOOGL, AVGO, NBIS, COIN, MSTR, HOOD**. The managed-position limit remains three, with one contract per symbol and an $80,000 production collateral limit.

The October 12, 2026 selection removes **MSFT, RKLB, NOW, PLTR, META** from the fifteen-symbol research universe. Each symbol was replayed independently using the same finished January 2–October 9 data, fixed `conservative-roi-v1` strategy, Robinhood fee schedule, starting cash and research ceiling. Ranking uses net profit divided by peak committed capital; a symbol with no trades in the shared portfolio was not treated as a zero-return symbol. The ten highest measured ratios were retained.

This is retrospective selection on the evaluation period. It is not an independent validation of future returns. Missing interior option marks leave affected standalone drawdowns unavailable; endpoint valuations used for ranking were verified. Standalone returns cannot be added to obtain a portfolio return. Private input hashes, cash-ledger proofs and the combined replay remain outside Git.

Changing an installed daemon requires updating its private configuration: deployment does not replace an existing config with the example. Preserve existing strategy settings, account binding, risk limits, journal and retirement evidence. Retain every symbol with an owned position or unresolved intent until its lifecycle is finished. A release or configuration change revokes activation and requires fresh native readiness checks.
