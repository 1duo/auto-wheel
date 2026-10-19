"""One charged, bounded RD1 replay. No retries or source admission.

The offline binary is outside Lexar's noexec mount; inputs, journals and receipts
stay on its private research view. Forward accounts reconstruct from the common
2023 seed with verified prior input/journal prefixes. Sensitivities bind the
complete primary procedure and use its exact inputs without refitting.
"""
import json
import os
from pathlib import Path
import resource
import signal
import stat
import subprocess
import sys
import time

from rd1_financial_verify import _loads, verify
from rd1_availability_verify import analyze as availability_analyze
from rd1_effect_verify import analyze as effect_analyze
from rd1_attempt_verify import analyze as attempt_analyze
from rd1_cash_control import CASH,analyze as cash_analyze,preflight as cash_preflight
from rd1_stock_control import STOCK,analyze as stock_analyze,preflight as stock_preflight
from rd1_terminal_verify import analyze as terminal_analyze
from rd1_failure_fixture import FIXTURE,preflight as fixture_preflight,verify_stop as fixture_verify
from rd1_carry import review as carry_review
from rd1_forward import (completed_account, preflight as forward_preflight,
                         reproduced_outputs, reproduction_plan)
from rd1_metrics import SCENARIOS, analyze as metrics_analyze
from rd1_storage import PARENT, PHYSICAL, PROTOCOL_SHA256, budget, canonical, require, root, sha, write
from rd1_trials import PROFILE_IDS, case_identity, reserve, terminal

RUNTIME = Path('/home/yuduo/Projects/AutoWheel-research/rd1-runtime-261016-2037')
MAX_RSS_KIB = 4 * 1024 * 1024


def checked(path, pin, limit, *, lexar=True, retain=True, allow_empty=False):
    """Read one private immutable artifact and verify its physical bytes."""
    path = Path(path)
    require(path.is_absolute() and path.resolve() == path, 'noncanonical or symlinked artifact')
    info = path.lstat()
    require(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid()
            and info.st_mode & 0o077 == 0 and (info.st_size>0 or allow_empty) and info.st_size <= limit,
            'artifact is not private, regular and bounded')
    import hashlib
    def read_hash(value):
        digest = hashlib.sha256();parts = [];size = 0
        with os.fdopen(os.open(value, os.O_RDONLY | os.O_NOFOLLOW), 'rb') as handle:
            while chunk := handle.read(1 << 20):
                size += len(chunk);require(size <= limit, 'artifact grew beyond bound')
                digest.update(chunk)
                if retain:parts.append(chunk)
        require(digest.hexdigest() == pin, 'artifact bytes differ')
        return b''.join(parts)
    raw = read_hash(path)
    if lexar:
        require(path.is_relative_to(PARENT), 'artifact outside Lexar research namespace')
        physical = PHYSICAL/path.relative_to(PARENT)
        read_hash(physical)
    return raw


def validate(spec, run):
    case = spec['case'];case_identity(case)
    forward = case['purpose'] in ('forward','sensitivity')
    reproduction = case['purpose']=='reproduction'
    fixture = case['purpose']=='failure_fixture'
    cash = case['purpose']=='control' and case['profile_id']==CASH
    stock = case['purpose']=='control' and case['profile_id']==STOCK
    require(case['purpose'] in ('pilot', 'training', 'final_fit', 'reproduction', 'forward','sensitivity','failure_fixture','control')
            and (case['purpose']!='control' or cash or stock)
            and (case['sensitivity'] in SCENARIOS or fixture and case['sensitivity']==FIXTURE)
            and (case['sensitivity']!='primary')==(case['purpose'] in ('sensitivity','failure_fixture'))
            and (not fixture or case['sensitivity']==FIXTURE)
            and case['model_sha256'] == PROTOCOL_SHA256
            and (forward or reproduction or (cash or stock or case['profile_id'] in PROFILE_IDS) and case['parent_state_sha256'] == '0'*64),
            'dispatcher supports only frozen RD1 wheel/scenario cases')
    timeout = spec['max_runtime_seconds']
    require(type(timeout) is int and 1 <= timeout <= 900, 'unbounded replay timeout')
    def ref(value, limit):
        p = Path(value['file'])
        require(p.is_relative_to(run), 'case input outside this run')
        return checked(p, value['sha256'], limit,
            allow_empty=p.name=='diagnostics.jsonl' and p.parent.parent==run/'cases')
    def source_ref(value,limit):
        return checked(value['file'],value['sha256'],limit,retain=False)
    packet_raw = ref(spec['packet'], 256 << 20)
    require(sha(packet_raw) == case['input_sha256'], 'charged packet differs')
    packet = _loads(packet_raw)
    require(packet['schema'] == ('AutoWheelRD1CashControlPacketV1' if cash else 'AutoWheelRD1StockControlPacketV1' if stock else 'AutoWheelRD1ReplayPacketV1')
            and packet['protocol_sha256'] == PROTOCOL_SHA256
            and packet['admission_status'] == ('admitted_cash_control_under_RD1' if cash else 'admitted_stock_control_under_RD1' if stock else 'admitted_under_RD1')
            and packet['unqualified_inputs'] == []
            and packet['first_session'] == case['start']
            and packet['last_session'] == case['end'], 'unadmitted or mismatched packet')
    if not (forward or reproduction or cash or stock):
        _, mode, exit_rule = case['profile_id'].split('-')
        require(packet['opening_policies'] and all(
            p['profile'] == dict(mode=mode, exit=exit_rule)
            for p in packet['opening_policies']), 'packet profile differs from charged case')
    admission_raw = ref(spec['admission'], 4 << 20)
    require(sha(admission_raw) == packet['admission_receipt_sha256'], 'admission receipt differs')
    admission = _loads(admission_raw)
    cash_proof=stock_proof=None
    if cash:
        cash_proof=cash_preflight(case,packet,admission,spec['packet'],spec['admission'],source_ref)
    elif stock:
        stock_proof=stock_preflight(case,packet,admission,spec['packet'],spec['admission'],ref,source_ref)
    else:
        require(admission['schema'] == 'AutoWheelRD1InputAdmissionV1'
                and admission['protocol_sha256'] == PROTOCOL_SHA256
                and admission['status'] == 'admitted_under_RD1'
                and admission['unqualified_inputs'] == []
                and admission['cost_basis'] == 'verified_historical_customer_fees',
                'external source review incomplete or cost branch differs')
        # The receipt binds the reviewed body without a circular packet hash.
        body = dict(packet);del body['admission_receipt_sha256']
        require(admission['packet_body_sha256'] == sha(canonical(body)), 'reviewed packet body differs')
        sources = admission['source_artifacts']
        require(type(sources) is list and 0 < len(sources) <= 2000,
                'external review lacks bounded original evidence')
        for source in sources:checked(source['file'], source['sha256'], 512 << 20, retain=False)
    build_raw = ref(spec['build'], 4 << 20)
    require(not (Path(spec['build']['file']).parent/'withdrawal.json').exists(),
            'replay build has a recorded withdrawal; no automatic replacement or retry')
    require(sha(build_raw) == case['implementation_sha256'], 'charged implementation differs')
    build = _loads(build_raw)
    require(build['schema'] == 'AutoWheelRD1BuildReceiptV1'
            and build['target'] == 'linux/amd64'
            and build['package'] == 'autowheel/cmd/autowheel-rd1-replay'
            and build['source_closure_complete'] is True
            and build.get('supported_scenarios')==list(SCENARIOS)
            and build.get('supported_failure_fixtures')==[FIXTURE]
            and build.get('supported_controls')==[CASH,STOCK]
            and build.get('runtime_case_sensitivity_binding')=='explicit_CLI_and_result_label_v1',
            'unqualified replay build')
    require(type(build['source_artifacts']) is list and 0 < len(build['source_artifacts']) <= 2000,
            'build source closure absent')
    for source in build['source_artifacts']:
        require(Path(source['file']).is_relative_to(run), 'build source outside this run')
        checked(source['file'], source['sha256'], 16 << 20, retain=False)
    controller_binding(build)
    binary = Path(build['binary']['file'])
    require(binary.is_relative_to(RUNTIME) and os.access(binary, os.X_OK), 'binary outside private runtime')
    checked(binary, build['binary']['sha256'], 64 << 20, lexar=False)
    require(not (run/'cases'/case['case_id']).exists()
            and not (run/'dispatch'/case['case_id']).exists(), 'case artifacts already exist')
    forward_proof = forward_preflight(spec,packet,admission,run,ref,source_ref) if forward else None
    reproduction_proof=None
    if reproduction:
        require('original_forward_receipt' in spec, 'reproduction lacks its original charged forward receipt')
        original=completed_account(spec['original_forward_receipt'],run,ref,source_ref)
        reproduction_proof=reproduction_plan(case,original,spec['packet'],spec['admission'],spec['build'])
    fixture_proof=fixture_preflight(spec,run,ref,source_ref) if fixture else None
    return packet, binary, forward_proof, reproduction_proof, fixture_proof, cash_proof, stock_proof


def controller_binding(build):
    """The executing Python controllers must match the charged frozen build."""
    directory=Path(__file__).resolve().parent
    require(directory.is_relative_to(RUNTIME), 'dispatch controller outside private runtime')
    controllers={Path(x['source_relative_path']).name:x for x in build['source_artifacts']
                 if x['source_relative_path'].startswith('scripts/rd1_')}
    require('rd1_dispatch.py' in controllers and len(controllers)>=4,
            'build omits the dispatch controller closure')
    loaded={name+'.py' for name in sys.modules if name.startswith('rd1_')}
    require(loaded<=set(controllers), 'build omits an imported RD1 controller')
    for name,source in controllers.items():
        live=directory/name
        checked(live,source['sha256'],16<<20,lexar=False,retain=False)
        module=sys.modules.get(Path(name).stem)
        if module is not None:
            require(Path(module.__file__).resolve()==live, 'imported controller came from another revision')


def child_limits():
    os.umask(0o077)
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    resource.setrlimit(resource.RLIMIT_NOFILE, (128, 128))
    resource.setrlimit(resource.RLIMIT_FSIZE, (256 << 20, 256 << 20))


def group_rss_kib(group):
    total = 0
    for path in Path('/proc').iterdir():
        if not path.name.isdecimal():continue
        try:
            # stat's command may contain spaces/parentheses; parse after its last ).
            fields = (path/'stat').read_text().rsplit(')', 1)[1].split()
            if int(fields[2]) != group:continue
            for line in (path/'status').read_text().splitlines():
                if line.startswith('VmRSS:'):total += int(line.split()[1]);break
        except (OSError, ValueError, IndexError):continue  # Process exited during observation.
    return total


def execute(command, timeout, stdout, stderr):
    start = time.monotonic();peak = 0;stop = None
    env = dict(PATH='/usr/bin:/bin', LANG='C.UTF-8', TZ='UTC',
               GOMEMLIMIT='3GiB', GOMAXPROCS='2')
    with stdout.open('xb') as out, stderr.open('xb') as err:
        proc = subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=out, stderr=err,
                                start_new_session=True, preexec_fn=child_limits, env=env)
        try:
            while proc.poll() is None:
                peak = max(peak, group_rss_kib(proc.pid))
                if time.monotonic()-start >= timeout:stop = 'replay_timeout'
                elif peak > MAX_RSS_KIB:stop = 'replay_group_RSS_exceeded_4_GiB'
                if stop:
                    try:os.killpg(proc.pid, signal.SIGKILL)
                    except ProcessLookupError:pass
                    break
                time.sleep(.1)
            code = proc.wait(timeout=5)
        finally:
            if proc.poll() is None:
                try:os.killpg(proc.pid, signal.SIGKILL)
                except ProcessLookupError:pass
                proc.wait(timeout=5)
        out.flush();os.fsync(out.fileno());err.flush();os.fsync(err.fileno())
    return dict(exit_code=code, stopped_reason=stop,
                elapsed_milliseconds=int((time.monotonic()-start)*1000),
                sampled_process_group_peak_RSS_KiB=peak, RSS_sampling_milliseconds=100)


def run(spec):
    run_root = root(spec['run_id']);budget(run_root, spec['observation'], spec['max_runtime_seconds'])
    packet, binary, forward_proof, reproduction_proof, fixture_proof, cash_proof, stock_proof = validate(spec, run_root)
    for name in ('dispatch', 'cases'):
        p = run_root/name;p.mkdir(mode=0o700, exist_ok=True);info = p.lstat()
        require(p.resolve() == p and stat.S_ISDIR(info.st_mode)
                and info.st_uid == os.getuid() and info.st_mode & 0o077 == 0, 'unsafe case namespace')
    # No financial process starts before the durable reservation. Any crash after
    # this point leaves the case charged/live and requires explicit disposition.
    reservation = reserve(run_root, spec['observation'], spec['case'], spec['max_runtime_seconds'])
    case = spec['case'];directory = run_root/'dispatch'/case['case_id'];directory.mkdir(mode=0o700)
    invocation = write(directory/'invocation.json', spec);write(directory/'reservation.json', reservation)
    fixture_plan=write(directory/'failure-fixture-plan.json',fixture_proof) if fixture_proof else None
    output = run_root/'cases'/case['case_id']
    command = [str(binary), '-packet', spec['packet']['file'], '-packet-sha256', case['input_sha256'],
               '-out', str(output), '-sensitivity', case['sensitivity']]
    if fixture_proof:
        command.extend(['-fixture-missing-held-mark-at',fixture_proof['fault_at'],
                        '-fixture-contract-id',fixture_proof['fault_contract_id']])
    if cash_proof:command.extend(['-control',CASH])
    if stock_proof:command.extend(['-control',STOCK])
    failure = None
    try:
        resources = execute(command, spec['max_runtime_seconds'], directory/'stdout', directory/'stderr')
    except Exception as error:
        resources = None;failure = type(error).__name__
    artifacts = []
    for p in sorted(output.iterdir()) if output.is_dir() else []:
        require(p.name in ('journal.jsonl', 'diagnostics.jsonl', 'result.json'), 'unexpected replay output')
        info = p.lstat()
        require(stat.S_ISREG(info.st_mode) and not p.is_symlink()
                and info.st_mode & 0o077 == 0 and info.st_size <= 256 << 20, 'unsafe replay output')
        raw = p.read_bytes()
        require(sha(raw) == sha((PHYSICAL/p.relative_to(PARENT)).read_bytes()), 'physical replay differs')
        artifacts.append(dict(file=str(p), sha256=sha(raw), bytes=len(raw)))
    by_name = {Path(p['file']).name:p for p in artifacts}
    financial = availability = effects = decisions = analytics = carry = reproduction = fixture_result = cash_result = stock_result = terminal_risk = None
    status = 'failed';reason = failure or 'replay_failed_or_outputs_absent'
    if 'journal.jsonl' in by_name and 'result.json' in by_name:
        journal = by_name['journal.jsonl'];result = by_name['result.json']
        financial = verify(journal['file'], journal['sha256'], result['file'], result['sha256'])
        if fixture_proof:
            try:
                require('diagnostics.jsonl' in by_name, 'fixture diagnostics absent')
                diagnostics=by_name['diagnostics.jsonl']
                fixture_result=fixture_verify(fixture_proof,resources,
                    checked(journal['file'],journal['sha256'],32<<20),
                    checked(diagnostics['file'],diagnostics['sha256'],32<<20),
                    _loads(checked(result['file'],result['sha256'],4<<20)),financial)
                status='incomplete';reason='expected missing-held-mark stop verified; account performance remains incomplete'
            except (ValueError,KeyError,TypeError) as error:
                fixture_result=dict(expectation_status='failed',reason=str(error)[:240],full_period_metrics=None)
                status='failed';reason='missing-held-mark failure expectation not established'
        elif cash_proof:
            if resources and resources['exit_code']==0 and not resources['stopped_reason'] and financial['qualified']:
                cash_result=cash_analyze(journal['file'],journal['sha256'],result['file'],result['sha256'],
                                        spec['packet']['file'],spec['packet']['sha256'])
                terminal_risk=terminal_analyze(journal['file'],journal['sha256'],result['file'],result['sha256'],
                                        spec['packet']['file'],spec['packet']['sha256'])
                if cash_result['status']==terminal_risk['status']=='arithmetic_complete_pending_external_admission':
                    status='complete';reason='cash descriptive control reconciled on its full source-admitted schedule; wheel research pending'
                else:status='incomplete';reason='independent cash control, schedule or terminal-risk verification failed'
            else:status='incomplete';reason='cash process or independent financial reconciliation incomplete'
        elif stock_proof:
            if resources and resources['exit_code']==0 and not resources['stopped_reason'] and financial['qualified'] and 'diagnostics.jsonl' in by_name:
                d=by_name['diagnostics.jsonl']
                stock_result=stock_analyze(journal['file'],journal['sha256'],result['file'],result['sha256'],
                    spec['packet']['file'],spec['packet']['sha256'],d['file'],d['sha256'])
                terminal_risk=terminal_analyze(journal['file'],journal['sha256'],result['file'],result['sha256'],
                    spec['packet']['file'],spec['packet']['sha256'])
                if stock_result['status']==terminal_risk['status']=='arithmetic_complete_pending_external_admission':
                    status='complete';reason='original-universe stock descriptive control reconciled; wheel research pending'
                else:status='incomplete';reason='independent stock control source, allocation, dividend, schedule or terminal-risk verification failed'
            else:status='incomplete';reason='stock control process, diagnostics or independent money reconciliation incomplete'
        elif resources and resources['exit_code'] == 0 and not resources['stopped_reason'] and financial['qualified']:
            args=(journal['file'],journal['sha256'],result['file'],result['sha256'],
                  spec['packet']['file'],spec['packet']['sha256'])
            availability=availability_analyze(*args,expected_sensitivity=case['sensitivity'])
            effects=effect_analyze(*args,expected_sensitivity=case['sensitivity'])
            analytics=metrics_analyze(*args,expected_sensitivity=case['sensitivity'])
            terminal_risk=terminal_analyze(*args,expected_sensitivity=case['sensitivity'])
            d=by_name.get('diagnostics.jsonl')
            decisions=(attempt_analyze(*args,d['file'],d['sha256'],expected_sensitivity=case['sensitivity'])
                if d else dict(status='failed',checks=None,error='durable diagnostics absent'))
            checks=[availability['status'],effects['status'],analytics['status'],decisions['status'],terminal_risk['status']]
            if forward_proof and forward_proof['previous'] is not None:
                carry=carry_review(forward_proof['previous'],dict(case=case,packet=spec['packet'],journal=journal,result=result))
                checks.append(carry['status'])
            if reproduction_proof:
                try:reproduction=reproduced_outputs(reproduction_proof,artifacts)
                except ValueError as error:
                    reproduction=dict(status='failed',reason=str(error)[:240])
                checks.append(reproduction['status'])
            if all(x=='arithmetic_complete_pending_external_admission' for x in checks):
                status='complete';reason=case['sensitivity']+' account money, availability and sampled schedule complete; research qualification pending'
            else:
                status='incomplete';reason='independent availability, source effects, decisions, terminal risk, sampled schedule, carry or reproduction verification failed'
        else:
            status = 'incomplete';reason = (resources or {}).get('stopped_reason') or 'replay_or_independent_money_verification_incomplete'
    report = dict(schema='AutoWheelRD1DispatchReceiptV1', case=case, resources=resources,invocation=invocation,
                  status=status, reason=reason, artifacts=artifacts, financial_verification=financial,
                  availability_verification=availability,sampled_schedule_and_analytics=analytics,
                  source_effect_verification=effects,decision_verification=decisions,
                  forward_preflight=forward_proof,carry_verification=carry,
                  reproduction_preflight=reproduction_proof,reproduction_verification=reproduction,
                  failure_fixture_plan=fixture_plan,failure_fixture_verification=fixture_result,
                  cash_control_preflight=cash_proof,cash_control_verification=cash_result,
                  stock_control_preflight=stock_proof,stock_control_verification=stock_result,
                  terminal_risk_verification=terminal_risk,
                  automatic_retries=False, source_admission_performed_by_dispatcher=False,
                  independent_schedule_and_settlement_verification_complete=False,
                  full_research_complete=False)
    receipt = write(directory/'receipt.json', report)
    terminal(run_root, case['case_id'], status, receipt['sha256'], reason)
    print(json.dumps(dict(receipt=receipt, status=status, full_research_complete=False)))


if __name__ == '__main__':run(json.load(sys.stdin))
