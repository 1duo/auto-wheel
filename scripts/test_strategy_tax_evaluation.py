import unittest
import test_strategy_tax_analytics as fixtures
from strategy_tax import SEED,analyze_records,tax_ledger,after_tax


class SharedEvaluation(unittest.TestCase):
    def test_dividend_entitlement_and_payment_are_counted_once(self):
        f=fixtures.TaxAccountingTests()
        rows,result=f.records([(f.effect('buy_stock','2025-01-01',price_cents=100,fee_cents=100),-10100),
            (f.effect('dividend','2025-02-01'),0),(f.effect('pay_dividend','2025-02-15'),100)],stock_bid=100)
        rows[1]['receivable_delta_cents']=100
        rows[2]['receivable_delta_cents']=-100
        ledger=tax_ledger(rows,result)
        self.assertEqual(ledger['remaining_receivables_cents'],0)
        self.assertEqual(ledger['dividends_paid_cents'],100)
        self.assertEqual(after_tax(ledger)['modeled_tax_cents'],41)
        self.assertEqual(result['net_profit_cents'],0)

    def test_shared_entrypoint_requires_a_complete_consistent_account(self):
        f=fixtures.TaxAccountingTests()
        rows,result=f.records([(f.effect('buy_stock','2025-01-01',price_cents=100,fee_cents=100),-10100)],stock_bid=120)
        with self.assertRaises(ValueError):analyze_records(rows,result)
        result['status']='complete'
        report=analyze_records(rows,result)
        self.assertEqual(set(report['scenarios']),{'federal_carry','federal_deny_losses','federal_CA_carry','federal_CA_deny_losses'})
        self.assertEqual(report['tax_NAV_identity_difference_cents'],0)
        result['last_near_close_NAV_cents']+=1
        with self.assertRaises(ValueError):analyze_records(rows,result)


if __name__=='__main__':unittest.main()
