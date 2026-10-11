"""Focused source-scope, fail-closed census, and actual Linux hold controls."""
import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import research_source_quiescence as guard


class QuiescenceTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory();self.addCleanup(temp.cleanup)
        self.root = Path(temp.name).resolve()
        self.view = self.root / 'view';self.physical = self.root / 'physical'
        self.view.mkdir();self.physical.mkdir()
        self.source = self.view / 'cycle92-retrieve-fixture'
        self.lower = self.physical / self.source.name
        self.source.mkdir();self.lower.mkdir()
        self.paths = guard.source_paths(self.source, self.view, self.physical)

    def fake_proc(self):
        proc = self.root / 'proc';proc.mkdir()
        process = proc / '123';process.mkdir();(process / 'fd').mkdir()
        (process / 'cwd').symlink_to(self.root)
        (process / 'root').symlink_to(self.root)
        (process / 'maps').write_text('')
        return proc, process

    def inspect(self, proc):
        return guard.census(self.paths, privileged=False, proc=proc, executable=sys.executable)

    def test_scopes_reject_siblings_traversal_and_links(self):
        for path in (self.view, self.root / self.source.name,
                     self.view / '..' / self.source.name, self.view / 'unrelated'):
            with self.subTest(path=path), self.assertRaises(ValueError):
                guard.source_paths(path, self.view, self.physical)
        link = self.view / 'cycle93-retrieve-fixture';link.symlink_to(self.source)
        with self.assertRaises(ValueError):
            guard.source_paths(link, self.view, self.physical)
        self.assertEqual(self.paths, (self.source, self.lower))

    def test_sibling_fd_is_ignored_but_target_and_deleted_fd_block(self):
        proc, process = self.fake_proc();fd = process / 'fd/7'
        fd.symlink_to(self.view / (self.source.name + '-sibling') / 'file')
        self.assertEqual(self.inspect(proc), {'status': 'quiescent'})
        fd.unlink();fd.symlink_to(str(self.source / 'file') + ' (deleted)')
        result = self.inspect(proc)
        self.assertEqual((result['status'], result['kind'], result['scope_index']), ('in_use', 'fd', 0))

    def test_physical_map_and_escaped_path_block(self):
        proc, process = self.fake_proc()
        (process / 'maps').write_text('1000-2000 rw-s 0000 00:00 1 ' + str(self.lower / 'file') + '\n')
        result = self.inspect(proc)
        self.assertEqual((result['status'], result['kind'], result['scope_index']), ('in_use', 'map', 1))
        spaced = self.root / 'with space'
        path = str(spaced / 'mapped').replace(' ', r'\040')
        (process / 'maps').write_text('1000-2000 rw-s 0000 00:00 1 ' + path + '\n')
        result = guard.census((spaced, self.lower), privileged=False, proc=proc, executable=sys.executable)
        self.assertEqual(result['status'], 'in_use')

    def test_surviving_process_missing_census_surface_fails_closed(self):
        proc, process = self.fake_proc();(process / 'maps').unlink()
        self.assertEqual(self.inspect(proc)['status'], 'census_unavailable')

    def test_original_checks_delegated_and_other_scope_rejected(self):
        path = Path(__file__).with_name('seal-external-research-source-scoped.py')
        spec = importlib.util.spec_from_file_location('scoped_seal_candidate', path)
        module = importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
        original = module.load_original()
        proxy = module.ScopedVerifier(original, self.source)
        self.assertIs(proxy.verify_parent, original.verify_parent)
        self.assertIs(proxy.seal, original.seal)
        with self.assertRaises(ValueError):
            proxy.quiescent(self.root)

    @unittest.skipUnless(os.environ.get('AUTOWHEEL_PRIVILEGED_CENSUS_TEST') == '1', 'actual Linux privileged census')
    def test_actual_linux_logical_physical_and_sibling_holds(self):
        for namespace, mode, expected in ((self.source, 'fd', 'in_use'),
                                          (self.lower, 'cwd', 'in_use'),
                                          (self.lower, 'map', 'in_use'),
                                          (self.view / 'sibling', 'fd', 'quiescent')):
            with self.subTest(mode=mode, namespace=namespace):
                namespace.mkdir(exist_ok=True)
                member = namespace / 'file';member.write_bytes(b'fixture')
                code = "import mmap,os,sys;mode=sys.argv[1];p=sys.argv[2];f=None;m=None\nif mode=='cwd':os.chdir(p)\nelse:\n f=open(p,'rb')\n if mode=='map':m=mmap.mmap(f.fileno(),0,access=mmap.ACCESS_READ);f.close()\nprint('ready',flush=True);input()"
                child = subprocess.Popen([sys.executable, '-u', '-c', code, mode,
                    str(namespace if mode == 'cwd' else member)], stdin=subprocess.PIPE,
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                try:
                    self.assertEqual(child.stdout.readline().strip(), 'ready')
                    result = guard.census(self.paths)
                    self.assertEqual(result['status'], expected, result)
                    if expected == 'in_use':
                        self.assertEqual(result['pid'], child.pid)
                finally:
                    child.communicate('\n', timeout=10)
                self.assertEqual(guard.census(self.paths)['status'], 'quiescent')


if __name__ == '__main__':
    unittest.main(verbosity=2)
