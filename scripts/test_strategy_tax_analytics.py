import unittest
from strategy_tax import SEED, tax_ledger, tax_schedule, after_tax


class TaxAccountingTests(unittest.TestCase):
    def records(self, effects, stock_bid=None):
        cash = SEED; rows = []
        for e, delta in effects:
            cash += delta
            rows.append(dict(effect=e, cash_delta_cents=delta, cash_after_cents=cash,
                             receivable_delta_cents=0))
        nav = cash+(100*stock_bid if stock_bid is not None else 0)
        rows.append(dict(effect=dict(action='mark', at='2025-12-31T15:59:00-05:00', ref='NAV-2025-12-31'),
                         cash_after_cents=cash, receivable_delta_cents=0,
                         mark=dict(NAV_cents=nav, stock_books={'X': dict(bid_cents=stock_bid)} if stock_bid else {}, option_books={})))
        return rows, dict(last_near_close_NAV_cents=nav, net_profit_cents=nav-SEED)

    def effect(self, action, day, **kw):
        return dict(action=action, symbol='X', at=day+'T16:00:00-05:00', ref=day+action, **kw)

    def test_put_and_call_assignment_basis(self):
        put = dict(id='p', kind='put', strike_cents=10000, expiry='2025-02-01')
        call = dict(id='c', kind='call', strike_cents=10000, expiry='2025-03-01')
        rows, result = self.records([
            (self.effect('sell_put','2025-01-01', contract=put, price_cents=100, fee_cents=65), 9935),
            (self.effect('resolve','2025-02-01', contract=put, outcome='assigned', fee_cents=100), -1000100),
            (self.effect('sell_call','2025-02-02', contract=call, price_cents=50, fee_cents=65), 4935),
            (self.effect('resolve','2025-03-01', contract=call, outcome='called_away', fee_cents=100), 999900),
        ])
        ledger = tax_ledger(rows, result)
        self.assertEqual(ledger['capital_cents'], 14670)
        self.assertEqual(ledger['stock_sales'][0]['adjusted_stock_basis_cents'], 990165)

    def test_wash_loss_moves_to_replacement_basis(self):
        call = dict(id='c', kind='call', strike_cents=90, expiry='2025-02-01')
        rows, result = self.records([
            (self.effect('buy_stock','2025-01-01',price_cents=100,fee_cents=100), -10100),
            (self.effect('sell_call','2025-01-02',contract=call,price_cents=1,fee_cents=65), 35),
            (self.effect('resolve','2025-02-01',contract=call,outcome='called_away',fee_cents=100), 8900),
            (self.effect('buy_stock','2025-02-10',price_cents=80,fee_cents=100), -8100),
        ], stock_bid=95)
        ledger = tax_ledger(rows, result)
        self.assertEqual(ledger['wash_sales'][0]['deferred_loss_cents'], 1165)
        self.assertEqual(ledger['capital_cents'], 0)
        self.assertEqual(ledger['unrealized_capital_cents'], 235)
        self.assertEqual(tax_ledger(rows, result, wash=False)['capital_cents'], -1165)

    def test_annual_loss_credit_is_limited_and_carries(self):
        total, years = tax_schedule({'2025': dict(capital_cents=-1000000,positive_capital_cents=0,dividend_cents=0)})
        self.assertEqual(total, -111000)
        self.assertEqual(years[0]['outgoing_capital_loss_carry_cents'], 700000)
        self.assertEqual(tax_schedule({'2025': dict(capital_cents=-1000000,positive_capital_cents=0,dividend_cents=0)}, deny_losses=True)[0], 0)

    def test_open_gain_is_taxed_in_liquidation_comparison(self):
        rows, result = self.records([(self.effect('buy_stock','2025-01-01',price_cents=100,fee_cents=100), -10100)], stock_bid=120)
        report = after_tax(tax_ledger(rows,result))
        self.assertEqual(report['modeled_tax_cents'], 0)
        self.assertEqual(report['terminal_liquidation_tax_cents'], 734)
        self.assertLess(report['terminal_liquidation_after_tax_return_pct'], report['after_tax_return_pct'])


if __name__ == '__main__':
    unittest.main()
