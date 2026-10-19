"""Independent frozen-inventory entry guard; no predictive loss-bound claim."""
from fractions import Fraction
from rd1_metrics import integer
from rd1_storage import require

RULE='original_dd_minus40_v1'
BUDGET=Fraction(618445,8892202)


def entry_rule(packet):
    value=packet.get('entry_risk_rule','')
    require(type(value) is str and value in ('',RULE),'unknown entry risk model')
    if value:
        require(packet.get('contract_assumption')=='conditional_fixed_library_standard_100_shares_pending_actual_path_review'
            and packet.get('call_eligibility')=='stock_cost_floor_v1'
            and all(p['profile']['mode']=='conservative' for p in packet['sampled_data']['opening_policies']),
            'entry stress experiment requires conservative conditional floor paths')
    return value


def prepare_trace(records,trace,result):
    peak=8404717;receivables=0
    for i,r in enumerate(records):
        if i in trace['snapshots']:
            trace['snapshots'][i].update(risk_peak_nav=peak,receivables=receivables)
        if r['effect']['action']=='mark':
            peak=max(peak,integer(r['mark']['NAV_cents'],1))
        receivables=integer(receivables+r['receivable_delta_cents'])
    require(result['entry_risk_peak_NAV_cents']==peak,'entry-risk sampled peak differs')
    return dict(peak_NAV_cents=peak,receivables_cents=receivables,snapshots=len(trace['snapshots']))


def allowed(snapshot,obs,contract,price,fee,tariff):
    peak=integer(snapshot['risk_peak_nav'],1)
    equity=Fraction(integer(snapshot['cash'])+integer(snapshot['receivables']))
    physical=integer(tariff['dated_fees']['physical_exercise_cents'],0,10000)
    def shocked(symbol):
        require(symbol in obs['stock_books'],'missing underlying for entry stress')
        return Fraction(3,5)*integer(obs['stock_books'][symbol]['bid_cents'],1,1_000_000_000)
    for symbol,p in snapshot['positions'].items():
        spot=shocked(symbol);require(p['shares'] in (0,100),'nonstandard entry-risk lot')
        equity+=p['shares']*spot
        c=p['short']
        if c:
            strike=integer(c['strike_cents'],1,1_000_000_000)
            require((c['kind']=='put' and not p['shares']) or (c['kind']=='call' and p['shares']==100),
                'uncovered inventory in entry stress')
            intrinsic=max(Fraction(0),strike-spot if c['kind']=='put' else spot-strike)
            if intrinsic:equity-=100*intrinsic+physical
        require(equity.denominator==1,'fractional-cent aggregate entry stress')
        integer(equity.numerator,-100_000_000_000)
    require(contract['symbol'] not in snapshot['positions'] and contract['kind']=='put','entry stress lifecycle differs')
    credit=100*integer(price,1,1_000_000_000)-integer(fee,0,10000)
    require(credit>0,'nonpositive proposed entry credit')
    equity+=credit
    intrinsic=max(Fraction(0),integer(contract['strike_cents'],1,1_000_000_000)-shocked(contract['symbol']))
    if intrinsic:equity-=100*intrinsic+physical
    require(equity.denominator==1,'fractional-cent aggregate entry stress')
    integer(equity.numerator,-100_000_000_000)
    return equity>=peak*(1-BUDGET)
