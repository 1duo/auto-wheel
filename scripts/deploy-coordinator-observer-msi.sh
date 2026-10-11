#!/bin/sh
set -eu
task_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
task_host=yuduo@192.168.1.50
rsync -az --filter=':- .gitignore' --exclude .git --exclude state --exclude config.json --exclude bin --exclude var --exclude .build "$task_root/" "$task_host:Projects/AutoWheel/"
ssh -o BatchMode=yes -o StrictHostKeyChecking=yes "$task_host" 'bash -s' <<'REMOTE'
set -euo pipefail
task_source="$HOME/Projects/AutoWheel"
task_runtime="$HOME/.local/share/account-coordinator"
task_state="$HOME/.local/state/account-coordinator"
task_config="$HOME/.config/account-coordinator"
task_go="$HOME/.local/share/autowheel/toolchain/go/bin/go"
test ! -e "$task_state/SHARED_REQUIRED"
cd "$task_source"
export GOCACHE="$HOME/.local/share/autowheel/build-cache" GOMAXPROCS=2
task_release=$({ find cmd internal -name '*.go' -print0; printf 'go.mod\0go.sum\0deploy/account-coordinator-observer.service\0scripts/deploy-coordinator-observer-msi.sh\0'; } | sort -z | xargs -0 sha256sum | sha256sum | cut -c1-16)
task_dir="$task_runtime/releases/$task_release"
install -d -m 700 "$task_runtime" "$task_dir" "$task_state" "$task_config" "$HOME/.config/systemd/user"
"$task_go" build -trimpath -ldflags "-s -w -X main.version=$task_release" -o "$task_dir/account-coordinator" ./cmd/account-coordinator
chmod 500 "$task_dir/account-coordinator"
sha256sum "$task_dir/account-coordinator" > "$task_dir/SHA256SUMS"
python3 - "$task_source" "$task_config" <<'PY'
import hashlib,json,os,pathlib,tempfile,sqlite3
source,config=map(pathlib.Path,__import__('sys').argv[1:])
home=pathlib.Path.home()
aw=json.loads((home/'.config/autowheel/config.json').read_text())
ledger=home/'.local/state/account-coordinator/ledger.sqlite3'
if ledger.exists():
    with sqlite3.connect('file:'+str(ledger)+'?mode=ro',uri=True) as db:
        if db.execute('SELECT count(*) FROM account').fetchone()[0]:raise SystemExit('An imported account requires the guarded shared deployment path, not observer installation')
policy={'schema':'SharedObservationPolicyV1','mode':'observation_only','max_capital_cents':8000000,'financial_transport_enabled':False,'unqualified':['execution sizing and fee policy','frozen TradePulse ownership import','option and settled cash reconciliation','single writer migration and compatible rollback']}
def atomic(path,value):
    fd,tmp=tempfile.mkstemp(dir=path.parent,prefix='.coordinator-')
    try:
        os.fchmod(fd,0o600)
        with os.fdopen(fd,'wb') as f:f.write(json.dumps(value,sort_keys=True,indent=2).encode()+b'\n');f.flush();os.fsync(f.fileno())
        os.replace(tmp,path)
        directory=os.open(path.parent,os.O_DIRECTORY);os.fsync(directory);os.close(directory)
    finally:
        if os.path.exists(tmp):os.unlink(tmp)
policy_path=config/'policy.json'
atomic(policy_path,policy)
aw_binary=home/'.local/share/autowheel/current/autowheel'
aw_sha=hashlib.sha256(aw_binary.read_bytes()).hexdigest()
# This is the existing deployed TradePulse executable identity, not a grant
# to use the new shared route. The guarded migration must bind its new release.
tp_state=home/'.local/share/TradePulse/LivePilotV0'
tp_release=(tp_state/'supervisor/active-release').read_text().strip()
if len(tp_release)!=64 or any(c not in '0123456789abcdef' for c in tp_release):raise SystemExit('TradePulse release identity invalid')
tp_dir=tp_state/'releases'/tp_release
tp_manifest=json.loads((tp_dir/'release.json').read_text())
if tp_manifest.get('binary_file')!='tradepulse' or tp_manifest.get('release_hash')!=tp_release:raise SystemExit('TradePulse release manifest mismatch')
tp_sha=hashlib.sha256((tp_dir/'tradepulse').read_bytes()).hexdigest()
if tp_sha!=tp_manifest.get('binary_hash'):raise SystemExit('TradePulse executable identity mismatch')
atomic(config/'config.json',{'account_number':aw['account_number'],'token_file':str(home/'.local/state/autowheel/robinhood.token.json'),'max_capital_cents':8000000,'policy_hash':hashlib.sha256(policy_path.read_bytes()).hexdigest(),'client_releases':{'autowheel':aw_sha,'tradepulse':tp_sha}})
PY
if systemctl --user cat account-coordinator.service >/dev/null 2>&1; then
  systemctl --user disable --now account-coordinator.service
  rm -f "$HOME/.config/systemd/user/account-coordinator.service"
  systemctl --user daemon-reload
fi
sudo -n install -m 644 deploy/account-coordinator-observer.service /etc/systemd/system/account-coordinator.service
if sudo -n systemctl is-active --quiet account-coordinator.service; then sudo -n systemctl stop account-coordinator.service; fi
if [ -L "$task_runtime/current" ]; then readlink "$task_runtime/current" > "$task_runtime/previous-release"; fi
ln -s "releases/$task_release" "$task_runtime/current.next"
mv -Tf "$task_runtime/current.next" "$task_runtime/current"
sudo -n systemctl daemon-reload
sudo -n systemctl enable --now account-coordinator.service
sudo -n systemctl is-active account-coordinator.service
"$task_dir/account-coordinator" version
REMOTE
