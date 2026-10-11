#!/usr/bin/env python3
"""Restore preserved forensic read paths without depending on a later journal state."""
import hashlib
import importlib.util
from pathlib import Path
import sys

BASE=Path('/home/yuduo/Projects/AutoWheel-research')
TOOL=BASE/'source-afa3075/scripts/archive-research-artifacts-cycle73-v2.py'
TOOL_SHA='10eb572d5765a4c2b3edaac1f1ca6dbfcb5e26a6d2232d409ad1716e0b3e9a23'
SOURCE_NAME='cycle56-full-book-acquisition-20261007-retrieve-2026-04-13-cmbp-1-002'

def main():
    if sys.argv[1:]!=['--restore',SOURCE_NAME]:
        raise ValueError('only the exact preserved forensic view may be restored')
    if hashlib.sha256(TOOL.read_bytes()).hexdigest()!=TOOL_SHA:
        raise ValueError('sealed storage verifier changed')
    spec=importlib.util.spec_from_file_location('sealed_relocation',TOOL)
    tool=importlib.util.module_from_spec(spec);spec.loader.exec_module(tool)
    tool.RUN=BASE/'cycle73-forensic-copy-relocation-20261008'
    tool.LOWER=Path('/media/yuduo/Lexar1/AutoWheel-research-private-20261006/cycle73-forensic-copy-20261008')
    tool.ALLOWLIST=(SOURCE_NAME,)
    # The sealed restore branch validates the intent, mount protections and all
    # retained bytes before Q/journal planning. Later acquisitions cannot block it.
    tool.main()

if __name__=='__main__':
    main()
