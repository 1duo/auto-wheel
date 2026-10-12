// Focused checks for financial aggregation and unsafe report suppression.
const assert = require('node:assert/strict');
const {normalize, weekly, monday, quarterStart, moveQuarter, quarterWeeks, dateKey, money, add, parseReport, attachedReports, robinhoodOnly} = require('../internal/calendarui/calendar.js');
const {trimReport, attachHTML} = require('./attach-calendar-history.cjs');

function report() {
  return {
    schema_version: 'AutoWheelReplayReportV1', status: 'fixture',
    spec: {mode: 'fixture', corporate_actions_audited: true, start_date: '2026-01-29', end_date: '2026-02-06', initial_cash_cents: 100000, strategy_config: {selection_mode: 'conservative'}},
    expected_decisions: 2, observed_decisions: 2, expected_valuations: 4, observed_valuations: 4,
    performance: {ending_equity_cents: 99500, net_pnl_cents: -500},
    equity_curve: [
      {at: '2026-01-29T21:00:00Z', equity_cents: 100200},
      {at: '2026-01-30T21:00:00Z', equity_cents: 100500},
      {at: '2026-02-02T21:00:00Z', equity_cents: 99900},
      {at: '2026-02-06T21:00:00Z', equity_cents: 99500},
    ], trades: [{at: '2026-01-29T15:00:00Z', symbol: 'NVDA', action: 'sell_put', cash_flow_cents: 2000}]
  };
}
const normalized = normalize(report(), 'fixture.json');
assert.equal(normalized.kind, 'Synthetic fixture');
const weeks = weekly(normalized.scenarios[0]);
assert.equal(weeks.get('2026-01-26').pnl, 500);
assert.equal(weeks.get('2026-02-02').pnl, -1000);
assert.equal(weeks.get('2026-01-26').trades[0].cash, 2000); // Premium is not P&L.
assert.equal([...weeks.values()].reduce((sum, week) => sum + week.pnl, 0), -500);
assert.equal(weeks.get('2026-01-26').partial, true);
assert.equal(monday('2027-01-01'), '2026-12-28');
assert.equal(quarterStart('2026-03-31'), '2026-01-01');
assert.equal(quarterStart('2026-04-01'), '2026-04-01');
assert.equal(moveQuarter('2026-10-01', 1), '2027-01-01');
assert.equal(moveQuarter('2026-01-01', -1), '2025-10-01');
const boundaryQuarter = quarterWeeks('2024-07-01');
assert.equal(boundaryQuarter.length, 14);
assert.equal(boundaryQuarter.at(-1), '2024-09-30'); // The Sep 30–Oct 6 week belongs only to Q3.
assert.equal(quarterWeeks('2024-10-01')[0], '2024-10-07');
assert.equal(quarterWeeks('2026-10-01').length, 13);
assert.equal(quarterWeeks('2026-10-01').at(-1), '2026-12-28');
assert.equal(quarterWeeks('2027-01-01')[0], '2027-01-04');
const yearWeeks = ['2026-01-01', '2026-04-01', '2026-07-01', '2026-10-01'].flatMap(quarterWeeks);
assert.equal(yearWeeks.length, 52);
assert.equal(new Set(yearWeeks).size, 52);
const crossing = weekly({initial: 100000, start: '2026-03-30', end: '2026-04-10', trades: [], curve: [{date: '2026-03-31', equity: 100500}, {date: '2026-04-03', equity: 100900}, {date: '2026-04-10', equity: 100800}]});
const quarterPnl = date => quarterWeeks(date).reduce((sum, key) => add(sum, crossing.get(key)?.pnl ?? 0), 0);
assert.equal(quarterPnl('2026-01-01'), 900); // Apr 1–3 remain part of the Mar 30 week.
assert.equal(quarterPnl('2026-04-01'), -100);
assert.equal(quarterPnl('2026-01-01') + quarterPnl('2026-04-01'), 800);
assert.equal(dateKey('2026-10-05T01:00:00Z'), '2026-10-04'); // Eastern date, not UTC.
assert.equal(dateKey('2026-03-09T03:30:00Z'), '2026-03-08'); // DST.
assert.equal(money(-199), '−$1.99');
assert.equal(money(0), '$0.00');
assert.throws(() => add(Number.MAX_SAFE_INTEGER, 1), /safe integer/);
assert.equal(parseReport('{"cost_cents":199,"delta":0.1}').cost_cents, 199);
assert.equal(parseReport('{"cost_cents":null}').cost_cents, null);
assert.equal(parseReport('{"average_committed_cents":123.5,"cash_cents":100}').cash_cents, 100);
assert.equal(parseReport('{"source":"\\\"cost_cents\\\":0.001","cost_cents":1}').cost_cents, 1);
assert.throws(() => parseReport('{"cost_cents":199.00000000000001}'), /integer cents/);
assert.throws(() => parseReport('{"cost_cents":1e2}'), /integer cents/);
assert.throws(() => parseReport('{"cost_cents":9007199254740993}'), /safe integer/);
assert.throws(() => parseReport('{"cost_\\u0063ents":1.00000000000000001}'), /integer cents/);
assert.throws(() => dateKey('2026-02-30'), /Invalid/);
assert.throws(() => dateKey('2026-10-05T09:00:00'), /timezone/);

const blocked = report(); blocked.status = 'blocked';
assert.equal(normalize(blocked, 'blocked.json').scenarios.length, 0);
const incomplete = report(); incomplete.observed_valuations = 3;
assert.throws(() => normalize(incomplete, 'incomplete.json'), /coverage/);
const inconsistent = report(); inconsistent.performance.net_pnl_cents = 2000;
assert.throws(() => normalize(inconsistent, 'bad.json'), /reconcile/);
const duplicate = report(); duplicate.equity_curve[1].at = duplicate.equity_curve[0].at;
assert.throws(() => normalize(duplicate, 'bad.json'), /unique/);
const unsafe = report(); unsafe.equity_curve[0].equity_cents = 9007199254740992;
assert.throws(() => normalize(unsafe, 'bad.json'), /safe integer/);
const corruptCash = report(); Object.assign(corruptCash.equity_curve[0], {economic_cash_cents: 1, stock_value_cents: 0, option_liability_cents: 0});
assert.throws(() => normalize(corruptCash, 'bad.json'), /reconcile/);

const missing = weekly({initial: 100000, start: '2026-02-02', end: '2026-02-13', trades: [], curve: [{date: '2026-02-02', equity: null}, {date: '2026-02-06', equity: 101000}, {date: '2026-02-13', equity: 102000}]});
assert.equal(missing.get('2026-02-02').pnl, null);
assert.equal(missing.get('2026-02-09').pnl, 1000);
const gap = weekly({initial: 100000, start: '2026-02-02', end: '2026-02-27', trades: [], curve: [{date: '2026-02-06', equity: 101000}, {date: '2026-02-20', equity: 105000}, {date: '2026-02-27', equity: 106000}]});
assert.equal(gap.get('2026-02-16').pnl, null); // Never attribute two weeks of change to one.
assert.equal(gap.get('2026-02-23').pnl, 1000);
const eventWithoutMark = weekly({initial: 100000, start: '2026-02-02', end: '2026-02-13', curve: [{date: '2026-02-13', equity: 101000}], trades: [{date: '2026-02-03', cash: 1000, event: 'sell_put', symbol: 'NVDA'}]});
assert.equal(eventWithoutMark.get('2026-02-02').pnl, null);
assert.equal(eventWithoutMark.get('2026-02-02').trades.length, 1);
assert.equal(eventWithoutMark.get('2026-02-09').pnl, null);

const study = {schema: 'AutoWheelTwoMonthEndpointStudyV1', start: '2026-02-02', end: '2026-02-13', initial_cash_cents: 100000, results: [{mode: 'conservative', fee_per_contract_cents: 6, price_allowance_cents: 0, cash_ledger_reconciled: true, endpoint_complete: true, endpoint_pnl_reconciled: true, net_pnl_cents: 2000, ending_equity_cents: 102000, daily_equity: [{date: '2026-02-02', equity_cents: null}, {date: '2026-02-06', equity_cents: 101000}, {date: '2026-02-13', equity_cents: 102000}]}]};
assert.equal(weekly(normalize(study, 'study.json').scenarios[0]).get('2026-02-02').pnl, null);
study.results.push({...study.results[0], cash_ledger_reconciled: false});
assert.match(normalize(study, 'study.json').scenarios[1].reason, /not reconciled/);
assert.throws(() => normalize({schema: 'unrecognized', results: []}, 'bad.json'), /Unsupported/);
const privateReport = report();
privateReport.account_number = 'private-account';
privateReport.spec.strategy_config.token = 'private-token';
privateReport.spec.strategy_config.account_number = 'private-account';
privateReport.assumptions = ['Untrusted </script><script>text</script> stays text.'];
const attachment = [{name: 'Attached fixture', report: trimReport(privateReport)}];
const html = attachHTML('<script id="attached-reports" type="application/json">[]</script>', attachment);
assert.ok(!html.includes('private-account') && !html.includes('private-token'));
assert.equal((html.match(/<script/g) || []).length, 1);
const attached = attachedReports(html.match(/application\/json">([\s\S]*)<\/script>/)[1]);
assert.equal(attached[0].name, 'Attached fixture');
assert.equal(weekly(attached[0].scenarios[0]).get('2026-02-02').pnl, -1000);
assert.equal(attachedReports('[]').length, 0);
assert.throws(() => attachedReports('{}'), /must be a list/);
const mixed = {...study, limitations: ['Six-cent fee is a fixed retail estimate, not an invoice; $2 plus one-cent allowance is a separate cost/path sensitivity. No cash interest or taxes.'], results: [study.results[0], {...study.results[0], fee_per_contract_cents: 200, price_allowance_cents: 1}]};
const cleaned = robinhoodOnly(mixed);
assert.equal(cleaned.results.length, 1);
assert.equal(cleaned.results[0], mixed.results[0]); // Keep the original Robinhood result, never recalculate its returns.
assert.ok(!cleaned.limitations[0].includes('$2'));
assert.ok(cleaned.limitations[0].includes('No cash interest or taxes.'));
assert.equal(normalize(mixed, 'mixed.json').scenarios[0].name, 'conservative');
assert.equal(trimReport(mixed).results.length, 1);
const cleanedHTML = attachHTML('<script id="attached-reports" type="application/json">[]</script>', [{name:'Mixed',report:mixed}]);
assert.ok(!cleanedHTML.includes('"fee_per_contract_cents":200') && !cleanedHTML.includes('"price_allowance_cents":1'));
assert.equal(attachedReports(cleanedHTML.match(/application\/json">([\s\S]*)<\/script>/)[1])[0].scenarios.length, 1);
assert.throws(() => normalize({...mixed,results:[mixed.results[1]]}, 'other-fee.json'), /no results using the saved Robinhood/);
assert.throws(() => normalize({...mixed,results:[{...mixed.results[0],price_allowance_cents:1}]}, 'other-allowance.json'), /no results using the saved Robinhood/);
assert.throws(() => normalize({...mixed,results:[{...mixed.results[0],fee_per_contract_cents:undefined}]}, 'unknown-fee.json'), /no results using the saved Robinhood/);
console.log('Calendar checks passed: weekly attribution, exact cents, Eastern dates, coverage, missing marks and blocked performance.');
