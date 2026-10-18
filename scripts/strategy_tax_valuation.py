"""Year-end after-tax liquidation wealth, separate from realized-tax cash NAV."""
from strategy_tax import SEED,tax_ledger,after_tax,require


def annual_liquidation_values(records,result):
    require(result.get('status')=='complete' and result['last_near_close_NAV_cents']-result['net_profit_cents']==SEED,
            'complete fixed-seed account required')
    ends={}
    for index,row in enumerate(records):
        e=row['effect']
        if e['action']=='mark' and e['ref'].startswith('NAV-'):ends[e['at'][:4]]=index
    require(ends,'no near-close valuation points')
    snapshots=[];previous={name:SEED for name in ('federal_carry','federal_deny_losses','federal_CA_carry','federal_CA_deny_losses')}
    for year,index in sorted(ends.items()):
        mark=records[index];nav=mark['mark']['NAV_cents']
        prefix_result=dict(result,last_near_close_NAV_cents=nav,net_profit_cents=nav-SEED)
        prefix=records[:index+1]
        ledgers={False:tax_ledger(prefix,prefix_result),True:tax_ledger(prefix,prefix_result,wash=False)}
        scenarios={}
        for name,ca in [('federal',0),('federal_CA',1330)]:
            for denial in (False,True):
                key=name+('_deny_losses' if denial else '_carry');ledger=ledgers[denial]
                tax=after_tax(ledger,ca=ca,deny_losses=denial)
                wealth=nav-ledger['modeled_terminal_closing_fees_cents']-tax['terminal_liquidation_tax_cents']
                scenarios[key]=dict(liquidation_after_tax_NAV_cents=wealth,
                    modeled_cumulative_liquidation_tax_cents=tax['terminal_liquidation_tax_cents'],
                    period_liquidation_wealth_change_cents=wealth-previous[key],
                    period_liquidation_wealth_return_pct=100*(wealth-previous[key])/previous[key] if previous[key]>0 else None)
                previous[key]=wealth
        snapshots.append(dict(year=year,as_of=mark['effect']['at'],scenarios=scenarios))
    return dict(kind='year_end_or_final_observed_date_hypothetical_liquidation',snapshots=snapshots,
        initial_seed_cents=SEED,actual_strategy_resets_or_liquidations=0,
        future_replacements_after_each_cutoff_censored=True,
        purpose='Separate tax on previously accrued gains from current-period economic wealth. These are valuation snapshots, not trades or personal returns.')
