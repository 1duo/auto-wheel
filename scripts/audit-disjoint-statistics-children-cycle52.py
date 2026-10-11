#!/usr/bin/env python3
"""Offline 17-field native and Go/Python 75-cut audits of the two children.

Keep the frozen 543-operation plan and unknown parent financial outcome intact.
The children can establish their exact data union, never a synthetic parent
acquisition, economic reference date, replay source gate, or performance result.
"""
from collections import Counter
import ast
from datetime import datetime, time, timedelta, timezone
import gzip
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
from zoneinfo import ZoneInfo

BASE = Path('/home/yuduo/Projects/AutoWheel-research')
ROOT = BASE / 'source-afa3075'
D = ROOT / 'research/simulation/2026-10-05'
PLAN = BASE / 'cycle51-statistics-disjoint-overlay-plan-20261007'
RUN = BASE / 'cycle51-statistics-overlay-execute-20261007'
OUTPUT = BASE / 'cycle52-disjoint-statistics-native-OI-20261007'
PYTHON = BASE / 'dbn-env-0.71.0/bin/python'
BINARY = BASE / 'cycle42-tools-20261007/autowheel-native-oi'
PARENT_OP = '01593d27c3a667a42b8736bba57c377e98dd6f19949f4601e2c08ef505abbf5f'


def sha(path):
    with path.open('rb') as source:
        return hashlib.file_digest(source, 'sha256').hexdigest()


def main():
    if OUTPUT.exists():
        raise ValueError('fresh private audit namespace required')
    module_path = ROOT / 'scripts/audit-all-acquired-statistics-cycle51-scope.py'
    if sha(module_path) != '1058e66999087af4ec66a117ef3a3c11ea6157ab18c22261dd0611f407e36f83':
        raise ValueError('accepted audit helper differs')
    spec = importlib.util.spec_from_file_location('accepted_scope', module_path)
    scope = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(scope)
    proof_module = scope.load_module('batch_acquisition_proof.py')
    reference = scope.load_module('audit-acquired-statistics-scope.py')
    comparison = scope.load_module('compare-native-oi-scope.py')
    read = proof_module.read_json
    qualification = scope.read_scope(D / 'databento-qualification.json')
    known = {r['path']: r['sha256'] for r in qualification['source_files']}
    sources = {}

    def bind(path, expected=None):
        path = Path(path)
        path = path if path.is_absolute() else ROOT / path
        if not path.is_file() or path != PYTHON and (path.is_symlink() or path.resolve() != path):
            raise ValueError('unsafe audit source')
        digest = sha(path)
        if expected is not None and digest != expected or str(path) in sources and sources[str(path)] != digest:
            raise ValueError('source binding differs')
        sources[str(path)] = digest
        return digest

    def accepted(path):
        path = Path(path)
        label = str(path.relative_to(ROOT)) if path.is_relative_to(ROOT) else str(path)
        if label not in known:
            raise ValueError('previously accepted source missing')
        return bind(path, known[label])

    for path in (module_path, Path(__file__), ROOT / 'internal/causaloi/oi.go',
                 ROOT / 'cmd/autowheel-native-oi/main.go', *[ROOT / 'scripts' / n for n in scope.PINS]):
        bind(path) if path == Path(__file__) else accepted(path)
    accepted(BINARY)
    if sha(BINARY) != 'cf59d0f4b49cf7d4893c1e6a26b90025f92ae13c8d3ca875ec2a874f94987612':
        raise ValueError('accepted Go exporter differs')
    accepted(PYTHON)
    plan_path = PLAN / 'manifest.json'
    bind(plan_path, '33a4bc5fe205e3013272e50c78e0ecd92e40793064d1403740f493bb0be63550')
    ledger_path = RUN / 'ledger-after.private.json'
    # The saved snapshot uses a different JSON layout from the live journal.
    # Bind its own already accepted digest, preserving the immutable bytes.
    accepted(ledger_path)
    ledger = read(ledger_path)
    if ledger['authorization_mode'] != 'uncapped' or ledger['lifetime_nano'] != 0 or len(ledger['reservations']) != 559:
        raise ValueError('accepted immutable financial snapshot differs')
    rows = {r['operation_id']: r for r in ledger['reservations']}
    if rows[PARENT_OP]['status'] != 'reserved':
        raise ValueError('original unknown parent outcome differs')
    parent_path = BASE / 'cycle24-selective-acquisition-queries-20261006/2026-09-18-statistics-002.json'
    bind(parent_path, 'dd72d518f59fb800d0291b01920d159e09a41950f8523785f283093715ec70a2')
    parent = read(parent_path)
    query_paths = [PLAN / ('2026-09-18-statistics-overlay-%03d.json' % i) for i in range(2)]
    queries = [read(p) for p in query_paths]
    if (len(parent['symbols']) != 2000 or queries[0]['symbols'] != parent['symbols'][:1000]
            or queries[1]['symbols'] != parent['symbols'][1000:]
            or set(queries[0]['symbols']) & set(queries[1]['symbols'])
            or any(any(q[k] != parent[k] for k in ('dataset', 'schema', 'stype_in', 'start', 'end')) for q in queries)):
        raise ValueError('exact disjoint full-day parent union differs')
    identities = BASE / 'full-definition-identities-20261006'
    accepted(identities / 'audit.json')
    parts = {p['date']: p for p in read(identities / 'audit.json')['partitions']}
    part = parts['2026-09-18']
    identity_path = identities / part['identity_file']
    bind(identity_path, part['identity_sha256'])
    definitions, count, digest = {}, 0, hashlib.sha256()
    with gzip.open(identity_path, 'rb') as source:
        for line in source:
            if len(line) > 4096 or not line.endswith(b'\n'):
                raise ValueError('definition line bound differs')
            digest.update(line)
            value = json.loads(line)
            if len(value) != 9 or value[2] in definitions:
                raise ValueError('definition identity ambiguous')
            definitions[value[2]] = value
            count += 1
    if count != part['records'] or digest.hexdigest() != part['identity_jsonl_sha256']:
        raise ValueError('definition count/hash/EOF differs')
    local = datetime.combine(datetime(2026, 9, 18).date(), time(), ZoneInfo('America/New_York'))
    cuts = [int((local + timedelta(minutes=n)).timestamp()) * 1_000_000_000 for n in [*range(575,945,5),960]]
    if len(cuts) != 75:
        raise ValueError('frozen cut scope differs')
    OUTPUT.mkdir(mode=0o700)
    results, totals = [], Counter()
    for query_path, query in zip(query_paths, queries):
        accepted(query_path)
        wire = proof_module.canonical_query(query)
        op = hashlib.sha256(wire + b'\0/batch.submit_job').hexdigest()
        proof_path = RUN / query_path.stem / 'proof.json'
        proof = read(proof_path)
        row = rows[op]
        bind(proof_path, row['data_sha256'])
        source = Path(proof['source_directory'])
        entries = {str(proof_path): {'sha256': row['data_sha256']}}
        for code in (*proof_module.CODE_FILES, proof_module.native_verifier_path(proof)):
            entries[code] = {'sha256': accepted(ROOT / code)}
        for name, field in (('query_path','query_file_sha256'),('acknowledgement_path','acknowledgement_file_sha256'),('ack_body_path','ack_body_sha256')):
            entries[proof[name]] = {'sha256': bind(proof[name], proof[field])}
        for name, field in (('receipt.json','receipt_sha256'),('job-source.json','job_sha256'),('files-source.json','file_list_sha256')):
            entries[str(source / name)] = {'sha256': bind(source / name, proof[field])}
        for member in proof['files']:
            entries[str(source / member['filename'])] = {'sha256': bind(source / member['filename'], member['sha256'])}
        if not proof_module.validate_batch_acquisition(row, str(proof_path), str(source / 'receipt.json'), entries, set(entries), ROOT)[0]:
            raise ValueError('original child acquisition proof binding failed')
        native = OUTPUT / (query_path.stem + '.native')
        commands = [[str(PYTHON), str(ROOT / 'scripts/audit-native-statistics-batch.py'), '--receipt', str(source / 'receipt.json'),
                     '--query', str(query_path), '--output-directory', str(native)],
                    [str(PYTHON), str(ROOT / 'scripts/verify-native-statistics-vectors.py'), '--source-directory', str(source),
                     '--audit-directory', str(native), '--identity-directory', str(identities), '--cutoff', '2026-09-18T13:35:00Z',
                     '--output', str(native / 'independent-reference.json')]]
        for i, command in enumerate(commands):
            fd = os.open(OUTPUT / (query_path.stem + '.native-%d.private.log' % i), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, 'wb') as log:
                if subprocess.run(command, stdout=log, stderr=log).returncode != 0:
                    raise ValueError('native independent pass failed; private evidence retained')
        projection = reference.project(native, query, definitions, cuts, sources)
        independent = read(native / 'independent-reference.json')
        bind(native / 'independent-reference.json')
        member = read(native / 'audit.json')['files'][0]
        if (projection['records'] != proof['native']['records'] or independent['records_compared_field_for_field'] != projection['records']
                or independent['vector_fields_per_record'] != 17
                or len(query['symbols']) - projection['first_cut_status_counts'].get('not_observed',0) != independent['OI_symbols_at_cutoff']):
            raise ValueError('native/vector independent comparison differs')
        bindings = []
        for raw in sorted(query['symbols']):
            definition = definitions.get(raw)
            bindings.append(dict(RawSymbol=raw, InstrumentID=0, Definition=None if definition is None else
                                 dict(InstrumentID=definition[1], ReceiveAt=definition[3], EventAt=definition[4])))
        config = dict(schema='AutoWheelNativeOIScopeV1', dataset='OPRA.PILLAR', utc_start=proof_module.utc_ns(parent['start']),
                      native_version=member['native_version'], expected_records=member['records'], vector_sha256=member['vector_sha256'],
                      vector_jsonl_sha256=member['vector_jsonl_sha256'], bindings=bindings, cuts=cuts)
        config_path = OUTPUT / (query_path.stem + '.scope.json')
        reference.save(config_path, config)
        bind(config_path)
        go_out = OUTPUT / (query_path.stem + '.go')
        fd = os.open(OUTPUT / (query_path.stem + '.go.private.log'), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, 'wb') as log:
            if subprocess.run([str(BINARY), '--config', str(config_path), '--vectors', str(native / member['vector_file']),
                               '--output-directory', str(go_out)], stdout=log, stderr=log).returncode != 0:
                raise ValueError('Go export failed; private evidence retained')
        bind(go_out / 'report.json')
        go = read(go_out / 'report.json')
        compared = comparison.compare(Path(projection['private_views_path']), go_out / 'OI-views.jsonl.gz', cuts, sorted(query['symbols']), sources)
        if (go['summary'] != dict(Records=member['records'],OpenInterestRecords=member['counts_by_stat_type']['9'],Snapshots=75,Observations=75000)
                or go['status_counts'] != projection['status_counts'] or compared['status_counts'] != projection['status_counts']
                or go['views_jsonl_sha256'] != compared['go_decoded_sha256'] or go['views_sha256'] != sources[str(go_out / 'OI-views.jsonl.gz')]
                or go['config_sha256'] != sources[str(config_path)] or go['source_gate_passed'] is not False
                or go['historical_performance_available'] is not False or go['financial_requests'] != 0):
            raise ValueError('Go/Python view or source policy differs')
        results.append(dict(query_file=query_path.name, query_sha256=sha(query_path), proof_sha256=row['data_sha256'], records=projection['records'], **compared))
        totals.update(compared['status_counts'])
        print(json.dumps({'children_audited': len(results), 'records': sum(r['records'] for r in results),
                          'observations': sum(r['observations'] for r in results)}), flush=True)
    for path, digest in sources.items():
        if sha(Path(path)) != digest:
            raise ValueError('source changed during audit')
    report = dict(schema='AutoWheelDisjointStatisticsNativeOIComponentV1', fields_per_record=17,
                  closed_cuts_per_child=75, children_audited=2, queried_symbols=2000,
                  records=sum(r['records'] for r in results), observations=150000,
                  go_python_views_equal=True, exact_disjoint_parent_data_scope_covered=True,
                  original_parent_financial_status='reserved_unknown_untouched',
                  canonical_statistics_operations=543, canonical_statistics_acquired=542,
                  economic_reference_dates_invented=0, full_study_coverage_proved=False,
                  source_gate_passed=False, historical_performance_available=False,
                  financial_requests=0, journal_writes=0, status_counts=dict(totals), results=results,
                  source_files=[dict(path=p,sha256=h) for p,h in sorted(sources.items())])
    reference.save(OUTPUT / 'summary.json', report)


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        fixed = {n.args[0].value for n in ast.walk(ast.parse(Path(__file__).read_text()))
                 if isinstance(n, ast.Call) and isinstance(n.func, ast.Name) and n.func.id == 'ValueError'
                 and len(n.args) == 1 and isinstance(n.args[0], ast.Constant) and isinstance(n.args[0].value, str)}
        print(json.dumps({'stopped':True,'error_type':type(exc).__name__,
                          'fixed_error':str(exc) if str(exc) in fixed else 'unclassified',
                          'financial_requests':0,'journal_writes':0}), flush=True)
        raise SystemExit(1)
