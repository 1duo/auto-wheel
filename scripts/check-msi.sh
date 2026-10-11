#!/bin/sh
set -eu
ssh -o BatchMode=yes yuduo@192.168.1.50 'bash -s' <<'REMOTE'
set -euo pipefail
cd "$HOME/Projects/AutoWheel"
export GOCACHE="$HOME/.local/share/autowheel/build-cache" GOMAXPROCS=2
task_go="$HOME/.local/share/autowheel/toolchain/go/bin/go"
"$task_go" test ./internal/wheel ./internal/runtime ./internal/robinhood -run 'Test(Money|Nearest|Slots|Durable|Assignment|Cancellation|Journal|MCP|Provider)' -count=1
"$task_go" test ./cmd/autowheel -run '^TestThreeScanWorkers$' -count=1
"$task_go" vet ./internal/wheel ./internal/runtime ./internal/robinhood ./cmd/autowheel
REMOTE
