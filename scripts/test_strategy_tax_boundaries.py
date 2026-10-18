import unittest
from strategy_tax import tax_ledger, tax_schedule, after_tax
import test_strategy_tax_analytics as fixtures


class TaxBoundaries(unittest.TestCase):
    def test_30_day_replacement_is_deferred_but_day_31_is_not(self):
        fixture=fixtures.TaxAccountingTests()
        call=dict(id='c',kind='call',strike_cents=90,expiry='2025-02-01')
        for day,wash_count in [('2025-03-03',1),('2025-03-04',0)]:
            rows,result=fixture.records([
                (fixture.effect('buy_stock','2025-01-01',price_cents=100,fee_cents=100),-10100),
                (fixture.effect('sell_call','2025-01-02',contract=call,price_cents=1,fee_cents=65),35),
                (fixture.effect('resolve','2025-02-01',contract=call,outcome='called_away',fee_cents=100),8900),
                (fixture.effect('buy_stock',day,price_cents=80,fee_cents=100),-8100),
            ],stock_bid=95)
            self.assertEqual(len(tax_ledger(rows,result)['wash_sales']),wash_count)

    def test_loss_carry_reduces_next_year_gain_without_NIIT_refund(self):
        tax,years=tax_schedule({'2024':dict(capital_cents=-1000000,positive_capital_cents=0,dividend_cents=0),
                                '2025':dict(capital_cents=1000000,positive_capital_cents=1000000,dividend_cents=0)})
        self.assertEqual(tax,11400)
        self.assertEqual(years[1]['taxable_net_capital_cents'],300000)

    def test_doubled_costs_scale_terminal_liquidation_fee(self):
        fixture=fixtures.TaxAccountingTests()
        rows,result=fixture.records([(fixture.effect('buy_stock','2025-01-01',price_cents=100,fee_cents=100),-10100)],stock_bid=120)
        result['modeled_costs']={'scale':2}
        ledger=tax_ledger(rows,result)
        self.assertEqual(ledger['modeled_terminal_closing_fees_cents'],200)
        self.assertEqual(after_tax(ledger)['terminal_liquidation_tax_cents'],694)


if __name__=='__main__':unittest.main()
