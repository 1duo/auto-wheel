#!/usr/bin/env python3
"""Preserve an inactive incomplete copy byte-for-byte; never qualify or overwrite it."""
import hashlib
import importlib.util
import json
from pathlib import Path

BASE=Path('/home/yuduo/Projects/AutoWheel-research')
ROOT=BASE/'source-afa3075'
TOOL=ROOT/'scripts/archive-research-artifacts-cycle73-v2.py'
TOOL_SHA='10eb572d5765a4c2b3edaac1f1ca6dbfcb5e26a6d2232d409ad1716e0b3e9a23'

def main():
    assert hashlib.sha256(TOOL.read_bytes()).hexdigest()==TOOL_SHA
    stopped=json.loads((BASE/'cycle70-prepaid-book-recovery-supervisor-20261008/runtime.json').read_bytes())
    assert stopped['finished'] is True and stopped['exit_code']==1
    proof_path=BASE/'cycle70-prepaid-book-recovery-20261008/2026-04-13-cmbp-1-002/proof.json'
    raw=proof_path.read_bytes();proof=json.loads(raw)
    assert proof['native']['all_eof_verified'] is True and proof['source_qualified'] is False
    assert proof['native']['records']==proof['provider_record_count']
    current=json.loads(Path('/home/yuduo/.local/share/autowheel-research/databento-budget.json').read_bytes())
    original=json.loads((BASE/'cycle56-full-book-acquisition-20261007/ledger-before.private.json').read_bytes())
    assert len(current['reservations'])==590 and current['reservations'][:575]==original['reservations']
    row=next(v for v in current['reservations'] if v['operation_id']==proof['reservation']['operation_id'])
    assert row['status']=='acquired' and row['data_sha256']==hashlib.sha256(raw).hexdigest()
    spec=importlib.util.spec_from_file_location('sealed_relocation',TOOL)
    tool=importlib.util.module_from_spec(spec);spec.loader.exec_module(tool)
    tool.__doc__=__doc__
    tool.RUN=BASE/'cycle73-forensic-copy-relocation-20261008'
    tool.LOWER=Path('/media/yuduo/Lexar1/AutoWheel-research-private-20261006/cycle73-forensic-copy-20261008')
    tool.ALLOWLIST=('cycle56-full-book-acquisition-20261007-retrieve-2026-04-13-cmbp-1-002',)
    tool.main()

if __name__=='__main__':
    main()
