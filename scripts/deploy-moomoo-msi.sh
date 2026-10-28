#!/bin/sh
set -eu

# Unique upload trees and a remote rollout lock keep source hashes reproducible.
task_host=yuduo@192.168.1.50
task_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
task_upload=$(uuidgen)
case "$task_upload" in ''|*[!A-Za-z0-9-]*) exit 1 ;; esac
task_remote=.local/share/autowheel-moomoo/staging/$task_upload
ssh -T -o BatchMode=yes -o StrictHostKeyChecking=yes "$task_host" "umask 077; test ! -L \"\$HOME/.local/share/autowheel-moomoo\" && mkdir -p \"\$HOME/$task_remote/cmd\" \"\$HOME/$task_remote/internal\" \"\$HOME/$task_remote/deploy\" \"\$HOME/$task_remote/scripts\""
rsync -az "$task_root/go.mod" "$task_root/go.sum" "$task_host:$task_remote/"
rsync -az --exclude '*_test.go' "$task_root/cmd/autowheel-moomoo/" "$task_host:$task_remote/cmd/autowheel-moomoo/"
for task_package in moomoo wheel runtime contractpolicy; do
  rsync -az --exclude '*_test.go' "$task_root/internal/$task_package/" "$task_host:$task_remote/internal/$task_package/"
done
rsync -az "$task_root/deploy/autowheel-moomoo.service" "$task_host:$task_remote/deploy/"
rsync -az "$task_root/scripts/deploy-moomoo-msi.sh" "$task_host:$task_remote/scripts/"
# MOOMOO_ROSTER="SYMBOLS|SOURCE_SHA256|KNOWN_AT|FROM|THROUGH" binds the reviewed
# execution policy during this rollout. It never requests consent or activates.
task_roster=${MOOMOO_ROSTER:-}
case "$task_roster" in *[!A-Z0-9a-f,|:.+-]*) exit 1 ;; esac
ssh -T -o BatchMode=yes -o StrictHostKeyChecking=yes "$task_host" "bash -s -- '$task_upload' '$task_roster'" <<'REMOTE'
set -euo pipefail
umask 077
case "$1" in ''|*[!A-Za-z0-9-]*) exit 1 ;; esac
task_roster="${2:-}"
task_runtime="$HOME/.local/share/autowheel-moomoo"
task_source="$task_runtime/staging/$1"
task_config="$HOME/.config/autowheel-moomoo/config.json"
task_state="$HOME/.local/state/autowheel-moomoo"
task_go="$HOME/.local/share/autowheel/toolchain/go/bin/go"
task_rollout_started=0
task_previous=""
task_was_active=0
task_had_unit=0
task_config_changed=0
rollback() {
  task_result=$?
  set +e
  if [ "$task_result" -ne 0 ] && [ "$task_rollout_started" -eq 1 ]; then
    sudo -n systemctl stop autowheel-moomoo.service
    if [ "$task_config_changed" -eq 1 ]; then
      install -m 600 "$task_source/previous-config.json" "$task_config.rollback"
      mv -Tf "$task_config.rollback" "$task_config"
      sync -f "$task_config"
    fi
    if [ -n "$task_previous" ]; then
      rm -f "$task_runtime/current.rollback"
      ln -s "$task_previous" "$task_runtime/current.rollback"
      mv -Tf "$task_runtime/current.rollback" "$task_runtime/current"
    else
      rm -f "$task_runtime/current"
    fi
    if [ "$task_had_unit" -eq 1 ]; then
      sudo -n install -m 644 "$task_source/previous.service" /etc/systemd/system/autowheel-moomoo.service
    else
      sudo -n systemctl disable autowheel-moomoo.service
      sudo -n rm -f /etc/systemd/system/autowheel-moomoo.service
    fi
    sudo -n systemctl daemon-reload
    if [ "$task_was_active" -eq 1 ]; then
      sudo -n systemctl start autowheel-moomoo.service || printf 'Moomoo rollback needs operator attention\n' >&2
    fi
    printf 'Moomoo rollout failed; prior release restored with trading paused\n' >&2
  fi
  rm -rf -- "$task_source"
  exit "$task_result"
}
trap rollback EXIT
for task_directory in "$task_runtime" "$(dirname "$task_config")" "$task_state"; do
  test ! -L "$task_directory"
  mkdir -p "$task_directory"
  chmod 700 "$task_directory"
done
test ! -L "$task_runtime/deploy.lock"
exec 9>"$task_runtime/deploy.lock"
flock -n 9
test -x "$task_go"
cd "$task_source"
export GOCACHE="$task_runtime/build-cache" GOMAXPROCS=2
{ find cmd internal -name '*.go' -print0; printf 'go.mod\0go.sum\0deploy/autowheel-moomoo.service\0scripts/deploy-moomoo-msi.sh\0'; } | sort -z | xargs -0 sha256sum > SOURCE.SHA256SUMS
task_release=$(sha256sum SOURCE.SHA256SUMS | cut -c1-16)
task_dir="$task_runtime/releases/$task_release"
if [ -d "$task_dir" ]; then
  cmp SOURCE.SHA256SUMS "$task_dir/SOURCE.SHA256SUMS"
  (cd "$task_dir" && sha256sum --check --status SHA256SUMS)
  cmp deploy/autowheel-moomoo.service "$task_dir/autowheel-moomoo.service"
else
  mkdir -p "$task_runtime/releases" "$task_source/release"
  "$task_go" build -trimpath -ldflags "-s -w -X main.version=$task_release" -o "$task_source/release/autowheel-moomoo" ./cmd/autowheel-moomoo
  cp SOURCE.SHA256SUMS "$task_source/release/SOURCE.SHA256SUMS"
  cp deploy/autowheel-moomoo.service "$task_source/release/autowheel-moomoo.service"
  (cd "$task_source/release" && sha256sum autowheel-moomoo > SHA256SUMS)
  chmod 500 "$task_source/release/autowheel-moomoo"
  mv "$task_source/release" "$task_dir"
fi
test ! -L "$task_config"
if [ ! -e "$task_config" ]; then
  "$task_dir/autowheel-moomoo" init --config "$task_config"
fi
"$task_dir/autowheel-moomoo" check-config --config "$task_config" >/dev/null
if [ -n "$task_roster" ] && python3 -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1]))["strategy"].get("fixed_policy") is not None else 1)' "$task_config"; then
  printf 'MOOMOO_ROSTER cannot replace an already bound execution policy; no change made\n' >&2
  exit 1
fi
if [ -L "$task_runtime/current" ]; then
  task_previous=$(readlink "$task_runtime/current")
elif [ -e "$task_runtime/current" ]; then
  printf 'Moomoo current release must be a symlink\n' >&2
  exit 1
fi
if [ -f /etc/systemd/system/autowheel-moomoo.service ]; then
  cp /etc/systemd/system/autowheel-moomoo.service "$task_source/previous.service"
  task_had_unit=1
fi
if sudo -n systemctl is-active --quiet autowheel-moomoo.service; then task_was_active=1; fi
task_rollout_started=1
if [ "$task_had_unit" -eq 1 ]; then sudo -n systemctl stop autowheel-moomoo.service; fi
# Prove/initialize the broker binding under the daemon's lock before fencing.
"$task_dir/autowheel-moomoo" init-state --config "$task_config" --state "$task_state"
install -m 600 /dev/null "$task_state/HALT"
sync -f "$task_state/HALT"
rm -f "$task_state/LIVE.json"
sync -f "$task_state"
install -m 600 "$task_config" "$task_source/previous-config.json"
test ! -L "$task_runtime/config-backups"
mkdir -p "$task_runtime/config-backups"
chmod 700 "$task_runtime/config-backups"
install -m 600 "$task_config" "$task_runtime/config-backups/$1.json"
sync -f "$task_runtime/config-backups/$1.json"
sync -f "$task_runtime/config-backups"
task_config_changed=1
task_bound=$(python3 -c 'import json,sys; print(int(json.load(open(sys.argv[1]))["strategy"].get("fixed_policy") is not None))' "$task_config")
if [ "$task_bound" -eq 0 ]; then
  "$task_dir/autowheel-moomoo" configure-five-slots --config "$task_config" --state "$task_state"
  if [ -n "$task_roster" ]; then
    IFS='|' read -r task_symbols task_source_sha task_known task_from task_through <<<"$task_roster"
    "$task_dir/autowheel-moomoo" configure-execution --config "$task_config" --state "$task_state" \
      --roster-symbols "$task_symbols" --roster-source-sha256 "$task_source_sha" --roster-known-at "$task_known" \
      --roster-from "$task_from" --roster-through "$task_through"
    task_bound=1
  fi
fi
if [ "$task_bound" -eq 1 ]; then
  # Read-only production-path audit; it never prepares or sends an order.
  "$task_dir/autowheel-moomoo" audit-execution --config "$task_config" --state "$task_state" || printf 'Moomoo execution audit did not pass; readiness will stay blocked\n' >&2
fi
sudo -n install -m 644 "$task_dir/autowheel-moomoo.service" /etc/systemd/system/autowheel-moomoo.service
if [ -n "$task_previous" ]; then printf '%s\n' "$task_previous" > "$task_runtime/previous-release"; fi
rm -f "$task_runtime/current.next"
ln -s "releases/$task_release" "$task_runtime/current.next"
mv -Tf "$task_runtime/current.next" "$task_runtime/current"
sync -f "$task_runtime"
sudo -n systemctl daemon-reload
sudo -n systemctl enable --now autowheel-moomoo.service
python3 - "$task_release" "$task_bound" <<'PY'
import json, sys, time, urllib.request
expected, bound = sys.argv[1], sys.argv[2] == '1'
for attempt in range(12):
    try:
        with urllib.request.urlopen('http://127.0.0.1:9842/status', timeout=2) as response:
            data = json.load(response)
        assert data['broker_id'] == 'moomoo' and data['deployment_id'] == 'autowheel-moomoo'
        assert data['version'] == expected and data['mode'] == 'read_only'
        assert data['halted'] is True and data['live_capable'] is bound
        assert (data['config'].get('order_coverage_policy') == 'moomoo_complete') is bound
        assert data['config']['max_symbols'] == 5
        assert data['execution_policy_id'] == 'conservative-rd1-floor-tp75-asap-v3'
        break
    except Exception:
        if attempt == 11:
            raise SystemExit('Moomoo release health verification failed')
        time.sleep(0.5)
PY
sudo -n systemctl is-active --quiet autowheel-moomoo.service
task_rollout_started=0
printf 'Moomoo release %s verified active and paused (execution policy bound: %s)\n' "$task_release" "$task_bound"
REMOTE
