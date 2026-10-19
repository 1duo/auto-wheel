"""Corroborate published dividend fields; do not infer missing cashflows."""
from datetime import datetime
from decimal import Decimal
from html.parser import HTMLParser
import json
import re
import sys
from zoneinfo import ZoneInfo

from rd1_dispatch import checked
from rd1_inputs import decode
from rd1_storage import require,root,write
from simple_research_capture import budget_check

NY=ZoneInfo('America/New_York')


class Tables(HTMLParser):
    def __init__(self):
        super().__init__();self.rows=[];self.row=None;self.cell=None

    def handle_starttag(self,tag,attrs):
        if tag=='tr':self.row=[]
        if tag in ('td','th'):self.cell=[]

    def handle_data(self,data):
        if self.cell is not None:self.cell.append(data)

    def handle_endtag(self,tag):
        if tag in ('td','th') and self.cell is not None:
            if self.row is not None:self.row.append(' '.join(' '.join(self.cell).split()))
            self.cell=None
        if tag=='tr' and self.row:self.rows.append(self.row);self.row=None


def payment_rows(raw,symbol,first='2021-01-01',last='2022-12-31'):
    text=raw.decode('utf-8');require(symbol+' Dividend History' in text,'payment page symbol differs')
    parser=Tables();parser.feed(text);rows={}
    for cells in parser.rows:
        if len(cells)<3 or not re.fullmatch(r'\d{4}-\d{2}-\d{2}',cells[0]):continue
        if not first<=cells[0]<=last:continue
        require(re.fullmatch(r'\d{4}-\d{2}-\d{2}',cells[1])
            and re.fullmatch(r'\$[0-9]+(?:\.[0-9]+)?',cells[2])
            and not any(x in ' '.join(cells).lower() for x in ('estimated','unconfirmed')),
            'unconfirmed or ambiguous historical dividend row')
        amount=Decimal(cells[2][1:])*10000
        require(amount==int(amount) and amount>0 and cells[0] not in rows,
            'inexact/repeated dividend cashflow: '+symbol+' '+cells[0])
        rows[cells[0]]=dict(pay_date=cells[1],amount_for_100_shares_cents=int(amount))
    return rows


def run(spec):
    r=root('rd1-261017-0748');budget_check(r,spec['observation'],120)
    histories={};census={};sources=[]
    def original(directory,index):
        p=r/directory/f'{index:03}.receipt.json';receipt=json.loads(p.read_bytes())
        require(receipt['http_status']==200 and receipt['complete'],'public dividend source incomplete')
        ref=receipt['raw'];raw=checked(ref['file'],ref['sha256'],8<<20)
        require(len(raw)==ref['bytes']==receipt['bytes'],'public dividend size differs')
        sources.append(dict(url=receipt['url'],**ref));return raw,ref
    # Only these first ten payment pages belong to the 2021–2022 source cohort.
    # Later entrants' old split-adjusted dividends cannot affect an unowned lot.
    names=['AAPL','MSFT','BA','AMD','V','INTC','PYPL','SQ','MRNA','MU']
    for offset,symbol in enumerate(names):
        raw,ref=original(f'pilot-simple-dividend-payment-b{offset//8+1}-v1',offset%8)
        histories[symbol]=(payment_rows(raw,'XYZ' if symbol=='SQ' else symbol),ref)
    for symbol,index in (('XOM',5),('JPM',7)):
        raw,ref=original('pilot-simple-INTC-cashflow-checks-v1',index)
        histories[symbol]=(payment_rows(raw,symbol),ref)
    names=['AAPL','MSFT','BA','AMD','JPM','V','INTC','PYPL','SQ','XOM','MRNA','MU']
    stock=json.loads(checked(r/'long-stock-stage-v3/stock-samples-2022.json',
        'e80b1379893e6e3f44f39944023be88417e99df620a0d8935a643c3ddd575066',4<<20))
    require(set(names)==set(stock['symbols']),'dividend cohort differs from possible held names')
    for offset,symbol in enumerate(names):
        raw,ref=original(f'pilot-simple-dividend-census-b{offset//8+1}-v1',offset%8)
        chart=decode(raw)['chart'];require(chart['error'] is None and len(chart['result'])==1,'ambiguous public dividend census')
        body=chart['result'][0];meta=body['meta']
        require(meta['symbol']==('XYZ' if symbol=='SQ' else symbol) and meta['currency']=='USD'
            and meta['instrumentType']=='EQUITY','dividend census identity/units differ')
        events={}
        for key,event in body.get('events',{}).get('dividends',{}).items():
            require(str(event['date'])==key,'dividend event date key differs')
            day=str(datetime.fromtimestamp(event['date'],NY).date())
            if not '2021-01-01'<=day<='2022-12-31':continue
            amount=Decimal(event['amount'])*10000
            require(amount==int(amount) and amount>0 and day not in events,'inexact/repeated reported dividend')
            events[day]=int(amount)
        census[symbol]=(events,ref)
    results=[];disagreements=[];cashflows=[]
    for symbol in names:
        payments,payment_ref=histories[symbol];events,event_ref=census[symbol]
        missing=sorted(set(payments)^set(events))
        if missing:disagreements.append(dict(symbol=symbol,reason='ex_date_set_differs',dates=missing))
        for day in sorted(set(payments)&set(events)):
            row=payments[day];amount=row['amount_for_100_shares_cents']
            if amount!=events[day]:disagreements.append(dict(symbol=symbol,ex_date=day,
                reason='reported_amounts_differ',payment_history_amount_cents=amount,census_amount_cents=events[day]))
            ex=datetime.fromisoformat(day+'T09:30:00').replace(tzinfo=NY)
            pay=datetime.fromisoformat(row['pay_date']+'T09:30:00').replace(tzinfo=NY)
            require(ex<=pay,'payment precedes ex-date')
            cashflows.append(dict(id=symbol+'-'+day.replace('-',''),symbol=symbol,ex_at=ex.isoformat(),
                known_at=ex.isoformat(),amount_for_100_shares_cents=amount,pay_at=pay.isoformat(),
                available_at=pay.isoformat(),payment_history=payment_ref,ex_date_corroboration=event_ref))
        results.append(dict(symbol=symbol,payment_events=len(payments),census_events=len(events),
            exact_ex_date_sets_match=not missing,zero_events_are_observed_provider_census_not_universal_absence_proof=not payments))
    out=r/'dividend-fields-2021-2022-v1';out.mkdir(mode=0o700)
    refs=[write(out/'cashflows.json',cashflows),write(out/'disagreements.json',disagreements),write(out/'sources.json',sources)]
    report=dict(schema='AutoWheelSimplePublishedDividendFieldsV1',first='2021-01-01',last='2022-12-31',
        symbols=results,paired_cashflows=len(cashflows),disagreements=len(disagreements),
        reviewed_issuer_corroboration_pending=True,full_source_admission_performed=False,financial_cases=0,
        information_model='terms_revealed_at_ex_date_open_no_claim_of_announcement_timestamp',
        cash_availability_model='published_pay_date_at_0930_New_York_not_observed_broker_receipt',artifacts=refs)
    print(json.dumps(dict(summary=write(out/'summary.json',report),report=report)))


if __name__=='__main__':run(json.load(sys.stdin))
