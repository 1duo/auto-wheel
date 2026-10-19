"""Check the recovered research snapshot without running providers or replays."""
import ast
import hashlib
import json
from pathlib import Path
import re


ROOT = Path(__file__).resolve().parent.parent
MANIFEST = ROOT / 'docs/research-source-recovery-20261019.json'


def main():
    manifest = json.loads(MANIFEST.read_bytes())
    files = dict(manifest['files'])
    files.update({'scripts/' + name: {'sha256': pin}
                  for name, pin in manifest['existing_tax_sources'].items()})
    modules = {Path(name).stem for name in files if name.endswith('.py')}
    counts = {'go': 0, 'python': 0}
    for name, ref in files.items():
        path = Path(name)
        if path.is_absolute() or '..' in path.parts:
            raise ValueError('recovery manifest path must stay within the repository')
        raw = (ROOT / path).read_bytes()
        if hashlib.sha256(raw).hexdigest() != ref['sha256']:
            raise ValueError('source differs from the recovery snapshot: ' + name)
        if path.suffix == '.go':
            counts['go'] += 1
            if re.search(r'"autowheel/internal/(robinhood|runtime|accountservice|sharedclient|sharedbroker)', raw.decode()):
                raise ValueError('offline research source imports production transport/state: ' + name)
        elif path.suffix == '.py':
            counts['python'] += 1
            tree = ast.parse(raw, filename=name)
            for node in ast.walk(tree):
                imports = [a.name for a in node.names] if isinstance(node, ast.Import) else (
                    [node.module] if isinstance(node, ast.ImportFrom) and node.module else [])
                for module in imports:
                    if module.startswith(('rd1_', 'simple_research_', 'strategy_tax', 'tax_analytics')) and module not in modules:
                        raise ValueError('missing recovered Python dependency: ' + module)
    print(json.dumps({'source_hashes_match': True, 'files': len(files), **counts,
                      'provider_requests': 0, 'historical_replays': 0}))


if __name__ == '__main__':
    main()
