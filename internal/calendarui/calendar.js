/* Read-only calendar. Imported report text is rendered only with textContent. */
(function () {
  "use strict";
  const eastern = new Intl.DateTimeFormat("en-CA", {timeZone: "America/New_York", year: "numeric", month: "2-digit", day: "2-digit"});
  function cents(value) {
    if (!Number.isSafeInteger(value)) throw new Error("Money must be exact, safe integer cents.");
    return value;
  }
  function add(a, b) { return cents(cents(a) + cents(b)); }
  function subtract(a, b) { return cents(cents(a) - cents(b)); }
  function money(value, signed = false) {
    if (value === null || value === undefined) return "—";
    cents(value);
    const absolute = Math.abs(value);
    return (value < 0 ? "−" : signed && value > 0 ? "+" : "") + "$" + Math.floor(absolute / 100).toLocaleString("en-US") + "." + String(absolute % 100).padStart(2, "0");
  }
  function dateKey(value) {
    if (typeof value !== "string") throw new Error("Missing observation date.");
    if (/^\d{4}-\d{2}-\d{2}$/.test(value)) {
      const date = new Date(value + "T12:00:00Z");
      if (!Number.isFinite(date.getTime()) || date.toISOString().slice(0, 10) !== value) throw new Error("Invalid observation date.");
      return value;
    }
    if (!/^\d{4}-\d{2}-\d{2}T.*(?:Z|[+-]\d{2}:\d{2})$/.test(value)) throw new Error("Observation timestamps require a timezone.");
    const date = new Date(value);
    if (!Number.isFinite(date.getTime())) throw new Error("Invalid observation timestamp.");
    return eastern.format(date);
  }
  function shift(key, days) {
    const date = new Date(key + "T12:00:00Z");
    date.setUTCDate(date.getUTCDate() + days);
    return date.toISOString().slice(0, 10);
  }
  function monday(key) {
    return shift(key, -((new Date(key + "T12:00:00Z").getUTCDay() + 6) % 7));
  }
  function quarterStart(key) {
    key = dateKey(key);
    const month = 1 + Math.floor((Number(key.slice(5, 7)) - 1) / 3) * 3;
    return key.slice(0, 4) + "-" + String(month).padStart(2, "0") + "-01";
  }
  function moveQuarter(key, offset) {
    const date = new Date(quarterStart(key) + "T12:00:00Z");
    date.setUTCMonth(date.getUTCMonth() + offset * 3);
    return date.toISOString().slice(0, 10);
  }
  function quarterWeeks(key) {
    const start = quarterStart(key), end = moveQuarter(start, 1), keys = [];
    let first = monday(start);
    if (first < start) first = shift(first, 7);
    for (let week = first; week < end; week = shift(week, 7)) keys.push(week);
    return keys;
  }
  function label(key, year = false) {
    return new Intl.DateTimeFormat("en-US", {timeZone: "UTC", month: "short", day: "numeric", ...(year ? {year: "numeric"} : {})}).format(new Date(key + "T12:00:00Z"));
  }
  function textList(value) { return Array.isArray(value) ? value.filter(item => typeof item === "string") : []; }
  function parseReport(text) {
    const report = JSON.parse(text);
    // Validate original money tokens before rounded JSON numbers can reach accounting.
    // This also works in browsers without JSON.parse's newer source-context argument.
    const tokens = /"(?:\\[\s\S]|[^"\\])*"|[^\s{}\[\],:"]+|[{}\[\],:]/g;
    let previous, beforePrevious;
    for (const match of text.matchAll(tokens)) {
      const token = match[0];
      if (previous === ":" && beforePrevious?.startsWith('"')) {
        const key = JSON.parse(beforePrevious);
        // A study's time-averaged exposure is a fractional diagnostic, never used for P&L.
        if (key.endsWith("_cents") && key !== "average_committed_cents" && token !== "null") {
          if (!/^-?(?:0|[1-9]\d*)$/.test(token)) throw new Error("Report money must use integer cents, without decimals or exponents.");
          cents(Number(token));
        }
      }
      beforePrevious = previous;
      previous = token;
    }
    return report;
  }
  function points(raw, start, end) {
    if (!Array.isArray(raw) || !raw.length) throw new Error("No equity valuations in this result.");
    let previous = "";
    return raw.map(point => {
      const date = dateKey(point.at ?? point.date);
      // Calendar uses one closing valuation per session, never sorts damaged input.
      if (date <= previous || date < start || date > end) throw new Error("Valuation dates must be unique, increasing and inside the report window.");
      previous = date;
      const equity = point.equity_cents === null ? null : cents(point.equity_cents);
      const cash = point.economic_cash_cents ?? point.cash_cents;
      if (equity !== null && cash !== undefined && point.stock_value_cents !== undefined && point.option_liability_cents !== undefined) {
        const expected = subtract(add(add(cash, point.stock_value_cents), point.dividend_receivable_cents ?? 0), point.option_liability_cents);
        if (equity !== expected) throw new Error("An equity valuation does not reconcile to cash, stock and option liability.");
      }
      if (equity !== null && cash !== undefined && Array.isArray(point.positions)) {
        let expected = add(cash, point.dividend_receivable_cents ?? 0);
        for (const position of point.positions) expected = subtract(add(expected, position.stock_value_cents), position.option_liability_cents);
        if (equity !== expected) throw new Error("Portfolio equity does not reconcile to its positions.");
      }
      return {date, equity};
    });
  }
  function trades(raw, start, end, symbol) {
    if (raw === undefined || raw === null) return [];
    if (!Array.isArray(raw)) throw new Error("Invalid event ledger.");
    return raw.map(trade => {
      const date = dateKey(trade.at ?? trade.date);
      if (date < start || date > end) throw new Error("An event falls outside the report window.");
      const cash = trade.cash_flow_cents ?? trade.cashflow_cents;
      if (cash !== undefined && cash !== null) cents(cash);
      return {date, symbol: String(trade.symbol ?? symbol ?? "—"), event: String(trade.action ?? trade.event ?? "Event"), cash: cash ?? null};
    });
  }
  function reconcile(initial, curve, pnl, ending) {
    if (cents(initial) <= 0) throw new Error("Starting equity must be positive.");
    if (curve.at(-1).equity === null) {
      if (pnl !== null || (ending !== undefined && ending !== null)) throw new Error("Performance is present without a final equity valuation.");
      return;
    }
    if (ending !== undefined && ending !== null && cents(ending) !== curve.at(-1).equity) throw new Error("Ending equity does not match the valuation ledger.");
    if (pnl !== null && subtract(curve.at(-1).equity, initial) !== cents(pnl)) throw new Error("Net P&L does not reconcile to starting and ending equity.");
  }
  function blocked(name, report, reason) {
    return {name, kind: "Blocked simulation", start: null, scenarios: [], assumptions: [], reason: [reason, ...textList(report.blockers)].filter(Boolean).join("\n")};
  }
  const studySchemas = new Set([
    "AutoWheelTwoMonthStudyV1", "AutoWheelTwoMonthEndpointStudyV1", "AutoWheelThreeMonthEndpointStudyV1", "AutoWheelYearToDateEndpointStudyV1",
    "AutoWheelSharedPortfolioEndpointStudyV1", "AutoWheelSharedPortfolioVerifiedV1", "AutoWheelFairSecuredCostStudyV2", "AutoWheelRobinhoodSingleFeeStudyV1", "AutoWheelEODSupplementedAccountingV1"
  ]);
  const robinhoodFeeEstimate = Object.freeze({feeCents: 6, priceAllowanceCents: 0});
  function robinhoodOnly(report) {
    if (!report || !Array.isArray(report.results)) return report;
    const results = report.results.filter(result => result.fee_per_contract_cents === robinhoodFeeEstimate.feeCents && result.price_allowance_cents === robinhoodFeeEstimate.priceAllowanceCents);
    if (!results.length) throw new Error("This report has no results using the saved Robinhood fee estimate ($0.06 per filled option contract, no price allowance).");
    const clean = values => textList(values).map(text => text.replace("; $2 plus one-cent allowance is a separate cost/path sensitivity.", "."));
    return {...report, results, ...(report.limitations ? {limitations: clean(report.limitations)} : {}), ...(report.assumptions ? {assumptions: clean(report.assumptions)} : {})};
  }
  function normalize(report, name) {
    if (!report || typeof report !== "object" || Array.isArray(report)) throw new Error("Report must be a JSON object.");
    if (report.status === "blocked" || textList(report.blockers).length) return blocked(name, report, "No measured performance: report blocked.");
    if (report.schema_version === "AutoWheelReplayReportV1") {
      if (!["fixture", "counterfactual"].includes(report.status)) return blocked(name, report, "Unsupported replay status.");
      const spec = report.spec, perf = report.performance;
      if (!spec || !perf) return blocked(name, report, "Performance or replay specification missing.");
      const fixture = report.status === "fixture";
      if (!fixture && spec.strategy_config?.fee_per_contract_cents !== robinhoodFeeEstimate.feeCents) throw new Error("Historical replay does not use the saved Robinhood fee estimate ($0.06 per filled option contract).");
      if (spec.mode !== (fixture ? "fixture" : "historical_quote_replay") || spec.corporate_actions_audited !== true) throw new Error("Replay mode or corporate-action qualification failed.");
      for (const kind of ["decisions", "valuations"]) {
        const expected = report["expected_" + kind];
        if (!Number.isSafeInteger(expected) || expected <= 0 || expected !== report["observed_" + kind]) throw new Error("Incomplete replay observation coverage.");
      }
      if (!fixture && ["input_sha256", "strategy_sha256", "simulation_engine_sha256"].some(key => !/^[a-f0-9]{64}$/.test(report[key] ?? ""))) throw new Error("Historical replay integrity hashes missing.");
      const start = dateKey(spec.start_date), end = dateKey(spec.end_date);
      if (end < start) throw new Error("Invalid report window.");
      const curve = points(report.equity_curve, start, end);
      if (curve.length !== report.expected_valuations || curve.some(point => point.equity === null)) throw new Error("Incomplete replay valuations.");
      reconcile(spec.initial_cash_cents, curve, perf.net_pnl_cents, perf.ending_equity_cents);
      return {name, kind: fixture ? "Synthetic fixture" : "Historical counterfactual", start, feeEstimate: fixture ? null : robinhoodFeeEstimate.feeCents, assumptions: textList(report.assumptions), scenarios: [{name: spec.strategy_config?.selection_mode || "Replay strategy", start, end, initial: spec.initial_cash_cents, curve, trades: trades(report.trades, start, end), notes: []}]};
    }
    if (!studySchemas.has(report.schema)) throw new Error("Unsupported report schema. Load an AutoWheel replay or historical study result JSON.");
    if (!Array.isArray(report.results) || !report.results.length) throw new Error("Historical study has no scenarios.");
    report = robinhoodOnly(report);
    const assumptions = [...textList(report.limitations), ...[report.valuation_policy, report.expiry_policy, report.ranking_policy, report.universe_selection_bias].filter(value => typeof value === "string")];
    if (report.starting_cash_cents !== undefined) assumptions.push("Hypothetical starting cash: " + money(report.starting_cash_cents) + (report.max_collateral_cents !== undefined ? ". Research collateral ceiling: " + money(report.max_collateral_cents) + "." : "."));
    assumptions.unshift("Retrospective research with modeled fills and lifecycle events. Not live broker performance or a blind out-of-sample result.");
    const scenarios = report.results.map((result, index) => {
      const scenarioName = [report.symbol, result.strategy ?? result.profile?.id ?? result.mode ?? ("Scenario " + (index + 1))].filter(Boolean).join(" · ");
      try {
        if (result.accounting_reconciled !== true && result.cash_ledger_reconciled !== true) throw new Error("Scenario cash accounting is not reconciled.");
        if (result.endpoint_complete === false || result.endpoint_pnl_reconciled === false || result.pnl_reconciled === false) throw new Error("Scenario endpoint performance is incomplete or unreconciled.");
        const start = dateKey(report.start ?? report.window?.[0] ?? result.daily_equity?.[0]?.date);
        const end = dateKey(report.end ?? report.window?.[1] ?? result.daily_equity?.at(-1)?.date);
        if (end < start) throw new Error("Invalid report window.");
        const curve = points(result.daily_equity, start, end);
        // Some accounting wrappers omit the seed; derive it only from two reported, exact amounts.
        let initial = report.initial_cash_cents ?? report.starting_cash_cents ?? result.starting_cash_cents;
        const notes = [];
        if (initial === undefined) {
          initial = subtract(curve.at(-1).equity, result.net_pnl_cents);
          notes.push("Starting equity derived from final reported equity less reported net P&L.");
        }
        reconcile(initial, curve, result.net_pnl_cents, result.ending_equity_cents);
        if (result.daily_valuation_complete === false || curve.some(point => point.equity === null)) notes.push("Missing daily valuations: affected weeks have no P&L; no interpolation.");
        if (report.schema === "AutoWheelEODSupplementedAccountingV1") notes.push("Includes EOD supplemental option marks; strict 4pm valuation coverage is not established.");
        return {name: scenarioName, start, end, initial, curve, trades: trades(result.trade_ledger, start, end, report.symbol), notes};
      } catch (error) { return {name: scenarioName, reason: error.message}; }
    });
    return {name, kind: "Historical study · approximate", feeEstimate: robinhoodFeeEstimate.feeCents, start: scenarios.find(scenario => scenario.start)?.start ?? null, assumptions, scenarios};
  }
  function weekly(scenario) {
    const weeks = new Map();
    let previous = scenario.initial;
    let previousDate = null;
    for (const point of scenario.curve) {
      const key = monday(point.date);
      if (!weeks.has(key)) weeks.set(key, {start: key, end: shift(key, 6), pnl: 0, observations: 0, closing: null, trades: [], partial: key < scenario.start || shift(key, 6) > scenario.end});
      const week = weeks.get(key);
      if (point.equity === null || previous === null || point.date > shift(previousDate ?? scenario.start, 7)) week.pnl = null;
      else if (week.pnl !== null) week.pnl = add(week.pnl, subtract(point.equity, previous));
      week.closing = point.equity;
      week.observations++;
      previous = point.equity;
      previousDate = point.date;
    }
    for (const trade of scenario.trades) {
      const key = monday(trade.date);
      if (!weeks.has(key)) weeks.set(key, {start: key, end: shift(key, 6), pnl: null, observations: 0, closing: null, trades: [], partial: key < scenario.start || shift(key, 6) > scenario.end});
      weeks.get(key).trades.push(trade);
    }
    return weeks;
  }
  function prepareReport(raw, name) {
    const report = normalize(raw, name);
    for (const scenario of report.scenarios) if (scenario.curve) weekly(scenario);
    return report;
  }
  function attachedReports(text) {
    const entries = parseReport(text);
    if (!Array.isArray(entries)) throw new Error("Attached reports must be a list.");
    return entries.map(entry => {
      if (!entry || typeof entry.name !== "string") throw new Error("Invalid attached report.");
      return prepareReport(entry.report, entry.name);
    });
  }
  const core = {cents, add, subtract, money, dateKey, shift, monday, quarterStart, moveQuarter, quarterWeeks, parseReport, normalize, weekly, prepareReport, attachedReports, robinhoodOnly, robinhoodFeeEstimate};
  if (typeof module !== "undefined" && module.exports) module.exports = core;
  if (typeof document === "undefined") return;

  const byID = id => document.getElementById(id);
  const reports = [];
  let quarter = quarterStart(new Date().toISOString()), selectedWeek = null, weeks = new Map(), liveMessage = "Checking live account…";
  let sourceMode = "live";
  const modeQuarters = {live: quarter, simulation: quarter};
  function node(tag, text, className) {
    const element = document.createElement(tag);
    if (text !== undefined) element.textContent = text;
    if (className) element.className = className;
    return element;
  }
  function currentReport() { return reports[Number(byID("report").value)] ?? null; }
  function currentScenario() { return currentReport()?.scenarios[Number(byID("scenario").value)] ?? null; }
  function error(message) { byID("error").textContent = message; byID("error").hidden = !message; }
  function showDetail() {
    const week = weeks.get(selectedWeek);
    byID("week-detail").hidden = !week;
    if (!week) return;
    byID("detail-title").textContent = label(week.start, true) + " – " + label(week.end, true);
    byID("detail-summary").textContent = "Weekly P&L " + money(week.pnl, true);
    byID("events").replaceChildren();
    for (const trade of week.trades) {
      const row = node("tr");
      for (const value of [trade.date, trade.symbol, trade.event.replaceAll("_", " "), money(trade.cash, true)]) row.append(node("td", value));
      byID("events").append(row);
    }
    byID("detail-note").textContent = week.trades.length ? "Cash flows are reported ledger events, not weekly profit. Weekly P&L also reflects stock and option marks." : "No ledger events in this week. Equity changes may still produce P&L.";
  }
  function render() {
    const simulation = sourceMode === "simulation", report = simulation ? currentReport() : null, scenario = simulation ? currentScenario() : null;
    byID("live-mode").setAttribute("aria-pressed", String(!simulation));
    byID("simulation-mode").setAttribute("aria-pressed", String(simulation));
    byID("run-field").hidden = !simulation;
    byID("scenario-field").hidden = !simulation;
    byID("source-badge").textContent = simulation ? report?.kind ?? "Historical simulation" : "Live account";
    byID("notice").className = simulation ? "notice warning" : "notice";
    byID("notice").textContent = simulation ? report ? report.reason ?? scenario?.reason ?? [report.kind + " · " + (scenario?.start ?? "") + " to " + (scenario?.end ?? ""), ...(scenario?.notes ?? [])].join("\n") : "No historical reports are attached to this installation. Add saved AutoWheel report JSON files to view weekly simulation results." : liveMessage;
    byID("fee-estimate").hidden = !simulation || report?.feeEstimate === undefined || report?.feeEstimate === null;
    byID("fee-estimate").textContent = report?.feeEstimate !== undefined && report?.feeEstimate !== null ? "Robinhood fee estimate: " + money(report.feeEstimate) + " per filled option contract · estimate, not an invoice." : "";
    byID("assumptions").textContent = report?.assumptions.join("\n\n") ?? "";
    byID("disclosures").hidden = !report?.assumptions.length;
    weeks = scenario?.curve ? weekly(scenario) : new Map();
    const quarterDate = new Date(quarter + "T12:00:00Z"), endDate = new Date(shift(moveQuarter(quarter, 1), -1) + "T12:00:00Z");
    const monthLabel = new Intl.DateTimeFormat("en-US", {timeZone: "UTC", month: "short"});
    byID("quarter").textContent = "Q" + (quarterDate.getUTCMonth() / 3 + 1) + " " + quarterDate.getUTCFullYear() + " · " + monthLabel.format(quarterDate) + "–" + monthLabel.format(endDate);
    const keys = quarterWeeks(quarter), rows = keys.map(key => weeks.get(key)).filter(Boolean);
    const valued = rows.filter(week => week.pnl !== null);
    let total = rows.length && valued.length === rows.length ? 0 : null;
    if (total !== null) {
      try { for (const week of rows) total = add(total, week.pnl); }
      catch (_) { total = null; error("Quarter P&L exceeds the exact integer display range."); }
    }
    byID("chips").replaceChildren();
    for (const [caption, value] of [["Quarter P&L (observed)", money(total, true)], ["Valued weeks", valued.length + " / " + keys.length], ["Ledger events", rows.length ? String(rows.reduce((count, week) => count + week.trades.length, 0)) : "—"]]) {
      const chip = node("span", caption + " ", "chip"); chip.append(node("b", value)); byID("chips").append(chip);
    }
    const grid = byID("weeks"); grid.replaceChildren();
    const todayWeek = monday(dateKey(new Date().toISOString()));
    for (const key of keys) {
      const week = weeks.get(key), value = week?.pnl;
      const button = node("button", undefined, "week " + (value === undefined || value === null ? "" : value > 0 ? "pos" : value < 0 ? "neg" : "flat") + (key === todayWeek ? " current" : ""));
      button.type = "button"; button.setAttribute("aria-pressed", String(selectedWeek === key));
      button.setAttribute("aria-label", "Week " + label(key, true) + ", P&L " + money(value, true));
      button.append(node("span", label(key) + " – " + label(shift(key, 6)), "range"), node("span", money(value, true), "amount"));
      button.append(node("span", week ? week.observations + " valuations · " + week.trades.length + " events" : "No observations", "sub"));
      if (week) {
        button.append(node("span", "Closing equity " + money(week.closing), "sub"));
        if (week.partial) button.append(node("span", "Partial report week", "sub"));
        if (week.pnl === null) button.append(node("span", "Missing valuation / boundary", "sub"));
      }
      button.disabled = !week;
      button.addEventListener("click", () => { selectedWeek = key; render(); });
      grid.append(button);
    }
    showDetail();
  }
  function populateScenarios() {
    const report = currentReport(), select = byID("scenario"); select.replaceChildren();
    for (const [index, scenario] of (report?.scenarios ?? []).entries()) {
      const option = node("option", scenario.name + (scenario.reason ? " · Unavailable" : "")); option.value = String(index); select.append(option);
    }
    select.disabled = !report?.scenarios.length;
    if (select.disabled) select.append(node("option", "No measured scenarios"));
    selectedWeek = null;
    const scenario = currentScenario();
    if (scenario?.curve) modeQuarters.simulation = quarterStart(monday(scenario.curve.at(-1).date));
    if (sourceMode === "simulation") quarter = modeQuarters.simulation;
    render();
  }
  function populateReports(selected = 0) {
    byID("report").replaceChildren();
    reports.forEach((report, index) => { const option = node("option", report.name + " · " + report.kind); option.value = String(index); byID("report").append(option); });
    byID("report").disabled = !reports.length;
    if (reports.length) byID("report").value = String(selected);
    else byID("report").append(node("option", "No attached reports"));
    populateScenarios();
  }
  function setMode(mode) {
    modeQuarters[sourceMode] = quarter;
    sourceMode = mode;
    selectedWeek = null;
    quarter = modeQuarters[mode];
    try { localStorage.setItem("autowheel-calendar-mode", mode); } catch (_) {}
    render();
  }
  byID("live-mode").addEventListener("click", () => setMode("live"));
  byID("simulation-mode").addEventListener("click", () => setMode("simulation"));
  byID("report").addEventListener("change", populateScenarios);
  byID("scenario").addEventListener("change", () => { selectedWeek = null; if (currentScenario()?.curve) quarter = quarterStart(monday(currentScenario().curve.at(-1).date)); render(); });
  byID("import").addEventListener("click", () => byID("files").click());
  byID("files").addEventListener("change", async () => {
    const failures = [], firstNew = reports.length;
    for (const file of byID("files").files) {
      try {
        if (file.size > 16 * 1024 * 1024) throw new Error("Report exceeds the 16 MB limit.");
        reports.push(prepareReport(parseReport(await file.text()), file.name));
      } catch (caught) { failures.push(file.name + ": " + caught.message); }
    }
    byID("files").value = "";
    if (reports.length > firstNew) {
      populateReports(firstNew);
      setMode("simulation");
    }
    error(failures.join("\n"));
  });
  function navigate(offset) {
    quarter = moveQuarter(quarter, offset); selectedWeek = null; render();
  }
  byID("prev").addEventListener("click", () => navigate(-1));
  byID("next").addEventListener("click", () => navigate(1));
  byID("current").addEventListener("click", () => { quarter = quarterStart(new Date().toISOString()); selectedWeek = null; render(); });
  const media = window.matchMedia("(prefers-color-scheme: dark)");
  let savedTheme;
  try { savedTheme = localStorage.getItem("autowheel-calendar-theme"); } catch (_) { /* Local files may disallow storage. */ }
  function applyTheme(dark) { document.documentElement.dataset.theme = dark ? "dark" : "light"; byID("theme").checked = dark; }
  applyTheme(savedTheme ? savedTheme === "dark" : media.matches);
  byID("theme").addEventListener("change", () => { applyTheme(byID("theme").checked); savedTheme = byID("theme").checked ? "dark" : "light"; try { localStorage.setItem("autowheel-calendar-theme", savedTheme); } catch (_) {} });
  media.addEventListener("change", event => { if (!savedTheme) applyTheme(event.matches); });
  async function loadLive() {
    if (location.protocol === "file:") liveMessage = "Verified live P&L history is unavailable. Switch to Simulation to view the attached historical results.";
    else {
      try {
        const response = await fetch("/status", {cache: "no-store", signal: AbortSignal.timeout(7000)});
        if (!response.ok) throw new Error("Daemon unavailable");
        const status = await response.json();
        const at = new Date(status.server_time);
        if (!Number.isFinite(at.getTime()) || Math.abs(Date.now() - at.getTime()) > 120000) throw new Error("Daemon status is stale");
        liveMessage = "Daemon connected · " + (status.halted ? "Paused" : status.mode === "live" ? "Live" : "Read only") + ". Verified AutoWheel live P&L history is not recorded yet. Weekly results remain unavailable.";
      } catch (_) { liveMessage = "Live daemon unavailable or stale. Weekly results are unavailable. Switch to Simulation to view historical results."; }
    }
    render();
  }
  try { reports.push(...attachedReports(byID("attached-reports").textContent)); }
  catch (caught) { error("Attached simulations could not be loaded: " + caught.message); }
  populateReports();
  let savedMode;
  try { savedMode = localStorage.getItem("autowheel-calendar-mode"); } catch (_) {}
  setMode(["live", "simulation"].includes(savedMode) ? savedMode : reports.length ? "simulation" : "live");
  loadLive();
})();
