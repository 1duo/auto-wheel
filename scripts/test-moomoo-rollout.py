#!/usr/bin/env python3
"""Exercise rollout failure/isolation on Linux using fake services and HOME."""
import fcntl
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('deploy-moomoo-msi.sh').read_text()
REMOTE = SCRIPT.split("<<'REMOTE'\n", 1)[1].rsplit('\nREMOTE', 1)[0]


class RolloutTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='autowheel-moomoo-rollout-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.home = self.root / 'home'
        self.runtime = self.home / '.local/share/autowheel-moomoo'
        self.state = self.home / '.local/state/autowheel-moomoo'
        self.unit = self.root / 'unit.service'
        self.bin = self.root / 'bin'
        for path in (self.runtime / 'releases/prior', self.state, self.bin,
                     self.home / '.config/autowheel-moomoo',
                     self.home / '.local/share/autowheel/toolchain/go/bin'):
            path.mkdir(parents=True, exist_ok=True)
        (self.runtime / 'current').symlink_to('releases/prior')
        (self.state / 'LIVE.json').write_text('prior activation must be revoked')
        (self.home / '.config/autowheel-moomoo/config.json').write_text('{}')
        self.unit.write_text('prior unit')
        self.events = self.root / 'events'
        self.env = dict(os.environ, HOME=str(self.home), PATH=f'{self.bin}:' + os.environ['PATH'],
                        REVIEW_EVENTS=str(self.events), REVIEW_HEALTH='pass')
        self.executable(self.bin / 'sudo', '#!/bin/sh\nif [ "$1" = -n ]; then shift; fi\nexec "$@"\n')
        self.executable(self.bin / 'systemctl', '#!/bin/sh\nprintf "service %s\\n" "$*" >> "$REVIEW_EVENTS"\nexit 0\n')
        self.executable(self.bin / 'python3', '#!/bin/sh\n[ "$REVIEW_HEALTH" = pass ]\n')
        self.executable(self.home / '.local/share/autowheel/toolchain/go/bin/go', '''#!/bin/sh
printf 'build\n' >> "$REVIEW_EVENTS"
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then shift; output=$1; break; fi
  shift
done
cat > "$output" <<'FAKE'
#!/bin/sh
if [ "$1" = configure-five-slots ]; then
  shift
  while [ "$#" -gt 0 ]; do
    if [ "$1" = --config ]; then shift; policy_path=$1; fi
    shift
  done
  printf '{"slots":5}' > "$policy_path"
  if [ "$REVIEW_CONFIG_FAIL" = 1 ]; then exit 1; fi
fi
exit 0
FAKE
chmod 700 "$output"
''')

    def executable(self, path, contents):
        path.write_text(contents)
        path.chmod(0o700)

    def source(self, upload):
        root = self.runtime / 'staging' / upload
        for sub in ('cmd', 'internal', 'deploy', 'scripts'):
            (root / sub).mkdir(parents=True)
        (root / 'cmd/main.go').write_text('package main\n')
        (root / 'internal/data.go').write_text('package data\n')
        (root / 'go.mod').write_text('module fixture\n')
        (root / 'go.sum').write_text('')
        (root / 'deploy/autowheel-moomoo.service').write_text('new unit')
        (root / 'scripts/deploy-moomoo-msi.sh').write_text(SCRIPT)
        return root

    def run_rollout(self, upload, health='pass'):
        root = self.source(upload)
        # Redirect only the privileged unit path. Every service command runs
        # through our fake systemctl; neither the real HOME nor a daemon is used.
        body = REMOTE.replace('/etc/systemd/system/autowheel-moomoo.service', str(self.unit))
        env = dict(self.env, REVIEW_HEALTH=health)
        result = subprocess.run(['bash', '-s', '--', upload], input=body, text=True,
                                capture_output=True, env=env, timeout=20)
        self.assertFalse(root.exists(), 'temporary upload was not cleaned')
        return result

    def test_failed_health_restores_release_unit_and_service_while_paused(self):
        result = self.run_rollout('Failed-Health', health='fail')
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(os.readlink(self.runtime / 'current'), 'releases/prior')
        self.assertEqual(self.unit.read_text(), 'prior unit')
        self.assertTrue((self.state / 'HALT').exists())
        self.assertFalse((self.state / 'LIVE.json').exists())
        self.assertIn('service start autowheel-moomoo.service', self.events.read_text())
        self.assertNotIn(' autowheel.service', self.events.read_text())
        self.assertEqual((self.home / '.config/autowheel-moomoo/config.json').read_text(), '{}')

    def test_partial_config_failure_restores_compatible_previous_config(self):
        self.env['REVIEW_CONFIG_FAIL'] = '1'
        result = self.run_rollout('Failed-Configuration')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(os.readlink(self.runtime / 'current'), 'releases/prior')
        self.assertEqual((self.home / '.config/autowheel-moomoo/config.json').read_text(), '{}')
        self.assertFalse((self.state / 'LIVE.json').exists())
        self.assertNotIn(' autowheel.service', self.events.read_text())

    def test_success_and_repeat_reuse_verified_immutable_release(self):
        result = self.run_rollout('Successful-Release')
        self.assertEqual(result.returncode, 0, result.stderr)
        first = os.readlink(self.runtime / 'current')
        self.assertNotEqual(first, 'releases/prior')
        self.assertEqual(self.unit.read_text(), 'new unit')
        self.assertEqual((self.home / '.config/autowheel-moomoo/config.json').read_text(), '{"slots":5}')
        self.assertTrue((self.runtime / first / 'SOURCE.SHA256SUMS').exists())
        result = self.run_rollout('Repeat-Release')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(os.readlink(self.runtime / 'current'), first)
        self.assertEqual(self.events.read_text().splitlines().count('build'), 1)

    def test_foreign_symlinked_state_is_never_fenced(self):
        foreign = self.root / 'foreign-state'
        self.state.rename(foreign)
        self.state.symlink_to(foreign)
        result = self.run_rollout('Foreign-State')
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue((foreign / 'LIVE.json').exists())
        self.assertFalse((foreign / 'HALT').exists())
        self.assertEqual(os.readlink(self.runtime / 'current'), 'releases/prior')
        self.assertFalse(self.events.exists())

    def test_busy_rollout_lock_prevents_activation_changes(self):
        with (self.runtime / 'deploy.lock').open('w') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = self.run_rollout('Concurrent-Deploy')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(os.readlink(self.runtime / 'current'), 'releases/prior')
        self.assertTrue((self.state / 'LIVE.json').exists())
        self.assertFalse(self.events.exists())


if __name__ == '__main__':
    unittest.main()
