import AppKit
import SwiftUI

@MainActor
final class WheelModel: ObservableObject {
    @Published var status: WheelStatus?
    @Published var lastResponse: Date?
    @Published var error: String?
    @Published var notice: String?
    @Published var busy = false
    @Published var logs = "Select “Load msi logs” to read the latest 80 daemon log lines."
    @Published var selectedTab = 0
    var changed: (() -> Void)?
    private var timer: Timer?
    private var fetching = false
    private let api = OperatorAPI()

    var connected: Bool {
        guard error == nil, let at = lastResponse else { return false }
        return Date().timeIntervalSince(at) < 90
    }
    var canEnableLive: Bool {
        guard connected, status?.hasPendingSelection != true, status?.live_capable == true, let check = status?.live_readiness, check.ready,
              let at = timestamp(check.at), Date().timeIntervalSince(at) < 1200,
              let broker = status?.broker, let brokerAt = timestamp(broker.at), Date().timeIntervalSince(brokerAt) < 600 else { return false }
        return (status?.intents ?? [:]).isEmpty
    }
    var readOnly: Bool { status?.mode == "read_only" }
    var trayColor: NSColor {
        if !connected { return error == nil && lastResponse == nil ? .systemGray : .systemRed }
        if status?.halted == true || ["blocked", "waiting_configuration"].contains(status?.stage ?? "") { return .systemYellow }
        if status?.stage == "outside_trading_window" || readOnly { return NSColor(srgbRed: 0.18, green: 0.76, blue: 1, alpha: 1) }
        return .systemGreen
    }
    var color: Color { Color(nsColor: trayColor) }
    var title: String {
        if !connected { return lastResponse == nil ? (error == nil ? "Checking · Linux msi" : "Unavailable · Linux msi") : "Stale · Linux msi" }
        if status?.hasPendingSelection == true { return "Restart required · selection pending" }
        if status?.stage == "blocked" { return status?.halted == true ? "Paused · action required" : "Attention · action required" }
        if status?.halted == true { return status?.live_readiness?.ready == true ? "Ready · live trading paused" : "Paused · checking readiness" }
        if ["blocked", "waiting_configuration"].contains(status?.stage ?? "") { return "Attention · \(status?.title ?? "Setup required")" }
        if status?.stage == "outside_trading_window" { return "Off market · standby" }
        return readOnly ? "Read only · scans enabled" : "Live · entries enabled"
    }
    func start() {
        Task { await refresh() }
        timer = Timer.scheduledTimer(withTimeInterval: 30, repeats: true) { [weak self] _ in
            Task { @MainActor in await self?.refresh() }
        }
    }
    func stop() { timer?.invalidate(); timer = nil }
    func refresh() async {
        guard !fetching else { return }
        fetching = true
        defer { fetching = false; changed?() }
        do {
            let next = try await api.status()
            guard let serverTime = timestamp(next.server_time), abs(serverTime.timeIntervalSinceNow) < 120 else {
                throw OperatorError.message("Daemon clock is stale or differs from this Mac")
            }
            status = next
            lastResponse = Date()
            error = nil
        } catch { self.error = error.localizedDescription }
    }
    func control(_ action: OperatorAction) async {
        guard connected, !busy, action == .halt || action == .live || readOnly else { return }
        busy = true; notice = nil
        defer { busy = false; changed?() }
        do {
            try await api.control(action)
            switch action {
            case .halt: notice = "Pause saved. New submissions are fenced; accepted broker orders continue."
            case .resume: notice = "Read-only scanning resumed. Live trading remains disabled."
            case .live: notice = "Live trading enabled. Orders wait for market hours and fresh safety checks."
            case .refresh: notice = "Read-only scan queued. Market hours and setup checks still apply."
            }
            await refresh()
        } catch { notice = "Control outcome unconfirmed: \(error.localizedDescription). Refresh status before retrying." }
    }
    func takeProfit(_ symbol: String) async {
        guard connected, !busy, status?.config.fixed_policy == nil, canEnableLive, status?.halted == true || status?.mode == "live" else { return }
        busy = true; notice = nil
        defer { busy = false; changed?() }
        do {
            let preview = try await api.profitPreview(symbol)
            try await api.takeProfit(preview.id)
            notice = "Close submitted for \(symbol), capped at \(money(preview.candidate.close_limit_cents)) per share. " + (preview.continue_live ? "Live session continues after broker reconciliation." : "Entries remain paused.")
        } catch {
            notice = "Close not confirmed: \(error.localizedDescription). Refresh positions and intents before any further action; no request was repeated."
        }
        await refresh()
    }
    func selectMode(_ mode: SelectionMode) async {
        guard connected, !busy, status?.config.fixed_policy == nil, status?.config.selectionMode != nil else { return }
        busy = true; notice = nil
        defer { busy = false; changed?() }
        do {
            try await api.selection(mode)
            _ = try await MSIService.run(.restart)
            let restartedAt = Date()
            var applied = false
            for _ in 0..<6 {
                try? await Task.sleep(for: .milliseconds(1200))
                await refresh()
                if connected, let responseAt = lastResponse, responseAt > restartedAt,
                   status?.config.selectionMode == mode, status?.hasPendingSelection != true,
                   status?.mode == "read_only", status?.halted == true {
                    applied = true
                    break
                }
            }
            if applied, let s = status, s.mode == "read_only", s.halted {
                notice = "Selection saved: \(mode.displayLabel). Paused · restart applied · fresh readiness required for live activation."
            } else if status?.hasPendingSelection == true {
                notice = "Selection saved: \(mode.displayLabel). Restart msi service and refresh to load it; trading stays paused."
            } else {
                notice = "Selection outcome unconfirmed: restarted but loaded mode not yet verified. Refresh status; restart msi service if selection remains pending."
            }
            await refresh()
        } catch {
            notice = "Selection outcome unconfirmed: \(error.localizedDescription). Refresh status; restart msi service if selection remains pending. The selection request was not repeated."
            await refresh()
        }
    }
    func service(_ action: ServiceAction) async {
        guard !busy else { return }
        if [.start, .restart].contains(action), status?.mode == "live" && status?.halted == false { return }
        busy = true; notice = nil
        defer { busy = false; changed?() }
        do {
            let output = try await MSIService.run(action)
            if action == .logs {
                logs = output.isEmpty ? "No log lines returned." : output
            } else {
                notice = "msi service: \(action.rawValue) completed."
                try? await Task.sleep(for: .milliseconds(600))
                await refresh()
            }
        } catch { notice = "msi command outcome unconfirmed: \(error.localizedDescription)" }
    }
    func openCalendar() async {
        if let url = Bundle.main.url(forResource: "calendar", withExtension: "html") {
            NSWorkspace.shared.open(url)
        } else if await api.calendarAvailable() {
            NSWorkspace.shared.open(api.calendarURL)
        } else {
            notice = "Calendar unavailable. Reinstall the menubar app to include its offline calendar."
        }
    }
    func openGuide() {
        if status?.config.fixed_policy != nil {
            selectedTab = 1
            changed?()
            return
        }
        let path = Bundle.main.path(forResource: "guide", ofType: "html")
        if let path { NSWorkspace.shared.open(URL(fileURLWithPath: path)) }
    }
}
