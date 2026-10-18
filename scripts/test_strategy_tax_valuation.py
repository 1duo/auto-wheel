import unittest
import test_strategy_tax_analytics as fixtures
from strategy_tax import SEED,analyze_records
from strategy_tax_valuation import annual_liquidation_values


class TaxValuation(unittest.TestCase):
    def test_realizing_prior_gain_is_not_mistaken_for_new_economic_loss(self):
        f=fixtures.TaxAccountingTests()
        call=dict(id='c',kind='call',strike_cents=200,expiry='2025-02-01')
        rows,result=f.records([(f.effect('buy_stock','2024-01-01',price_cents=100,fee_cents=100),-10100),
            (f.effect('sell_call','2025-01-01',contract=call,price_cents=10,fee_cents=65),935),
            (f.effect('resolve','2025-02-01',contract=call,outcome='called_away',fee_cents=100),19900)])
        rows.insert(1,dict(effect=dict(action='mark',ref='NAV-2024-12-31',at='2024-12-31T15:59:00-05:00'),
            cash_after_cents=SEED-10100,cash_delta_cents=0,receivable_delta_cents=0,
            mark=dict(NAV_cents=SEED+9900,stock_books={'X':dict(bid_cents=200)},option_books={})))
        result['status']='complete'
        old=analyze_records(rows,result)['scenarios']['federal_CA_carry']
        self.assertLess(next(y['return_pct'] for y in old['years'] if y['year']=='2025'),0)
        values=annual_liquidation_values(rows,result)['snapshots']
        self.assertEqual(values[0]['scenarios']['federal_CA_carry']['liquidation_after_tax_NAV_cents'],SEED+4499)
        self.assertEqual(values[1]['scenarios']['federal_CA_carry']['liquidation_after_tax_NAV_cents'],SEED+4927)
        self.assertEqual(values[1]['scenarios']['federal_CA_carry']['period_liquidation_wealth_change_cents'],428)
        self.assertEqual(values[1]['scenarios']['federal_CA_carry']['modeled_cumulative_liquidation_tax_cents'],old['terminal_liquidation_tax_cents'])


if __name__=='__main__':unittest.main()
