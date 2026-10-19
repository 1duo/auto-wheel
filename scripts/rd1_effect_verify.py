"""Independently bind executed RD1 effects to the admitted packet.

This checks sampled sides, frozen choices, tariff arithmetic, mark books and
exercise references. It does not establish original-source admission, prove
that every eligible attempt was made, or qualify the complete strategy.
"""
from datetime import date
from fractions import Fraction

from rd1_financial_verify import (_check_hash, _loads, _parse_journal,
    _read_bounded, MAX_JOURNAL_BYTES, MAX_RESULT_BYTES, verify)
from rd1_metrics import NY, SCENARIOS, at, integer, session_calendar
from rd1_storage import PROTOCOL_SHA256, require


def sale_fee(rule,units,gross):
    """One complete order, with each disclosed fee component rounded once."""
    flat=integer(rule['flat_fee_cents'],0,10000)
    numerator=integer(rule['notional_numerator'],0,1_000_000)
    denominator=integer(rule['notional_denominator'],1,1_000_000_000_000)
    share_num=integer(rule['share_numerator'],0,1_000_000)
    share_den=integer(rule['share_denominator'],1,1_000_000_000_000)
    cap=integer(rule['share_cap_cents'],0,10000)
    waiver=integer(rule.get('notional_waiver_cents',0))
    rounding=rule.get('share_rounding','');subcent=rule.get('waive_subcent_share',False)
    require(rounding in ('','nearest') and type(subcent) is bool
            and (not subcent or rounding=='nearest'), 'ambiguous fee rounding')
    integer(units,1,100);integer(gross,1)
    ceil=lambda value,divisor:(value+divisor-1)//divisor
    notional=0 if gross<=waiver else ceil(gross*numerator,denominator)
    value=units*share_num;share=ceil(value,share_den)
    if rounding=='nearest':
        if subcent and value<share_den:share=0
        else:
            quotient,remainder=divmod(value,share_den)
            require(2*remainder!=share_den, 'unqualified nearest-cent half-cent fee tie')
            share=quotient+int(2*remainder>share_den)
    if cap:share=min(share,cap)
    return integer(flat+notional+share)


def opening_fee(tariff,price):
    fees=tariff['dated_fees'];flat=integer(fees['option_open_cents'],0,10000)
    rule=fees.get('option_sale')
    if rule is None:return flat
    require(flat==0, 'ambiguous flat and sale option fee')
    return sale_fee(rule,1,100*integer(price,1,1_000_000_000))


def entry_book(book):
    bid=integer(book['bid_cents'],1,1_000_000_000)
    ask=integer(book['ask_cents'],1,1_000_000_000)
    require(bid<=ask and Fraction(2*(ask-bid),ask+bid)<=Fraction(3,20),
            'entry requires a noncrossed book with spread at most 15 percent')


def fill_price(book,buy,scenario):
    bid=integer(book['bid_cents'],0,1_000_000_000)
    ask=integer(book['ask_cents'],1,1_000_000_000)
    price=ask if buy else bid
    if scenario=='extra_half_spread_cost':
        require(ask>=bid, 'undefined half-spread shock on crossed book')
        extra=(ask-bid+1)//2;price+=extra if buy else -extra
    return integer(price,1,1_000_000_000)


def contract_choice(menu,day,symbol,kind,stock,mode):
    """Independent exact observed-menu selection, without quote replacement."""
    targets={('conservative','put'):Fraction(9,10),('balanced','put'):Fraction(19,20),
        ('conservative','call'):Fraction(21,20),('balanced','call'):Fraction(103,100),
        ('aggressive','call'):Fraction(101,100)}
    require((mode,kind) in targets, 'unknown fixed-moneyness target')
    midpoint=Fraction(stock['bid_cents']+stock['ask_cents'],2);eligible=[];seen=set()
    for item in menu:
        strike=integer(item['strike_cents'],1,1_000_000_000);expiry=date.fromisoformat(item['expiry'])
        require(item['id'] not in seen and item['kind'] in ('put','call')
                and item['id']==item['symbol'].ljust(6)+expiry.strftime('%y%m%d')
                    +item['kind'][0].upper()+str(10*strike).zfill(8), 'ambiguous exact menu contract identity')
        seen.add(item['id']);dte=(expiry-date.fromisoformat(day)).days
        if (item['symbol']==symbol and item['kind']==kind and 21<=dte<=45
                and Fraction(4,5)*midpoint<=strike<=Fraction(6,5)*midpoint):eligible.append(item)
    require(eligible, 'filled entry has no observed eligible contract')
    expiration=min({c['expiry'] for c in eligible},key=lambda value:
        (abs((date.fromisoformat(value)-date.fromisoformat(day)).days-30),value))
    target=targets[mode,kind]*midpoint
    eligible=[c for c in eligible if c['expiry']==expiration and
              (c['strike_cents']<=target if kind=='put' else c['strike_cents']>=target)]
    require(eligible, 'filled entry has no observed strike at target')
    return min(eligible,key=lambda c:((-c['strike_cents'] if kind=='put' else c['strike_cents']),c['id']))


def effects(records,packet,scenario,*,pending=None,source_model=None):
    require(scenario in SCENARIOS, 'unknown or combined effect scenario')
    samples={at(row['at']):row for row in packet['observations']}
    tariffs={row['dated_fees']['date']:row for row in packet['tariffs']}
    refs={(row['symbol'],row['day']):row for row in packet['regular_session_references']}
    dividends={row['id']:row for row in packet['dividends']}
    sessions,_,_=session_calendar(packet['exchange_days'])
    require(len(samples)==len(packet['observations']) and len(tariffs)==len(packet['tariffs'])
            and len(refs)==len(packet['regular_session_references']) and len(dividends)==len(packet['dividends']),
            'duplicate source effect coordinate')
    positions={};entitlements={};counts=dict(marks=0,policy_fills=0,resolutions=0,dividend_events=0)
    def tariff(clock):
        if source_model is not None:return source_model.tariff(clock)
        value=tariffs[str(clock.astimezone(NY).date())]
        require(at(value['dated_fees']['known_at'])<=clock, 'tariff not yet known')
        return value
    def standard(symbol,clock):
        require(not any(row['symbol']==symbol and at(row['at'])<=clock
                        for row in packet['unsupported_actions']), 'held or entering unsupported action')
        if source_model is not None:
            require(source_model.standard(symbol,clock), 'explicit model does not cover this security/date')
            return
        terms=[row for row in packet['standard_terms'] if row['symbol']==symbol
               and at(row['from'])<=clock<=at(row['through']) and at(row['known_at'])<=clock]
        require(len(terms)==1 and terms[0]['shares']==terms[0]['multiplier']==100,
                'held or entering standard terms unknown or ambiguous')
    def policy(clock,symbol):
        day=str(clock.astimezone(NY).date())
        values=[p for p in packet['opening_policies'] if p['from']<=day<=p['through']]
        require(len(values)==1 and symbol in values[0]['symbols'], 'opening outside frozen universe')
        profile=values[0]['profile']
        require(profile['mode'] in ('conservative','balanced','aggressive')
                and profile['exit'] in ('hold','tp50','tp75','tp90','puttp50','puttp75','puttp90'), 'unknown opening profile')
        return profile
    def signal_decision(e,p):
        symbol=e['symbol'];action=e['action'];c=e['contract'];signal=at(e['signal_at'])
        before=samples[signal];profile=p['profile'] if p else policy(signal,symbol)
        require(e['profile']==profile, 'position opening rules changed')
        standard(symbol,signal)
        if action=='close_short':
            require(p and p['short']==c and profile['exit'] in ('tp50','tp75','tp90','puttp50','puttp75','puttp90'), 'managed close has no matching managed short')
            ask=fill_price(before['option_books'][c['id']],True,scenario)
            require(not profile['exit'].startswith('puttp') or c['kind']=='put','put-only TP attempted to close a call')
            target=int(profile['exit'].removeprefix('put')[2:])
            close=integer(tariff(signal)['dated_fees']['option_close_cents'],0,10000)
            require(100*(p['net']-100*ask-close)>=target*p['net'], 'managed close lacks the signal-time net-credit trigger')
        else:
            require((action=='buy_stock' and p is None and profile['mode']=='aggressive')
                    or (action=='sell_put' and p is None and profile['mode']!='aggressive')
                    or (action=='sell_call' and p and p['shares']==100 and not p['short']),
                    'entry action differs from the frozen mode/lifecycle')
            kind='put' if action=='sell_put' else 'call';stock=before['stock_books'][symbol];entry_book(stock)
            mode=source_model.selection_mode(profile['mode'],kind) if source_model is not None else profile['mode']
            require(c==contract_choice(before['observed_menus'][symbol],str(signal.astimezone(NY).date()),symbol,kind,stock,mode),
                    'fill replaces the frozen signal-time contract choice')
            entry_book(before['option_books'][c['id']])
            signal_price=fill_price(before['option_books'][c['id']],False,scenario)
            require(100*signal_price>opening_fee(tariff(signal),signal_price), 'nonpositive signal option net credit')
        return profile
    for record in records:
        e=record['effect'];action=e['action'];clock=at(e['at']);symbol=e.get('symbol');p=positions.get(symbol)
        for held in positions:standard(held,clock)
        if action=='mark':
            obs=samples[clock];marked=record['mark']
            stocks={name:obs['stock_books'][name] for name,value in positions.items() if value['shares']}
            options={value['short']['id']:obs['option_books'][value['short']['id']]
                     for value in positions.values() if value['short']}
            require(e['source_packet_sha256']==obs['source_packet_sha256']
                    and marked['stock_books']==stocks and marked['option_books']==options,
                    'journal mark books differ from held sampled packet sides')
            counts['marks']+=1;continue
        if action in ('sell_put','sell_call','buy_stock','close_short'):
            signal=at(e['signal_at']);later=samples[clock];fees=tariff(clock)
            require(e['source_packet_sha256']==later['source_packet_sha256'], 'fill source differs from actual execution sample')
            profile=signal_decision(e,p)
            c=e['contract']
            if action=='close_short':
                book=later['option_books'][c['id']];fee=fees['dated_fees']['option_close_cents']
            else:
                standard(symbol,clock)
                entry_book(later['stock_books'][symbol])
                book=later['stock_books'][symbol] if action=='buy_stock' else later['option_books'][c['id']]
                entry_book(book)
                if scenario=='execution_next_session' and action in ('buy_stock','sell_put'):
                    require(policy(clock,symbol)==profile, 'delayed execution crosses an opening-policy change')
                fee=fees['stock_buy_fee_cents'] if action=='buy_stock' else opening_fee(fees,fill_price(book,False,scenario))
            price=fill_price(book,action in ('buy_stock','close_short'),scenario)
            require(e['price_cents']==price and e['fee_cents']==integer(fee,0,10000),
                    'effect price or fee differs from actual side/disclosed dated tariff')
            if action in ('sell_put','sell_call'):
                require(100*price>fee, 'nonpositive executed option net credit')
                if not p:p=dict(shares=0,profile=profile,short=None);positions[symbol]=p
                p.update(short=c,net=100*price-fee)
            elif action=='buy_stock':positions[symbol]=dict(shares=100,profile=profile,short=None)
            else:
                p['short']=None
                if not p['shares']:del positions[symbol]
            counts['policy_fills']+=1
        elif action=='resolve':
            require(p and p['short']==e['contract'], 'resolution has no matching held contract')
            c=e['contract'];ref=refs[symbol,str(clock.astimezone(NY).date())]
            opened,closed=sessions[str(clock.astimezone(NY).date())]
            require(e['source_packet_sha256']==ref['source_packet_sha256']
                    and opened<=at(ref['at'])<=clock==closed,
                    'resolution differs from declared contemporaneous regular-session reference')
            difference=integer(ref['price_micros'],1,10_000_000_000_000)-10000*c['strike_cents']
            assigned=(difference<=-10000 if c['kind']=='put' else difference>=10000)
            pin=1000*abs(difference)<=ref['price_micros']
            if scenario=='pin_exercise' and pin:assigned=True
            if scenario=='pin_no_exercise' and pin:assigned=False
            if scenario=='daily_ITM_assignment':assigned=difference<0 if c['kind']=='put' else difference>0
            outcome=('assigned' if c['kind']=='put' else 'called_away') if assigned else 'expired'
            require(e['outcome']==outcome, 'resolution contradicts exact primary/ITM/pin rule')
            fee=0
            if assigned:
                fees=tariff(clock);fee=fees['dated_fees']['physical_exercise_cents']
                if outcome=='called_away':fee+=sale_fee(fees['physical_stock_sale'],100,100*c['strike_cents'])
            require(e['fee_cents']==integer(fee,0,10000), 'physical delivery fee differs from dated tariff')
            p['short']=None
            if outcome=='assigned':p['shares']=100
            if outcome=='called_away' or not p['shares']:del positions[symbol]
            counts['resolutions']+=1
        elif action in ('dividend','pay_dividend'):
            term=dividends[e['dividend_id']]
            require(symbol==term['symbol'] and e['source_packet_sha256']==term['source_sha256']
                    and e['price_cents']==term['amount_for_100_shares_cents'], 'dividend source or amount differs')
            if action=='dividend':
                require(p and p['shares']==100 and clock==at(term['ex_at'])
                        and at(term['known_at'])<=clock and e['dividend_id'] not in entitlements,
                        'dividend entitlement differs from owned shares or causal ex-date')
                entitlements[e['dividend_id']]=term
            else:require(e['dividend_id'] in entitlements and clock==at(term['pay_at']), 'unknown or mistimed dividend payment')
            counts['dividend_events']+=1
        else:raise ValueError('unsupported source-effect action')
    require(pending is None or type(pending) is list and len(pending)<=13, 'unbounded pending source intents')
    require(not pending or scenario=='execution_next_session', 'pending source intent outside the next-session model')
    for intent in pending or []:
        before=samples[at(intent['signal_at'])]
        require(intent['source_packet_sha256']==before['source_packet_sha256']
                and intent['price_cents']==intent['fee_cents']==0,
                'censored intent changes its signal source or invents a future financial effect')
        signal_decision(intent,positions.get(intent['symbol']))
    counts['censored_pending_attempts']=len(pending or [])
    return dict(counts=counts,executed_policy_choices_and_sides_verified=True,
        supplied_censored_intent_signals_verified=True,future_pending_price_or_fill_assumed=False,
        quoted_prices_replaced=False,tariff_rates_originally_qualified=False,
        all_eligible_attempts_or_abstentions_independently_reconstructed=False)


def analyze(journal_path,journal_sha256,result_path,result_sha256,packet_path,packet_sha256,*,expected_sensitivity='primary'):
    financial=verify(journal_path,journal_sha256,result_path,result_sha256)
    report=dict(schema='AutoWheelRD1EffectSourceArithmeticV1',status='insufficient',checks=None,
        protocol_sha256=PROTOCOL_SHA256,source_admission_performed=False,
        full_policy_qualification_performed=False,historical_performance_qualified=False)
    if not financial['qualified']:return report
    try:
        def read(path,pin,limit):
            raw=_read_bounded(path,limit,'source-effect input');_check_hash(raw,pin,'source-effect input');return raw
        packet=_loads(read(packet_path,packet_sha256,256<<20))
        require(packet['protocol_sha256']==PROTOCOL_SHA256 and packet['admission_status']=='admitted_under_RD1'
                and packet['unqualified_inputs']==[], 'unadmitted source-effect packet')
        result=_loads(read(result_path,result_sha256,MAX_RESULT_BYTES))
        require(expected_sensitivity in SCENARIOS and result.get('sensitivity')==expected_sensitivity,
                'source-effect scenario differs from charged case')
        checks=effects(_parse_journal(read(journal_path,journal_sha256,MAX_JOURNAL_BYTES)),packet,expected_sensitivity,
                       pending=result.get('pending_attempts'))
        report.update(status='arithmetic_complete_pending_external_admission',checks=checks,
                      packet_sha256=packet_sha256,journal_sha256=journal_sha256,result_sha256=result_sha256)
    except Exception as error:report.update(reason='source_effect_rejected:'+type(error).__name__+':'+str(error)[:240])
    return report
