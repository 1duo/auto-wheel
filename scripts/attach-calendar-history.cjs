// Attach existing, checked research results to a local calendar HTML copy.
// Personal research stays outside source, binaries and the public operator API.
const fs = require('node:fs');
const path = require('node:path');
const calendar = require('../internal/calendarui/calendar.js');

function pick(object, keys) {
  return Object.fromEntries(keys.filter(key => object[key] !== undefined).map(key => [key, object[key]]));
}
function trimReport(report) {
  report = calendar.robinhoodOnly(report);
  const out = pick(report, ['schema_version', 'schema', 'status', 'blockers', 'assumptions', 'start', 'end', 'window', 'symbol', 'initial_cash_cents', 'starting_cash_cents', 'max_collateral_cents', 'fee_model', 'limitations', 'valuation_policy', 'expiry_policy', 'ranking_policy', 'universe_selection_bias', 'input_sha256', 'strategy_sha256', 'simulation_engine_sha256', 'expected_decisions', 'observed_decisions', 'expected_valuations', 'observed_valuations', 'performance']);
  const curve = rows => rows?.map(point => {
    const value = pick(point, ['at', 'date', 'equity_cents', 'economic_cash_cents', 'cash_cents', 'stock_value_cents', 'option_liability_cents', 'dividend_receivable_cents']);
    if (Array.isArray(point.positions)) value.positions = point.positions.map(position => pick(position, ['stock_value_cents', 'option_liability_cents']));
    return value;
  });
  const trades = rows => rows?.map(trade => pick(trade, ['at', 'date', 'symbol', 'action', 'event', 'cash_flow_cents', 'cashflow_cents']));
  if (report.spec) {
    out.spec = pick(report.spec, ['mode', 'start_date', 'end_date', 'initial_cash_cents', 'corporate_actions_audited']);
    if (report.spec.strategy_config) out.spec.strategy_config = pick(report.spec.strategy_config, ['selection_mode', 'fee_per_contract_cents']);
  }
  if (report.equity_curve) out.equity_curve = curve(report.equity_curve);
  if (report.trades) out.trades = trades(report.trades);
  if (report.results) out.results = report.results.map(result => {
    const value = pick(result, ['mode', 'strategy', 'fee_model', 'fee_per_contract_cents', 'price_allowance_cents', 'starting_cash_cents', 'accounting_reconciled', 'cash_ledger_reconciled', 'endpoint_complete', 'endpoint_pnl_reconciled', 'pnl_reconciled', 'daily_valuation_complete', 'net_pnl_cents', 'ending_equity_cents']);
    if (result.profile) value.profile = pick(result.profile, ['id']);
    value.daily_equity = curve(result.daily_equity);
    value.trade_ledger = trades(result.trade_ledger);
    return value;
  });
  return out;
}
function attachHTML(html, entries) {
  entries = entries.map(entry => ({...entry, report: calendar.robinhoodOnly(entry.report)}));
  calendar.attachedReports(JSON.stringify(entries));
  const data = JSON.stringify(entries).replaceAll('<', '\\u003c').replaceAll('\u2028', '\\u2028').replaceAll('\u2029', '\\u2029');
  const pattern = /(<script id="attached-reports" type="application\/json">)[\s\S]*?(<\/script>)/;
  if (!pattern.test(html)) throw new Error('Calendar attachment placeholder missing.');
  return html.replace(pattern, (_, start, end) => start + data + end);
}
function defaultSources(root) {
  const research = path.join(root, 'research/pilots/2026-ytd/ten-symbol-v1');
  const sources = [{name: 'Portfolio · 2026 YTD', file: path.join(research, 'shared-account-roi-v1-20261011/performance.json')}];
  const symbols = path.join(research, 'study-robinhood-v2-eod/results');
  if (fs.existsSync(symbols)) for (const name of fs.readdirSync(symbols).filter(name => name.endsWith('.json')).sort()) {
    sources.push({name: name.slice(0, -5) + ' · 2026 YTD', file: path.join(symbols, name)});
  }
  return sources.filter(source => fs.existsSync(source.file));
}
if (require.main === module) {
  const target = process.argv[2];
  if (!target) throw new Error('Usage: node scripts/attach-calendar-history.cjs CALENDAR_HTML [REPORT_JSON ...]');
  const root = path.resolve(__dirname, '..');
  const sources = process.argv.length > 3 ? process.argv.slice(3).map(file => ({name: path.basename(file), file})) : defaultSources(root);
  const entries = sources.map(source => {
    const raw = calendar.parseReport(fs.readFileSync(source.file, 'utf8'));
    calendar.prepareReport(raw, source.name);
    return {name: source.name, report: trimReport(raw)};
  });
  const html = attachHTML(fs.readFileSync(target, 'utf8'), entries);
  fs.writeFileSync(target, html);
  fs.chmodSync(target, 0o600);
  console.log(`Attached ${entries.length} historical reports to the local calendar.`);
}
module.exports = {trimReport, attachHTML};
