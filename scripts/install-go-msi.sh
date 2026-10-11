#!/bin/sh
set -eu
ssh -o BatchMode=yes yuduo@192.168.1.50 'bash -s' <<'REMOTE'
set -euo pipefail
task_dir="$HOME/.local/share/autowheel/toolchain"
mkdir -p "$task_dir"
chmod 700 "$HOME/.local/share/autowheel"
if [ -x "$task_dir/go/bin/go" ]; then
  task_version=$(GOTOOLCHAIN=local "$task_dir/go/bin/go" env GOVERSION)
  case "$task_version" in
    go1.26.*) task_minimum=go1.26.9 ;;
    *) task_minimum=go1.27.2 ;;
  esac
  if [ "$(printf '%s\n' "$task_minimum" "$task_version" | sort -V | head -n 1)" != "$task_minimum" ]; then
    printf '%s\n' 'Existing Go toolchain is below the reviewed security patch level; upgrade it before deployment.' >&2
    exit 1
  fi
  GOTOOLCHAIN=local "$task_dir/go/bin/go" version
  exit 0
fi
cd "$task_dir"
curl --fail --silent --show-error --location --max-time 90 https://go.dev/dl/go1.26.9.linux-amd64.tar.gz -o go1.26.9.tar.gz
printf '%s  %s\n' 42d158b4d8f7b61ac0a830567c940a86098fb7aac52e467a5ebec03ef5cc2f8d go1.26.9.tar.gz | sha256sum -c -
tar -xzf go1.26.9.tar.gz
rm go1.26.9.tar.gz
./go/bin/go version
REMOTE
