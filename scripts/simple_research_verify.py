"""Independent arithmetic for experimental accounts; source review is separate."""
from datetime import date, timedelta
import re
from rd1_attempt_verify import reconstruct
from rd1_availability_verify import availability
from rd1_effect_verify import effects
from rd1_financial_verify import verify
from rd1_metrics import NY,at,cash_exposure,integer,inventory_metrics,series_metrics,session_calendar
from rd1_storage import require
from simple_research_entry_risk import entry_rule,prepare_trace,allowed as entry_allowed

ASSUMPTION='exploratory_pilot_standard_100_shares_actions_not_fully_qualified'
CONDITIONAL='conditional_2021_2022_standard_100_shares_pending_actual_path_review'
CONDITIONAL_PATH='conditional_fixed_library_standard_100_shares_pending_actual_path_review'
FIT_SYMBOLS={'AAPL','MSFT','BA','AMD','JPM','V','INTC','PYPL','SQ','XOM','MRNA','MU'}
PATH_SYMBOLS=FIT_SYMBOLS|{'BAC','CVX','JNJ','CRM','ORCL','WMT'}
CALL_FLOOR='stock_cost_floor_v1'


def call_eligibility(packet):
    rule=packet.get('call_eligibility','')
    require(type(rule) is str and rule in ('',CALL_FLOOR)
        and (not rule or packet.get('contract_assumption')==CONDITIONAL_PATH),
        'experimental call eligibility outside its declared conditional path')
    return rule


class PilotModel:
    def __init__(self,packet):
        self.packet=packet;self.data=packet['sampled_data'];self.costs=packet['modeled_costs']
        self.call_rule=call_eligibility(packet)
        self.entry_risk_rule=entry_rule(packet)
        require(packet['schema']=='AutoWheelSimpleResearchPacketV1'
            and packet['contract_assumption']==ASSUMPTION
            and self.data['schema']=='AutoWheelSimpleResearchDataV1'
            and self.data['admission_status']=='observed_pilot_prices_with_explicit_contract_assumption'
            and self.data['first_session']=='2021-01-04' and self.data['last_session']=='2021-04-07'
            and self.data['protocol_sha256']==self.costs['plan_sha256']
            and self.data['tariffs']==self.data['standard_terms']==self.data['unqualified_inputs']==[]
            and at(self.costs['designed_at']).year==2026 and type(self.costs['scale']) is int
            and self.costs['scale'] in (1,2),'ambiguous or out-of-scope exploratory pilot model')
        require(all(set(p['symbols'])=={'AAPL','MSFT'} for p in self.data['opening_policies']),
            'pilot universe differs from its fixed observed-data question')

    def tariff(self,clock):
        scale=self.costs['scale']
        zero=dict(flat_fee_cents=0,notional_numerator=0,notional_denominator=1,
            share_numerator=0,share_denominator=1,share_cap_cents=0)
        sale=dict(zero,flat_fee_cents=100*scale)
        return dict(dated_fees=dict(date=str(clock.astimezone(NY).date()),known_at=self.costs['designed_at'],
            source_sha256=self.costs['plan_sha256'],option_open_cents=65*scale,
            option_close_cents=65*scale,physical_exercise_cents=100*scale,stock_sale=sale),
            stock_buy_fee_cents=100*scale,physical_stock_sale=zero)

    def standard(self,symbol,clock):
        return symbol in ('AAPL','MSFT') and '2021-01-04'<=str(clock.astimezone(NY).date())<='2021-04-07'

    def selection_mode(self,mode,kind):
        return 'conservative' if mode=='aggressive' and kind=='call' else mode

    def call_allowed(self,position,contract,clock):
        if not self.call_rule:return True
        require(position is not None and position['shares']==100 and position['short'] is None
            and position['available']<=clock and contract['kind']=='call',
            'stock-cost gate requires settled covered shares')
        cost=integer(position['cost'],1,100_000_000_000)
        strike=integer(contract['strike_cents'],1,1_000_000_000)
        physical=integer(self.tariff(clock)['dated_fees']['physical_exercise_cents'],0,10000)
        required=integer(cost+physical,1,100_000_000_000)
        return 100*strike>=required

    def entry_risk_allowed(self,snapshot,observation,contract,price,fee,clock):
        return not self.entry_risk_rule or entry_allowed(snapshot,observation,contract,price,fee,self.tariff(clock))


class ReviewedModel(PilotModel):
    """Reuse fixed cost/strike formulas while requiring explicit source intervals."""
    def __init__(self,packet):
        self.packet=packet;self.data=packet['sampled_data'];self.costs=packet['modeled_costs']
        self.call_rule=call_eligibility(packet)
        self.entry_risk_rule=entry_rule(packet)
        require(packet['schema']=='AutoWheelSimpleResearchPacketV1'
            and not packet.get('contract_assumption')
            and self.data['schema']=='AutoWheelSimpleResearchDataV1'
            and self.data['admission_status']=='reviewed_sources_under_explicit_modeled_costs'
            and self.data['tariffs']==self.data['unqualified_inputs']==[]
            and self.data['standard_terms']
            and self.data['protocol_sha256']==self.costs['plan_sha256']
            and re.fullmatch('[0-9a-f]{64}',self.costs['plan_sha256'])
            and at(self.costs['designed_at']).year==2026 and type(self.costs['scale']) is int
            and self.costs['scale'] in (1,2),'ambiguous reviewed-source experimental model')
        first=date.fromisoformat(self.data['first_session']);last=date.fromisoformat(self.data['last_session'])
        require(date(2021,1,1)<=first<=last<=date(2026,10,9),'experimental history outside declared range')
        require(all(0<len(p['symbols'])<=10 and len(set(p['symbols']))==len(p['symbols'])
            and not set(p['symbols'])&{'QCOM','META','FB'} for p in self.data['opening_policies']),
            'excluded or ambiguous experimental opening universe')

    def standard(self,symbol,clock):
        if symbol in ('QCOM','META','FB'):return False
        if any(x['symbol']==symbol and at(x['at'])<=clock for x in self.data['unsupported_actions']):return False
        terms=[x for x in self.data['standard_terms'] if x['symbol']==symbol
            and at(x['from'])<=clock<=at(x['through']) and at(x['known_at'])<=clock]
        return len(terms)==1 and terms[0]['shares']==terms[0]['multiplier']==100


class ConditionalFitModel(PilotModel):
    """Describe actual paths under a model; never declare historical terms."""
    def __init__(self,packet):
        self.packet=packet;self.data=packet['sampled_data'];self.costs=packet['modeled_costs']
        self.call_rule=call_eligibility(packet)
        self.entry_risk_rule=entry_rule(packet)
        path_review=packet.get('contract_assumption')==CONDITIONAL_PATH
        self.symbols=PATH_SYMBOLS if path_review else FIT_SYMBOLS
        first=date.fromisoformat(self.data['first_session']);last=date.fromisoformat(self.data['last_session'])
        scope=(date(2021,1,4)<=first<=last<=date(2026,10,9)) if path_review else (
            first==date(2021,1,4) and last==date(2022,12,30))
        require(packet['schema']=='AutoWheelSimpleResearchPacketV1'
            and packet.get('contract_assumption') in (CONDITIONAL,CONDITIONAL_PATH)
            and self.data['schema']=='AutoWheelSimpleResearchDataV1'
            and self.data['admission_status']=='conditional_observed_prices_pending_contract_action_review'
            and scope
            and self.data['protocol_sha256']==self.costs['plan_sha256']
            and re.fullmatch('[0-9a-f]{64}',self.costs['plan_sha256'])
            and self.data['tariffs']==self.data['standard_terms']==self.data['unqualified_inputs']==[]
            and at(self.costs['designed_at']).year==2026 and type(self.costs['scale']) is int
            and self.costs['scale'] in (1,2),'ambiguous conditional first-fit model')
        minimum_symbols=0 if path_review and first>=date(2023,1,1) else 1
        require(all(minimum_symbols<=len(p['symbols'])<=10 and len(set(p['symbols']))==len(p['symbols'])
            and set(p['symbols'])<=self.symbols for p in self.data['opening_policies']),
            'conditional fit outside frozen cohort')

    def standard(self,symbol,clock):
        return (symbol in self.symbols and self.data['first_session']<=str(clock.astimezone(NY).date())<=self.data['last_session']
            and not any(x['symbol']==symbol and at(x['at'])<=clock for x in self.data['unsupported_actions']))


class TargetedModel(ConditionalFitModel):
    """Independently reconstruct the two frozen, causal offline ablations."""
    def __init__(self, packet, records, result):
        super().__init__(packet)
        self.controls = packet['targeted_controls']
        require(self.controls in ({'max_active_symbols':2},{'loss_reentry_calendar_days':31},
                {'growth_call_target':True},{'loss_reentry_calendar_days':31,'loss_reentry_december_only':True},
                {'reserve_highest_ST_tax':True},
                {'reserve_highest_ST_tax':True,'account_200k_no_symbol_allocation_cap':True}),
                'unknown or combined targeted control')
        required_mode = 'balanced' if 'max_active_symbols' in self.controls or 'growth_call_target' in self.controls else 'aggressive'
        require(all(p['profile']['mode'] in ('conservative','balanced','aggressive') if self.controls.get('reserve_highest_ST_tax') else p['profile']['mode']==required_mode for p in self.data['opening_policies']),
                'targeted control used outside its frozen mode')
        self.capital_limits=(20_000_000,20_000_000) if self.controls.get('account_200k_no_symbol_allocation_cap') else (8_000_000,2_666_666)
        self.tax_before={};self.tax_bound=0
        if self.controls.get('reserve_highest_ST_tax'):
            from tax_analytics import tax_ledger
            raw=tax_ledger(records,result,wash=False)
            events={e['index']:e for e in raw['events']}
            for index in range(len(records)):
                self.tax_before[index]=self.tax_bound
                event=events.get(index)
                income=event['positive_capital_cents']+event['dividend_cents'] if event else 0
                if income>0:self.tax_bound+=(income*5410+9999)//10000+1
            require(result.get('reserved_highest_ST_tax_cents')==self.tax_bound,'Go causal tax reserve differs from independent ledger')
        costs = {};self.loss_events = {}
        for record in records:
            e=record['effect'];s=e.get('symbol');a=e['action']
            if a=='buy_stock':costs[s]=100*e['price_cents']+e['fee_cents']
            elif a=='resolve' and e['outcome']=='assigned':costs[s]=100*e['contract']['strike_cents']+e['fee_cents']
            elif a=='resolve' and e['outcome']=='called_away':
                cost=costs.pop(s)
                if 100*e['contract']['strike_cents']-e['fee_cents']<cost:
                    clock=at(e['at']);day=clock.astimezone(NY).date()
                    if not self.controls.get('loss_reentry_december_only') or day.month==12:
                        self.loss_events.setdefault(s,[]).append((clock,day+timedelta(days=31)))

    def selection_mode(self,mode,kind):
        return 'conservative' if self.controls.get('growth_call_target') and kind=='call' else super().selection_mode(mode,kind)

    def targeted_reentry_blocked(self,symbol,clock):
        return 'loss_reentry_calendar_days' in self.controls and any(
            sold<clock and clock.astimezone(NY).date()<through for sold,through in self.loss_events.get(symbol,[]))

    def targeted_capacity_blocked(self,positions,symbol):
        return 'max_active_symbols' in self.controls and symbol not in positions and len(positions)>=2

    def targeted_tax_funded(self,settled,collateral,debit,addition,action,contract,index):
        if not self.controls.get('reserve_highest_ST_tax'):return True
        reserved=collateral+(addition if action=='sell_put' else 0)
        if action=='close_short' and contract['kind']=='put':reserved-=100*contract['strike_cents']
        required=debit+self.tax_before[index]+(1_680_944 if action in ('sell_put','buy_stock') else 0)
        return settled-reserved>=required


def analyze(packet,records,result,diagnostics,journal_path,journal_sha,result_path,result_sha,*,expected_sensitivity='primary'):
    pilot=packet.get('contract_assumption')==ASSUMPTION
    conditional=packet.get('contract_assumption') in (CONDITIONAL,CONDITIONAL_PATH)
    model=TargetedModel(packet,records,result) if packet.get('targeted_controls') else PilotModel(packet) if pilot else ConditionalFitModel(packet) if conditional else ReviewedModel(packet);data=packet['sampled_data']
    require(result.get('targeted_controls')==packet.get('targeted_controls'),'targeted control result binding differs')
    rule=call_eligibility(packet)
    label='simple-modeled-costs-stock-cost-call-floor-v1' if rule else 'simple-modeled-costs-growth-calls-v1'
    risk_rule=entry_rule(packet)
    if risk_rule:label+='-entry-minus40-budget-v1'
    require(result['research_model']==label and result.get('call_eligibility','')==rule
        and result.get('contract_assumption','')==packet.get('contract_assumption','')
        and result.get('entry_risk_rule','')==risk_rule
        and result['modeled_costs']==packet['modeled_costs'] and result['status']=='complete'
        and result['sensitivity']==expected_sensitivity and (not pilot or expected_sensitivity=='primary'),
        'result changed the declared experimental model')
    financial=verify(journal_path,journal_sha,result_path,result_sha)
    require(financial['qualified'],'pilot money/PnL reconciliation failed')
    trace={}
    resources=availability(records,result['checkpoint_state'],data['exchange_days'],data['clearing_days'],
        scenario=expected_sensitivity,dividend_terms=data['dividends'],trace=trace,
        capital_limits=getattr(model,'capital_limits',(8_000_000,2_666_666)))
    risk_trace=prepare_trace(records,trace,result) if risk_rule else None
    source=effects(records,data,expected_sensitivity,pending=result.get('pending_attempts'),source_model=model)
    decisions=reconstruct(records,result,data,diagnostics,expected_sensitivity,trace,source_model=model)
    points=[dict(ref=v['effect']['ref'],at=v['effect']['at'],nav_cents=v['mark']['NAV_cents'])
        for v in records if v['effect']['action']=='mark']
    metrics=series_metrics(points,data['exchange_days'],data['first_session'],data['last_session'],scenario=expected_sensitivity)
    metrics.update(inventory_metrics(records));metrics['financial_components']=financial['metrics']
    require((not pilot or metrics['sessions']==65 and len(points)==195)
        and metrics['sessions']==result['sessions_marked']
        and metrics['net_profit_cents']==financial['metrics']['net_profit_cents'],'account schedule or financial result differs')
    sessions,_,_=session_calendar(data['exchange_days'])
    metrics['cash_exposure']=cash_exposure(records,sessions[data['first_session']][0].isoformat(),result['last_near_close_at'])
    report=dict(schema='AutoWheelSimpleExploratoryPilotVerificationV1' if pilot else 'AutoWheelSimpleReviewedAccountVerificationV1',
        status='complete_arithmetic_under_exploratory_assumptions' if pilot or conditional else 'complete_arithmetic_pending_external_source_admission',metrics=metrics,
        money_and_PnL=financial,availability=resources,source_effects=source,scheduled_decisions=decisions,
        contract_and_action_history_qualified=False,fees_are_actual_historical_broker_charges=False,
        strategy_selection_or_deployment_qualified=False,contract_assumption=packet.get('contract_assumption',''))
    if rule:report['call_eligibility']=rule
    if risk_rule:report.update(entry_risk_rule=risk_rule,entry_risk_trace=risk_trace)
    return report
