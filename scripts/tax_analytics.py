"""Mandatory shared-tax adapter after independent RD1 finance verification."""
import json
from strategy_tax import *
from rd1_financial_verify import _parse_journal, verify


def analyze_case(terminal):
    a=terminal['artifacts'];result=json.loads(read(a['result.json']))
    require(result['status']=='complete','incomplete paths retained but not scored')
    require(verify(a['journal.jsonl']['file'],a['journal.jsonl']['sha256'],a['result.json']['file'],a['result.json']['sha256'])['qualified'],'independent financial reconciliation failed')
    records=_parse_journal(read(a['journal.jsonl']))
    return dict(analyze_records(records,result),case_id=terminal['case_id'],source=a)
