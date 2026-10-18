#!/usr/bin/env python3
"""Apply mandatory RD1 tax scenarios to a complete, hash-bound strategy path.

This certifies tax arithmetic only. Independent price, finance, policy and
funding verification remains necessary before evaluation or deployment.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
from strategy_tax import analyze_records, require
from strategy_tax_valuation import annual_liquidation_values


def load(path, pin, limit):
    p=Path(path)
    require(p.is_file() and p.stat().st_size<=limit,'bounded regular input required')
    raw=p.read_bytes()
    require(hashlib.sha256(raw).hexdigest()==pin,'input hash mismatch')
    return raw


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    for name in ('journal','journal-sha256','result','result-sha256','out'):parser.add_argument('--'+name,required=True)
    args=parser.parse_args()
    journal=load(args.journal,args.journal_sha256,32<<20)
    result=json.loads(load(args.result,args.result_sha256,2<<20))
    records=[json.loads(line) for line in journal.splitlines()]
    report=analyze_records(records,result)
    report['annual_liquidation_values']=annual_liquidation_values(records,result)
    report.update(qualification='tax_arithmetic_only',sources=dict(journal_sha256=args.journal_sha256,result_sha256=args.result_sha256))
    target=Path(args.out)
    fd=os.open(target,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(fd,'wb') as out:
        out.write(json.dumps(report,sort_keys=True,separators=(',',':')).encode()+b'\n');out.flush();os.fsync(out.fileno())


if __name__=='__main__':main()
