#!/bin/sh
set -eu

# Credentials are preserved; every deployment revokes trading activation.
task_host=yuduo@192.168.1.50
task_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
rsync -az --filter=':- .gitignore' --exclude .git --exclude state --exclude config.json --exclude bin --exclude var --exclude .build "$task_root/" "$task_host:Projects/AutoWheel/"
ssh -o BatchMode=yes "$task_host" 'bash -s' <<'REMOTE'
set -euo pipefail
task_source="$HOME/Projects/AutoWheel"
task_runtime="$HOME/.local/share/autowheel"
task_go="$task_runtime/toolchain/go/bin/go"
test -x "$task_go"
cd "$task_source"
export GOCACHE="$task_runtime/build-cache" GOMAXPROCS=2
task_release=$({ find cmd internal -name '*.go' -print0; printf 'go.mod\0go.sum\0deploy/autowheel.service\0scripts/deploy-msi.sh\0'; } | sort -z | xargs -0 sha256sum | sha256sum | cut -c1-16)
task_dir="$task_runtime/releases/$task_release"
mkdir -p "$task_dir" "$HOME/.config/autowheel" "$HOME/.local/state/autowheel" "$HOME/.config/systemd/user"
chmod 700 "$task_runtime" "$HOME/.config/autowheel" "$HOME/.local/state/autowheel"
"$task_go" build -trimpath -ldflags "-s -w -X main.version=$task_release" -o "$task_dir/autowheel" ./cmd/autowheel
sha256sum "$task_dir/autowheel" > "$task_dir/SHA256SUMS"
chmod 500 "$task_dir/autowheel"
if [ ! -e "$HOME/.config/autowheel/config.json" ]; then
  install -m 600 deploy/config.example.json "$HOME/.config/autowheel/config.json"
fi
if systemctl --user cat autowheel.service >/dev/null 2>&1; then
  systemctl --user disable --now autowheel.service
  rm -f "$HOME/.config/systemd/user/autowheel.service"
  systemctl --user daemon-reload
fi
if sudo -n systemctl is-active --quiet autowheel.service; then sudo -n systemctl stop autowheel.service; fi
sudo -n install -m 644 deploy/autowheel.service /etc/systemd/system/autowheel.service
install -m 600 /dev/null "$HOME/.local/state/autowheel/HALT"
sync -f "$HOME/.local/state/autowheel/HALT"
rm -f "$HOME/.local/state/autowheel/LIVE.json"
sync -f "$HOME/.local/state/autowheel"
if [ -L "$task_runtime/current" ]; then
  readlink "$task_runtime/current" > "$task_runtime/previous-release"
fi
ln -s "releases/$task_release" "$task_runtime/current.next"
mv -Tf "$task_runtime/current.next" "$task_runtime/current"
sudo -n systemctl daemon-reload
sudo -n systemctl enable --now autowheel.service
sudo -n systemctl restart autowheel.service
sudo -n systemctl is-active autowheel.service
"$task_dir/autowheel" version
REMOTE
