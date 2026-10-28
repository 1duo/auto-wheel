#!/bin/sh
# Prepare a pinned vendor gateway without logging in, unlocking or starting it.
set -eu
task_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
task_host=yuduo@192.168.1.50
task_upload=$(uuidgen)
case "$task_upload" in ''|*[!A-Za-z0-9-]*) exit 1 ;; esac
task_remote=.local/share/autowheel-moomoo-opend/staging/$task_upload
ssh -T -o BatchMode=yes -o StrictHostKeyChecking=yes "$task_host" "set -eu; umask 077; test ! -L \"\$HOME/.local/share/autowheel-moomoo-opend\"; mkdir -p \"\$HOME/$task_remote\""
rsync -az "$task_root/deploy/moomoo-opend.xml" "$task_root/scripts/moomoo-opend-login.sh" \
  "$task_root/scripts/moomoo-opend-audit.py" "$task_host:$task_remote/"
ssh -T -o BatchMode=yes -o StrictHostKeyChecking=yes "$task_host" "bash -s -- '$task_upload'" <<'REMOTE'
set -euo pipefail
umask 077
case "$1" in ''|*[!A-Za-z0-9-]*) exit 1 ;; esac
task_root="$HOME/.local/share/autowheel-moomoo-opend"
task_source="$task_root/staging/$1"
task_archive="$task_root/downloads/moomoo_OpenD_10.11.7108_Ubuntu18.04.tar.gz"
task_install=/opt/autowheel-moomoo-opend/10.11.7108
test ! -L "$task_root"
mkdir -p "$task_root/downloads"
test ! -L "$task_root/prepare.lock"
exec 9>"$task_root/prepare.lock"
flock -n 9
trap 'rm -rf -- "$task_source"' EXIT
if sudo -n systemctl is-active --quiet autowheel-moomoo-opend-qualify.service; then
  printf 'OpenD qualification is already running; stop it before preparing files\n' >&2
  exit 1
fi
# This is an observed SHA256 from the official HTTPS artifact, not a vendor signature.
if [ ! -f "$task_archive" ]; then
  curl --fail --silent --show-error --proto '=https' --proto-redir '=https' --location --max-time 240 \
    --output "$task_archive.part" \
    https://softwaredownload.moomoo.com/moomoo_OpenD_10.11.7108_Ubuntu18.04.tar.gz
  mv "$task_archive.part" "$task_archive"
fi
test ! -L "$task_archive"
python3 - "$task_archive" "$task_source" <<'PY'
import hashlib, pathlib, sys, tarfile
archive, destination = map(pathlib.Path, sys.argv[1:])
expected = '457bb4bfed1ea32b6c5b19f9ebc4384d9ae8dd01e577427df06f4fcd98b79458'
with archive.open('rb') as stream:
    assert hashlib.file_digest(stream, 'sha256').hexdigest() == expected, 'OpenD archive hash mismatch'
with tarfile.open(archive) as package:
    members = package.getmembers()
    assert len(members) < 10000 and sum(m.size for m in members) < 2 * 1024**3
    # Keep the official headless distribution; the GUI AppImage is not needed.
    members = [m for m in members if not m.name.endswith('.AppImage')]
    package.extractall(destination / 'unpacked', members=members, filter='data')
PY
task_dist="$task_source/unpacked/moomoo_OpenD_10.11.7108_Ubuntu18.04/moomoo_OpenD_10.11.7108_Ubuntu18.04"
test -x "$task_dist/OpenD"
(
  cd "$task_dist"
  find . -type f -print0 | sort -z | xargs -0 sha256sum > "$task_source/PACKAGE.SHA256SUMS"
)
if ! id autowheel-moomoo-opend >/dev/null 2>&1; then
  sudo -n useradd --system --home-dir /var/lib/autowheel-moomoo-opend --create-home \
    --shell /usr/sbin/nologin autowheel-moomoo-opend
fi
test "$(getent passwd autowheel-moomoo-opend | cut -d: -f6)" = /var/lib/autowheel-moomoo-opend
sudo -n test ! -L /var/lib/autowheel-moomoo-opend
sudo -n test ! -L /var/lib/autowheel-moomoo-opend/OpenD.xml
sudo -n test ! -L /opt/autowheel-moomoo-opend
sudo -n install -d -m 700 -o autowheel-moomoo-opend -g autowheel-moomoo-opend \
  /var/lib/autowheel-moomoo-opend /var/lib/autowheel-moomoo-opend/logs
sudo -n install -d -m 755 -o root -g root /opt/autowheel-moomoo-opend
if [ ! -e "$task_install" ]; then
  task_next=$(sudo -n mktemp -d /opt/autowheel-moomoo-opend/prepare.XXXXXXXX)
  sudo -n cp -a "$task_dist/." "$task_next/"
  sudo -n cp "$task_source/PACKAGE.SHA256SUMS" "$task_next/PACKAGE.SHA256SUMS"
  sudo -n chown -R root:root "$task_next"
  sudo -n chmod -R go-w "$task_next"
  sudo -n chmod 755 "$task_next"
  sudo -n mv "$task_next" "$task_install"
fi
test ! -L "$task_install"
# Public vendor resources must be readable by the dedicated gateway identity.
# tar's data filter intentionally strips some original access bits.
sudo -n chmod -R a+rX,go-w "$task_install"
cmp "$task_source/PACKAGE.SHA256SUMS" "$task_install/PACKAGE.SHA256SUMS"
(cd "$task_install" && sha256sum --check --status PACKAGE.SHA256SUMS)
sudo -n install -m 600 -o autowheel-moomoo-opend -g autowheel-moomoo-opend \
  "$task_source/moomoo-opend.xml" /var/lib/autowheel-moomoo-opend/OpenD.xml
sudo -n install -m 755 -o root -g root "$task_source/moomoo-opend-login.sh" /usr/local/bin/autowheel-moomoo-opend-login
install -m 500 "$task_source/moomoo-opend-audit.py" "$task_root/moomoo-opend-audit.py"
mkdir -p "$HOME/.local/state/autowheel-moomoo-opend/evidence"
chmod 700 "$HOME/.local/state/autowheel-moomoo-opend" "$HOME/.local/state/autowheel-moomoo-opend/evidence"
python3 - "$task_root" <<'PY'
import hashlib, json, pathlib, sys
root = pathlib.Path(sys.argv[1])
record = {'version': '10.11.7108', 'source': 'https://softwaredownload.moomoo.com/moomoo_OpenD_10.11.7108_Ubuntu18.04.tar.gz',
          'archive_sha256': '457bb4bfed1ea32b6c5b19f9ebc4384d9ae8dd01e577427df06f4fcd98b79458',
          'collector_sha256': hashlib.sha256((root/'moomoo-opend-audit.py').read_bytes()).hexdigest(),
          'address': '127.0.0.1:11112', 'authenticated_login_verified': False, 'execution_qualified': False}
(root/'preparation.json').write_text(json.dumps(record, indent=2)+'\n')
PY
printf 'OpenD 10.11.7108 and read-only collector prepared; gateway has not been started or authenticated.\n'
REMOTE
