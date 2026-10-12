import SwiftUI
import AppKit

private enum MenuFont {
    static let title = Font.system(size: 13, weight: .semibold)
    static let action = Font.system(size: 12, weight: .medium)
    static let rowLabel = Font.system(size: 12)
    static let rowValue = Font.system(size: 12, weight: .medium)
    static let sectionHeader = Font.system(size: 11, weight: .semibold)
    static let detail = Font.system(size: 11)
    static let detailEmphasis = Font.system(size: 11, weight: .medium)
    static let detailMono = Font.system(size: 11, weight: .medium, design: .monospaced)
}

struct WheelView: View {
    @ObservedObject var model: WheelModel
    @State private var showServiceControls = false

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 8) {
                Circle().fill(model.color).frame(width: 9, height: 9)
                Text(model.title).font(MenuFont.title)
                Spacer()
                if model.busy { ProgressView().controlSize(.small) }
            }.padding(.horizontal, 14).padding(.vertical, 10)
            menuDivider()
            if model.selectedTab == 0 {
                ScrollView {
                    VStack(alignment: .leading, spacing: 0) {
                        overview
                    }
                }.frame(height: 430)
            } else {
                actionRow("Back to AutoWheel", symbol: "chevron.left") { model.selectedTab = 0 }
                menuDivider()
                ScrollView {
                    VStack(alignment: .leading, spacing: 12) {
                        if model.selectedTab == 1 { strategy }
                        else if model.selectedTab == 2 { activity }
                        else { brokerDetails }
                    }.padding(.vertical, 8)
                }.frame(height: 430)
            }
            if let error = model.error {
                menuNotice(error, symbol: "xmark.circle", color: .red)
            }
            if let notice = model.notice {
                menuNotice(notice, symbol: "info.circle", color: model.color)
            }
            menuDivider()
            Button { Task { await model.refresh() } } label: {
                HStack {
                    Label("Refresh Status", systemImage: "sparkle").font(MenuFont.action)
                    Spacer()
                    TimelineView(.periodic(from: .now, by: 1)) { context in
                        Text(model.lastResponse.map { age($0.ISO8601Format(), now: context.date) } ?? "No response")
                            .font(MenuFont.detail).monospacedDigit()
                            .foregroundStyle(model.connected ? Color.secondary : .red)
                    }
                }.padding(.horizontal, 14).padding(.vertical, 7)
            }.buttonStyle(.plain).disabled(model.busy)
            .help("Refresh displayed status; does not place orders or run a broker scan")
            if model.selectedTab == 0 {
                actionRow("Strategy", symbol: "arrow.triangle.2.circlepath") { model.selectedTab = 1 }
                actionRow("Positions and Readiness", symbol: "list.bullet.rectangle") { model.selectedTab = 3 }
                actionRow("Activity and Logs", symbol: "text.alignleft") { model.selectedTab = 2 }
            }
            actionRow("Open Weekly P&L Calendar", symbol: "calendar") { Task { await model.openCalendar() } }
                .help("View weekly results or load saved historical simulation reports")
            actionRow("Open Strategy Guide", symbol: "book") { model.openGuide() }
            menuDivider()
            actionRow("Quit AutoWheel", symbol: "power") { NSApp.terminate(nil) }
                .help("Quit menubar; the msi daemon continues running")
        }.frame(width: 370)
    }

    @ViewBuilder private var overview: some View {
        if let s = model.status {
            HStack(spacing: 8) {
                Label("Live Trading", systemImage: s.mode == "live" && !s.halted ? "bolt.circle" : "bolt.slash.circle")
                    .font(MenuFont.action)
                Spacer(minLength: 8)
                Toggle("", isOn: Binding(
                    get: { s.mode == "live" && !s.halted },
                    set: { want in Task { await model.control(want ? .live : .halt) } }
                )).labelsHidden().toggleStyle(.switch).controlSize(.mini)
                    .disabled(!model.connected || model.busy || (s.mode != "live" && !model.canEnableLive))
                    .help("Enable automated real orders under the configured limits. Switching off persists a pause; accepted orders continue.")
            }.padding(.horizontal, 14).padding(.vertical, 8)
            if let currentSelection = s.config.selectionMode {
                HStack(spacing: 8) {
                    Label("Selection", systemImage: "arrow.triangle.2.circlepath")
                        .font(MenuFont.action)
                    Spacer(minLength: 8)
                    Picker("Selection", selection: Binding(
                        get: { s.config.selectionMode ?? currentSelection },
                        set: { want in Task { await model.selectMode(want) } }
                    )) {
                        ForEach(SelectionMode.allCases, id: \.self) { mode in
                            Text(mode.displayLabel).tag(mode)
                        }
                    }
                    .labelsHidden()
                    .pickerStyle(.menu)
                    .controlSize(.small)
                    .disabled(!model.connected || model.busy)
                    .help("Changing selection pauses trading and restarts the daemon; it does not enable live trading.")
                }.padding(.horizontal, 14).padding(.vertical, 4)
            } else {
                row("Selection", "Unknown")
            }
            detail("Selection changes pause trading and restart the msi service to load. Live activation still needs fresh readiness.")
            if s.hasPendingSelection {
                menuNotice("Selection \(s.pending_selection_mode ?? "change") pending · restart msi service, then refresh to verify. Trading stays paused.", symbol: "exclamationmark.triangle", color: .yellow)
            }
            menuDivider()
            row("Target", "Linux msi · Robinhood MCP")
            row("Execution", s.mode == "live" ? "Live" : "Read only · orders disabled")
            row("Live Readiness", s.live_readiness.map { $0.ready ? (s.config.restrictedOrders ? "Passed · restricted standard orders" : "Passed · one-click activation") : "Checking / blocked" } ?? "Checking")
            if s.config.restrictedOrders {
                menuNotice("Advanced/OCO visibility unknown. Keep this account exclusive to AutoWheel; no manual or contingent orders.", symbol: "exclamationmark.triangle", color: .yellow)
            }
            row("Shared Coordinator", s.coordinator.map { $0.healthShort } ?? "Not configured")
            row("Order Window", s.trading_window_open == true ? "Open · freshness checks apply" : "Closed · standby")
            row(s.config.symbol_limit_scope == "managed" ? "Managed Symbols" : "Active Symbols", s.activeSymbols.map { "\($0.count) / \(s.config.max_symbols)" } ?? "— / \(s.config.max_symbols)")
            menuDivider()
            sectionHeader("Account")
            if let info = s.account_info {
                row("Agentic Account", "••••\(info.account_last4)")
                row("Broker Read", age(info.at))
            }
            row("Capital Ceiling", money(s.config.max_collateral_cents, decimals: false))
            row("Available Cash · last known", s.broker.map { money($0.available_cash_cents) } ?? s.account_info.map { money($0.available_cash_cents) } ?? "Unavailable")
            row("Cash Reserve", money(s.config.cash_reserve_cents, decimals: false))
            if let coord = s.coordinator {
                row("Executable Shared Cash", money(coord.available_cash_cents))
                row("Shared Holds", money(coord.reserved_cash_cents))
                row("Managed Capital", money(coord.committed_capital_cents))
                row("Peer Exclusions", coord.peerSummary)
                row("Coordinator Observed", age(coord.at))
            } else {
                detail("Shared execution is not configured.")
            }
            menuDivider()
            sectionHeader("Wheel Positions")
            if (s.positions ?? [:]).isEmpty {
                detail("No AutoWheel-owned positions recorded")
            } else {
                ForEach((s.positions ?? [:]).keys.sorted(), id: \.self) { symbol in
                    if let p = s.positions?[symbol] {
                        HStack(spacing: 8) {
                            Text(symbol).font(MenuFont.detailMono)
                            Text(p.phase).font(MenuFont.detail).foregroundStyle(.secondary)
                            Spacer()
                        }.padding(.horizontal, 14).padding(.vertical, 4)
                    }
                }
            }
            if let holdings = s.account_info?.holdings, !holdings.isEmpty, (s.positions ?? [:]).isEmpty {
                ForEach(holdings.indices, id: \.self) { index in
                    row(holdings[index].symbol, "\(holdings[index].quantity) shares · \(s.coordinator?.ownerLabel(for: holdings[index].symbol) ?? "external")")
                }
            }
            if !(s.intents ?? [:]).isEmpty {
                menuNotice("Unresolved submission · new orders blocked", symbol: "exclamationmark.circle", color: .yellow)
            }
            if let reason = s.reason, !reason.isEmpty {
                menuNotice(reason, symbol: "info.circle", color: .secondary)
            }
            if let coord = s.coordinator {
                ForEach(coord.blockerList, id: \.self) { blocker in
                    menuNotice(blocker, symbol: "exclamationmark.triangle", color: .yellow)
                }
            }
            menuDivider()
            DisclosureGroup("Service Controls", isExpanded: $showServiceControls) {
                serviceButtons
                if model.readOnly {
                    actionRow("Scan Now", symbol: "magnifyingglass") { Task { await model.control(.refresh) } }
                        .disabled(!model.connected || model.busy || s.halted)
                }
                detail("Use Live Trading above to activate. Pause does not cancel accepted broker orders.")
            }.font(MenuFont.action).padding(.horizontal, 14).padding(.vertical, 7)
        } else {
            row("Target", "Linux msi · Robinhood MCP")
            row("Execution", "Unverified")
            detail("Waiting for the daemon. Service controls use SSH directly.")
            menuDivider()
            DisclosureGroup("Service Controls", isExpanded: $showServiceControls) { serviceButtons }
                .font(MenuFont.action).padding(.horizontal, 14).padding(.vertical, 7)
        }
    }

    @ViewBuilder private var brokerDetails: some View {
        if let s = model.status {
            if s.mode == "read_only" {
                banner("Orders disabled", "This daemon can inspect and plan. It cannot place orders in read-only mode.", color: .blue)
            } else {
                banner("Live trading enabled", "Every order rechecks fresh quotes, account cash, positions and broker review. Turn off Live Trading to pause.", color: .red)
            }
            if let reason = s.reason, !reason.isEmpty { banner("Daemon reason", reason, color: model.color) }
            if s.config.restrictedOrders {
                banner("Restricted standard orders", s.account_info?.coverage_warning ?? "Advanced/OCO visibility is unknown. Keep this account exclusive to AutoWheel; no manual, contingent or other-strategy orders. Observed outside activity blocks execution.", color: .yellow)
            }
            section("Readiness") {
                row("Agentic account configured", s.account_configured ? "Yes" : "Required")
                row("Private OAuth token file", s.token_present ? "Present" : "Missing")
                if let info = s.account_info {
                    row("Agentic account", "••••\(info.account_last4)")
                    row("Authenticated broker read", age(info.at))
                    row("Complete account snapshot", info.snapshot_complete ? "Yes" : (s.config.restrictedOrders ? "No · advanced/OCO unknown" : "Blocked · partial display facts"))
                    if s.config.restrictedOrders {
                        row("Standard execution facts", info.execution_snapshot_usable == true ? "Available · restricted policy" : "Blocked")
                    }
                }
                row("Live capability", s.live_capable == true ? "Installed" : "Disabled")
                row("Symbol limit scope", s.config.symbol_limit_scope == "managed" ? "AutoWheel only · external symbols quarantined" : "Whole account")
                if let coord = s.coordinator {
                    row("Shared coordinator", coord.healthShort)
                    row("Coordinator observed", age(coord.at))
                    row("Ledger generation / epoch", "\(coord.generation) / \(coord.permission_epoch)")
                    row("Shared pool", money(coord.available_cash_cents))
                    row("Shared holds", money(coord.reserved_cash_cents))
                    row("Committed capital", money(coord.committed_capital_cents))
                    row("Peer exclusions", coord.peerSummary)
                    row("Full reconcile", coord.full_account_reconciled ? "Yes" : "No")
                    row("Writer", coord.writer_exclusive ? "Verified exclusive" : "Not qualified")
                    if coord.blockerList.isEmpty, let reason = coord.reason, !reason.isEmpty {
                        detail("Coordinator: \(reason)")
                    }
                    ForEach(coord.blockerList, id: \.self) { blocker in
                        detail("Coordinator blocked: \(blocker)")
                    }
                } else {
                    detail("Shared execution is not configured.")
                }
                if let checks = s.live_readiness {
                    row("Activation checks", checks.ready ? "Passed" : "Blocked")
                    row("Checked", age(checks.at))
                    row("Verified symbols", checks.verified_symbols?.joined(separator: ", ").nonEmpty ?? "None")
                    if let reason = checks.reason { detail(reason) }
                    if let preview = checks.preview {
                        detail("Non-trading preview · last available quote; reselected with fresh data before any live order")
                        candidateView(preview)
                    }
                    ForEach((checks.unavailable_symbols ?? [:]).keys.sorted(), id: \.self) { symbol in
                        detail("\(symbol): \(checks.unavailable_symbols?[symbol] ?? "Unavailable")")
                    }
                }
                row("Pause marker", s.halted ? "HALT present" : "Absent")
                row("Last cycle", age(s.at))
                row("Release", s.version)
            }
            section("Broker data · last known") {
                if let broker = s.broker {
                    row("Available non-margin cash", money(broker.available_cash_cents))
                    row("Broker snapshot", age(broker.at))
                    row("Active symbols", s.activeSymbols?.joined(separator: ", ").nonEmpty ?? "None in snapshot")
                    row("Open broker orders", "\((broker.orders ?? []).filter { !$0.terminal }.count)")
                    ForEach((broker.shares ?? []).indices, id: \.self) { index in
                        let held = (broker.shares ?? [])[index]
                        row("\(held.symbol) · broker shares", "\(held.quantity) total / \(held.available) available")
                        if held.quantity > 0 { row("Purchase cost / share", money(held.cost_cents)) }
                    }
                    ForEach((broker.options ?? []).indices, id: \.self) { index in
                        let p = (broker.options ?? [])[index]
                        row("\(p.symbol) · short \(p.kind)", "\(p.quantity) × \(money(p.strike_cents)) · \(p.expiry)")
                    }
                    detail("These are snapshot values, not streaming balances. New orders recheck cash and holdings.").font(MenuFont.detail).foregroundStyle(.secondary)
                } else {
                    detail("Unavailable until account setup, authentication, and a successful in-window scan.")
                        .font(MenuFont.rowLabel).foregroundStyle(.secondary)
                }
            }
            section("Wheel positions · durable journal") {
                if (s.positions ?? [:]).isEmpty {
                    detail("No AutoWheel-owned positions recorded.").font(MenuFont.rowLabel).foregroundStyle(.secondary)
                }
                ForEach((s.positions ?? [:]).keys.sorted(), id: \.self) { symbol in
                    if let p = s.positions?[symbol] {
                        VStack(alignment: .leading, spacing: 4) {
                            row(symbol, p.phase)
                            if let cost = p.cost_cents { row("Purchase cost / share", money(cost)) }
                            if let strike = p.put_strike_cents { row("Put strike", money(strike)) }
                            if let provenance = p.provenance { detail(provenance).font(MenuFont.detail).foregroundStyle(.secondary).textSelection(.enabled) }
                        }
                    }
                }
            }
            section("Planned orders · last cycle") {
                if (s.candidates ?? []).isEmpty { detail("No eligible orders in the last plan.").font(MenuFont.rowLabel).foregroundStyle(.secondary) }
                if s.halted { detail("Paused plans will not be submitted.").font(MenuFont.detail).foregroundStyle(.secondary) }
                ForEach(s.candidates ?? [], id: \.id) { candidate in candidateView(candidate) }
            }
            section("Controls") {
                serviceButtons
                detail("Pause survives restarts. It does not cancel an accepted broker order. The Live Trading switch enables orders only after readiness passes. Neither activation nor resume clears unresolved submissions.")
                    .font(MenuFont.detail).foregroundStyle(.secondary)
            }
        } else {
            banner("Waiting for msi", "The dedicated SSH tunnel connects this Mac to the daemon’s loopback API. No account data is cached on disk.", color: .orange)
            serviceButtons
            actionRow("Open Strategy Guide", symbol: "book") { model.openGuide() }
        }
    }

    @ViewBuilder private var strategy: some View {
        if let s = model.status, let c = model.status?.config {
            if s.hasPendingSelection {
                menuNotice("Selection \(s.pending_selection_mode ?? "change") pending · restart msi service, then refresh to verify. Trading stays paused.", symbol: "exclamationmark.triangle", color: .yellow)
            }
            section("Selection mode · active") {
                row("Active mode", c.selectionMode?.displayLabel ?? "Unknown")
                detail("Conservative puts: absolute delta 0.05–0.15, lowest absolute delta first, then premium yield. Balanced puts: absolute delta 0.15–0.35, highest net premium ÷ collateral ÷ calendar day. Same priority across symbols.")
                detail("Both modes use the nearest weekly expiry 1–8 calendar days away, no same-day expiry. If the nearest expiry has no eligible contract, skip the symbol; liquidity and fresh-quote checks still apply.")
            }
            banner("Capital release comes first", "Calls favor the earliest expiry and highest delta. Time-value ROI breaks equal-delta ties. This is not a search for the highest call premium.", color: .blue)
            section("1 · Cash → cash-secured put") {
                detail("Pick the nearest eligible Friday expiry, 1–\(c.max_dte_days) calendar days away. A Thursday explicitly marked weekly is eligible. No same-day expiry.")
                row("Put absolute delta", "\(deltaText(c.put_min_delta))–\(deltaText(c.put_max_delta))")
                detail("Conservative 0.05–0.15 prefers the lowest absolute delta first, then premium yield. Balanced 0.15–0.35 ranks by net bid premium ÷ strike collateral ÷ calendar days. Strike must be below spot. Choose one contract per symbol.")
            }
            section("2 · Assignment → shares") {
                detail("When the own put disappears, hold the slot for at least 24 hours. Exactly 100 available shares at the put strike, with no intervening equity order, support inferred assignment. Ambiguous ownership blocks new orders. Conservative mode cannot guarantee avoiding assignment.")
            }
            section("3 · Shares → aggressive covered call") {
                detail("Sell against 100 AutoWheel-owned shares. Use the earliest eligible daily or weekly expiry tomorrow through \(c.max_dte_days) days away. Calls are unchanged in both modes.")
                row("Call delta", "\(deltaText(c.call_min_delta))–\(deltaText(c.call_max_delta))")
                row("Below purchase cost", c.allow_calls_below_cost ? "Allowed · loss on call-away possible" : "Blocked")
                detail("Highest delta wins within that expiry; time-value yield breaks ties. Calls get account allocation priority over new puts to release capital. After shares disappear, wait at least 24 hours before releasing the slot.")
            }
            section("Limits and execution") {
                row("Cash reserve", money(c.cash_reserve_cents, decimals: false))
                row("Estimated fee / contract", money(c.fee_per_contract_cents))
                row("Cycle interval", "\(c.poll_seconds / 60) minutes")
                row("Order window · New York", "09:35–15:45 · early close 12:45")
                detail("Reviewed exchange sessions for 2026–2028. Holidays and dates outside coverage block orders; fresh quotes remain required.")
                detail("At most \(c.max_symbols) " + (c.symbol_limit_scope == "managed" ? "AutoWheel-managed" : "account-wide active") + " symbols. Scans use up to three workers; orders are serialized, at most one new submission per cycle. Day limit at bid, rounded down to the broker tick.")
            }
            section("Quote and liquidity checks") {
                row("Maximum quote age", "\(c.max_quote_age_seconds) seconds")
                row("Minimum bid / open interest", "\(money(c.min_bid_cents)) / \(c.min_open_interest)")
                row("Maximum spread / ask", percent(Double(c.max_spread_bps) / 10000))
                detail("Positive net premium, bid size ≥1, standard 100-share deliverables, tradable contracts. If the earliest expiry fails, skip the symbol rather than moving to a later week.")
            }
            section("Symbol universe") { detail(c.symbols.joined(separator: " · ")).font(MenuFont.detailEmphasis) }
            section("Illustration · not current quotes") {
                detail("A $200 put sold at $2 with 4 days left reserves $20,000 and nets $199 after the $1 estimate: 0.995% over 4 days. If assigned at $200, a $195 call sold at $2 would produce a $301 loss on call-away, excluding the earlier put premium.")
            }
            section("Current scope") {
                detail("No rolling, automatic repricing, buybacks, stop loss, earnings filter, or 0DTE. Delta is a ranking input, not a guaranteed assignment probability. The 24-hour handoff can delay capital reuse. A price decline can exceed all premiums collected.")
            }
        } else {
            detail("Connect to msi to see the exact runtime configuration.")
            actionRow("Open Strategy Guide", symbol: "book") { model.openGuide() }
        }
    }

    @ViewBuilder private var activity: some View {
        if let s = model.status {
            section("Unresolved submissions") {
                if (s.intents ?? [:]).isEmpty { detail("None recorded in the journal.").foregroundStyle(.secondary) }
                ForEach((s.intents ?? [:]).keys.sorted(), id: \.self) { symbol in
                    if let intent = s.intents?[symbol] {
                        row(symbol, "\(intent.candidate.kind) · \(age(intent.at))")
                        detail("ref_id: \(intent.ref_id)\nBroker order: \(intent.order_id ?? "receipt unavailable")")
                            .font(MenuFont.detailMono).textSelection(.enabled)
                    }
                }
                detail("An unresolved submission blocks all new orders. The menubar cannot clear it or retry it.").font(MenuFont.detail).foregroundStyle(.secondary)
            }
        }
        HStack {
            Text("msi daemon logs").font(MenuFont.sectionHeader)
            Spacer()
            Button("Load msi logs") { Task { await model.service(.logs) } }.disabled(model.busy)
        }.padding(.horizontal, 14)
        Text(model.logs).id(model.logs).font(MenuFont.detailMono).textSelection(.enabled)
            .frame(maxWidth: .infinity, alignment: .leading).padding(.horizontal, 14)
    }
    private var serviceButtons: some View {
        VStack(alignment: .leading, spacing: 0) {
            actionRow("Start msi Service", symbol: "play") { Task { await model.service(.start) } }
                .disabled(model.busy || (model.status?.mode == "live" && model.status?.halted == false))
            actionRow("Restart msi Service", symbol: "arrow.clockwise") { Task { await model.service(.restart) } }
                .disabled(model.busy || (model.status?.mode == "live" && model.status?.halted == false))
            actionRow("Stop msi Service", symbol: "stop") { Task { await model.service(.stop) } }.disabled(model.busy)
        }
    }
    private func candidateView(_ a: WheelCandidate) -> some View {
        VStack(alignment: .leading, spacing: 5) {
            row("\(a.symbol) · \(a.kind.uppercased())", "\(money(a.strike_cents)) · \(a.expiry)")
            row("Limit / net credit", "\(money(a.bid_cents)) / \(money(a.net_premium_cents))")
            row(a.kind == "put" ? "Collateral / yield per day" : "Delta / time-value yield per day", a.kind == "put" ? "\(money(a.collateral_cents, decimals: false)) / \(percent(a.return_per_day))" : "\(deltaText(a.delta)) / \(percent(a.return_per_day))")
            if a.kind == "call" {
                row("Call-away P&L · this call + shares", money(a.call_away_pnl_cents ?? 0))
                detail("Excludes earlier premiums and taxes.").font(MenuFont.detail).foregroundStyle(.secondary)
            }
        }.padding(.vertical, 4)
    }
    private func row(_ label: String, _ value: String) -> some View {
        HStack(alignment: .top) {
            Text(label).font(MenuFont.rowLabel).foregroundStyle(.secondary)
            Spacer(minLength: 8)
            Text(value).id(value).font(MenuFont.rowValue).monospacedDigit()
                .multilineTextAlignment(.trailing).fixedSize(horizontal: false, vertical: true)
        }.padding(.horizontal, 14).padding(.vertical, 4)
    }
    private func menuDivider() -> some View { Divider().padding(.vertical, 3) }
    private func sectionHeader(_ title: String) -> some View {
        Text(title).font(MenuFont.sectionHeader).padding(.horizontal, 14).padding(.vertical, 4)
    }
    private func detail(_ text: String) -> some View {
        Text(text).id(text).font(MenuFont.detail).foregroundStyle(.secondary)
            .fixedSize(horizontal: false, vertical: true).padding(.horizontal, 14).padding(.vertical, 4)
    }
    private func menuNotice(_ text: String, symbol: String, color: Color) -> some View {
        Label(text, systemImage: symbol).id(text).font(MenuFont.detail).foregroundStyle(color)
            .fixedSize(horizontal: false, vertical: true).padding(.horizontal, 14).padding(.vertical, 6)
    }
    private func actionRow(_ title: String, symbol: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            HStack {
                Label(title, systemImage: symbol).font(MenuFont.action)
                Spacer()
            }.contentShape(Rectangle()).padding(.horizontal, 14).padding(.vertical, 7)
        }.buttonStyle(.plain)
    }
    private func banner(_ title: String, _ text: String, color: Color) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            sectionHeader(title).foregroundStyle(color)
            detail(text)
        }
    }
    private func section<Content: View>(_ title: String, @ViewBuilder content: () -> Content) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            sectionHeader(title)
            content().font(MenuFont.rowLabel).fixedSize(horizontal: false, vertical: true)
        }.frame(maxWidth: .infinity, alignment: .leading)
    }
}
private extension String { var nonEmpty: String? { isEmpty ? nil : self } }
