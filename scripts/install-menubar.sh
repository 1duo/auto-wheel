#!/bin/sh
set -eu
task_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
task_sdk=/Library/Developer/CommandLineTools/SDKs/MacOSX26.5.sdk
test -d "$task_sdk"
task_app="$HOME/Applications/AutoWheel.app"
task_build="$task_root/bin/AutoWheel.app"
task_cache="$task_root/var/swift-module-cache"
mkdir -p "$task_build/Contents/MacOS" "$task_build/Contents/Resources" "$task_cache" "$HOME/Applications" "$HOME/Library/LaunchAgents" "$task_root/var/log"
chmod 700 "$task_root/var" "$task_root/var/log"
xcrun swiftc -swift-version 5 -sdk "$task_sdk" -target arm64-apple-macosx14.0 -module-cache-path "$task_cache" -O -parse-as-library "$task_root"/apps/AutoWheelMenubar/*.swift -o "$task_build/Contents/MacOS/AutoWheelMenubar"
cat > "$task_build/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>com.autowheel.menubar</string>
<key>CFBundleExecutable</key><string>AutoWheelMenubar</string>
<key>CFBundleName</key><string>AutoWheel</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleVersion</key><string>1</string>
<key>CFBundleShortVersionString</key><string>1.0</string>
<key>LSMinimumSystemVersion</key><string>14.0</string>
<key>LSUIElement</key><true/>
<key>NSHighResolutionCapable</key><true/>
</dict></plist>
PLIST
cp "$task_root/docs/strategy.html" "$task_build/Contents/Resources/guide.html"
cp "$task_root/internal/calendarui/calendar.html" "$task_root/internal/calendarui/calendar.js" "$task_build/Contents/Resources/"
node "$task_root/scripts/attach-calendar-history.cjs" "$task_build/Contents/Resources/calendar.html"
codesign --force --sign - "$task_build"
task_uid=$(id -u)
for task_label in com.autowheel.menubar com.autowheel.msi-tunnel; do
  launchctl bootout "gui/$task_uid/$task_label" 2>/dev/null || true
done
rsync -a --delete "$task_build/" "$task_app/"
# Python's plist writer safely escapes paths; shell interpolation never creates XML.
/usr/bin/python3 - "$task_root" "$task_app" <<'PY'
import os, plistlib, sys
root, app = sys.argv[1:]
home = os.path.expanduser('~')
agents = os.path.join(home, 'Library/LaunchAgents')
entries = {
  'com.autowheel.msi-tunnel': {
    'ProgramArguments': ['/usr/bin/ssh', '-N', '-T', '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', 'ConnectTimeout=8', '-o', 'ExitOnForwardFailure=yes', '-o', 'ServerAliveInterval=15', '-o', 'ServerAliveCountMax=2', '-L', '127.0.0.1:19841:127.0.0.1:9841', 'yuduo@192.168.1.50'],
    'KeepAlive': True, 'ThrottleInterval': 15,
  },
  'com.autowheel.menubar': {
    'ProgramArguments': [os.path.join(app, 'Contents/MacOS/AutoWheelMenubar')],
    'KeepAlive': {'SuccessfulExit': False}, 'ThrottleInterval': 15,
  },
}
for label, entry in entries.items():
    entry.update(Label=label, RunAtLoad=True, ProcessType='Interactive' if label.endswith('menubar') else 'Background', StandardOutPath=os.path.join(root, 'var/log', label + '.out.log'), StandardErrorPath=os.path.join(root, 'var/log', label + '.err.log'), Umask=0o077)
    for key in ['StandardOutPath', 'StandardErrorPath']:
        fd = os.open(entry[key], os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        os.fchmod(fd, 0o600)
        os.close(fd)
    path = os.path.join(agents, label + '.plist')
    with open(path, 'wb') as f: plistlib.dump(entry, f)
    os.chmod(path, 0o600)
PY
launchctl bootstrap "gui/$task_uid" "$HOME/Library/LaunchAgents/com.autowheel.msi-tunnel.plist"
launchctl bootstrap "gui/$task_uid" "$HOME/Library/LaunchAgents/com.autowheel.menubar.plist"
printf 'Installed %s (API tunnel 127.0.0.1:19841 → msi:9841)\n' "$task_app"
