"""One synthetic money-journal safety check, not a historical account replay."""
import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

from rd1_financial_verify import verify


class RD1FinancialBoundary(unittest.TestCase):
    def test_liability_reconciliation_and_incomplete_path(self):
        source='a'*64;profile=dict(mode='balanced',exit='hold')
        contract=dict(id='fixture-put',symbol='AAPL',expiry='2021-01-29',kind='put',strike_cents=10000)
        opened='2021-01-04T15:30:00-05:00';marked='2021-01-04T15:59:00-05:00'
        sale=dict(contract=contract,opening_ref='fixture-open',opening_net_credit_cents=9999,
                  opening_fee_cents=1,opened_at=opened,close_cost_cents=0)
        records=[dict(sequence=1,effect=dict(ref='fixture-open',action='sell_put',symbol='AAPL',
            at=opened,profile=profile,contract=contract,price_cents=100,fee_cents=1,
            source_packet_sha256=source),cash_delta_cents=9999,cash_after_cents=8414716,
            receivable_delta_cents=0,fees_after_cents=1),
            dict(sequence=2,effect=dict(ref='NAV-2021-01-04',action='mark',symbol='',at=marked,
            price_cents=0,fee_cents=0,source_packet_sha256=source),cash_delta_cents=0,
            cash_after_cents=8414716,receivable_delta_cents=0,fees_after_cents=1,
            mark=dict(NAV_cents=8406716,stock_books=None,
                      option_books={'fixture-put':dict(bid_cents=81,ask_cents=80)}))]
        state=dict(seed_cents=8404717,economic_cash_cents=8414716,fees_cents=1,
            realized_stock_pnl_cents=0,at=marked,sequence=2,
            positions={'AAPL':dict(opening_profile=profile,shares=0,stock_acquisition_cents=0,short=sale)},
            option_sales={'fixture-open':sale},dividends={},
            used_refs={'fixture-open':True,'NAV-2021-01-04':True})
        result=dict(status='complete',reason='synthetic money fixture',last_near_close_NAV_cents=8406716,
            last_near_close_at=marked,net_profit_cents=1999,checkpoint_state=state,
            diagnostic_counts={},pending_attempts=None,independent_financial_verification_complete=False,
            sessions_marked=1)
        with tempfile.TemporaryDirectory(prefix='rd1-verifier-unit-') as folder:
            root=Path(folder);serial=0
            def check(rows,value):
                nonlocal serial
                serial+=1;journal=root/f'{serial}.jsonl';end=root/f'{serial}.json'
                journal.write_bytes(b''.join((json.dumps(x)+'\n').encode() for x in rows))
                end.write_text(json.dumps(value))
                return verify(journal,hashlib.sha256(journal.read_bytes()).hexdigest(),end,
                              hashlib.sha256(end.read_bytes()).hexdigest())
            valid=check(records,result)
            self.assertTrue(valid['qualified'],valid)
            self.assertEqual(valid['metrics']['net_profit_cents'],1999)
            self.assertIsNone(valid['metrics']['credit_capture']['ratio'])
            intent=dict(ref='policy-2021-01-04-MSFT',action='sell_put',symbol='MSFT',
                at='2021-01-05T15:30:00-05:00',signal_at='2021-01-04T15:29:00-05:00',
                profile=profile,contract=dict(contract,id='fixture-MSFT-put',symbol='MSFT'),
                price_cents=0,fee_cents=0,source_packet_sha256=source)
            censored=copy.deepcopy(result);censored.update(sensitivity='execution_next_session',pending_attempts=[intent])
            future=check(records,censored)
            self.assertTrue(future['qualified'],future)
            self.assertEqual(future['metrics'],valid['metrics'])
            self.assertEqual(future['censored_pending_attempts_count'],1)
            self.assertFalse(future['pending_calendar_and_signal_qualification_performed'])
            for label in ('primary','priced','fee','already_used','duplicate','not_future','lifecycle','unowned_close'):
                wrong=copy.deepcopy(censored);p=wrong['pending_attempts'][0]
                if label=='primary':wrong['sensitivity']='primary'
                elif label=='priced':p['price_cents']=1
                elif label=='fee':p['fee_cents']=1
                elif label=='already_used':p['ref']='fixture-open'
                elif label=='duplicate':wrong['pending_attempts'].append(copy.deepcopy(p))
                elif label=='not_future':p['at']=marked
                elif label=='lifecycle':p['outcome']='assigned'
                else:p['action']='close_short'
                failed=check(records,wrong)
                self.assertFalse(failed['qualified'],(label,failed))
                self.assertIsNone(failed['metrics'],(label,failed))
            for label,rows,end in [('missing_held_ask',copy.deepcopy(records),copy.deepcopy(result)),
                                   ('cash_tamper',copy.deepcopy(records),copy.deepcopy(result)),
                                   ('incomplete_path',copy.deepcopy(records),copy.deepcopy(result)),
                                   ('missing_near_close',copy.deepcopy(records),copy.deepcopy(result))]:
                if label=='missing_held_ask':rows[-1]['mark']['option_books']={}
                elif label=='cash_tamper':rows[0]['cash_after_cents']+=1
                elif label=='incomplete_path':end['status']='incomplete';end['net_profit_cents']=None
                else:rows[-1]['effect']['ref']='arbitrary-last-mark'
                rejected=check(rows,end)
                self.assertFalse(rejected['qualified'],(label,rejected))
                self.assertIsNone(rejected['metrics'],(label,rejected))


if __name__=='__main__':unittest.main()
