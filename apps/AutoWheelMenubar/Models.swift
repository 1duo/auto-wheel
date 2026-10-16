import Foundation

enum SelectionMode: String, CaseIterable, Decodable {
    case conservative
    case balanced
    var displayLabel: String {
        switch self {
        case .conservative: return "Conservative"
        case .balanced: return "Balanced"
        }
    }
}

struct WheelConfig: Decodable {
    let symbols: [String]
    let max_symbols: Int
    let symbol_limit_scope: String?
    let max_collateral_cents: Int64
    let cash_reserve_cents: Int64
    let fee_per_contract_cents: Int64
    let min_bid_cents: Int64
    let min_open_interest: Int
    let max_spread_bps: Int
    let put_min_delta: Double
    let put_max_delta: Double
    let call_min_delta: Double
    let call_max_delta: Double
    let allow_calls_below_cost: Bool
    let max_quote_age_seconds: Int
    let max_dte_days: Int
    let poll_seconds: Int
    let selection_mode: String?
	let order_coverage_policy: String?

	var restrictedOrders: Bool { order_coverage_policy == "exclusive_standard" }

    // Omitted/empty legacy config means balanced. Unknown raw values stay nil
    // so the selector disables instead of mislabeling.
    var selectionMode: SelectionMode? {
        guard let raw = selection_mode, !raw.isEmpty else { return .balanced }
        return SelectionMode(rawValue: raw)
    }
}

struct WheelPosition: Decodable {
    let put_id: String?
    let put_strike_cents: Int64?
    let owned_shares: Int
    let cost_cents: Int64?
    let missing_since: String?
    let provenance: String?
    var phase: String {
        if missing_since != nil { return "Settlement hold · ≥24h" }
        if owned_shares == 100 { return "100 shares · covered call phase" }
        if put_id != nil { return "Short put phase" }
        return "Awaiting reconciliation"
    }
}

struct WheelCandidate: Decodable {
    let id: String
    let symbol: String
    let kind: String
    let expiry: String
    let strike_cents: Int64
    let bid_cents: Int64
    let delta: Double
    let net_premium_cents: Int64
    let collateral_cents: Int64
    let return_per_day: Double
    let call_away_pnl_cents: Int64?
}

struct WheelIntent: Decodable {
    let ref_id: String
    let order_id: String?
    let candidate: WheelCandidate
    let at: String
}

struct BrokerShares: Decodable {
    let symbol: String
    let quantity: Int
    let available: Int
    let cost_cents: Int64
}
struct BrokerOption: Decodable {
    let symbol: String
    let kind: String
    let quantity: Int
    let strike_cents: Int64
    let expiry: String
}
struct BrokerOrder: Decodable {
    let symbol: String
    let state: String
    var terminal: Bool { ["filled", "cancelled", "canceled", "rejected", "failed", "voided"].contains(state) }
}
struct BrokerSnapshot: Decodable {
    let at: String
    let available_cash_cents: Int64
    let shares: [BrokerShares]?
    let options: [BrokerOption]?
    let orders: [BrokerOrder]?
    let external_symbols: [String]?
}
struct AccountHolding: Decodable {
    let symbol: String
    let quantity: String
    let available: String
}
struct AccountInfo: Decodable {
    let at: String
    let account_last4: String
    let options_level: String
    let available_cash_cents: Int64
    let holdings: [AccountHolding]?
    let snapshot_complete: Bool
	let execution_snapshot_usable: Bool?
	let coverage_warning: String?
}

struct LiveReadiness: Decodable {
    let at: String
    let ready: Bool
    let reason: String?
    let verified_symbols: [String]?
    let unavailable_symbols: [String: String]?
    let preview: WheelCandidate?
}

// Optional shared-coordinator health. Field names match Go
// internal/sharedclient Status JSON tags exactly (at is RFC3339Nano text).
struct CoordinatorStatus: Decodable {
    let ready: Bool
    let reason: String?
    let account_identity_hash: String
    let policy_hash: String
    let client_release: String
    let at: String
    let generation: Int64
    let permission_epoch: Int64
    let new_risk: Bool
    let management: Bool
    let full_account_reconciled: Bool
    let writer_exclusive: Bool
    let available_cash_cents: Int64
    let committed_capital_cents: Int64
    let reserved_cash_cents: Int64
    let peer_symbols: [String]?
    let blockers: [String]?

    var peerSet: Set<String> { Set(peer_symbols ?? []) }
    func ownerLabel(for symbol: String) -> String { peerSet.contains(symbol) ? "TradePulse" : "external" }
    var healthShort: String { ready ? "Ready" : "Blocked" }
    var peerSummary: String {
        guard let peers = peer_symbols, !peers.isEmpty else { return "None" }
        return peers.sorted().joined(separator: ", ")
    }
    var blockerList: [String] { blockers ?? [] }
}

struct WheelStatus: Decodable {
    let version: String
    let mode: String
    let stage: String
    let reason: String?
    let at: String
    let server_time: String
    let halted: Bool
    let live_capable: Bool?
    let trading_window_open: Bool?
    let live_readiness: LiveReadiness?
    let coordinator: CoordinatorStatus?
    let account_configured: Bool
    let token_present: Bool
    let config: WheelConfig
    let broker: BrokerSnapshot?
    let account_info: AccountInfo?
    let positions: [String: WheelPosition]?
    let intents: [String: WheelIntent]?
    let candidates: [WheelCandidate]?
    let pending_selection_mode: String?

    var pendingSelection: SelectionMode? {
        guard let raw = pending_selection_mode, !raw.isEmpty else { return nil }
        return SelectionMode(rawValue: raw)
    }
    var hasPendingSelection: Bool {
        guard let raw = pending_selection_mode else { return false }
        return !raw.isEmpty
    }

    var activeSymbols: [String]? {
        guard let b = broker else { return nil }
        let managed = Set((positions ?? [:]).keys)
        let accountScope = config.symbol_limit_scope != "managed"
        var active = Set((b.shares ?? []).filter { $0.quantity > 0 && (accountScope || managed.contains($0.symbol)) }.map(\.symbol))
        if accountScope { active.formUnion(b.external_symbols ?? []) }
        active.formUnion((b.options ?? []).map(\.symbol))
        active.formUnion((b.orders ?? []).filter { !$0.terminal }.map(\.symbol))
        active.formUnion((positions ?? [:]).filter { $0.value.owned_shares > 0 || $0.value.put_id != nil || $0.value.missing_since != nil }.map(\.key))
        active.formUnion((intents ?? [:]).keys)
        return active.sorted()
    }
    var title: String {
        if halted { return "Paused" }
        switch stage {
        case "waiting_configuration": return "Setup required"
        case "outside_trading_window": return "Outside trading window"
        case "planned": return mode == "read_only" ? "Read-only scan complete" : "Plan ready"
        case "submitted": return "Order submitted"
        case "blocked": return "Action required"
        case "halted": return "Paused · resume pending"
        default: return stage.replacingOccurrences(of: "_", with: " ").capitalized
        }
    }
}

func money(_ cents: Int64, decimals: Bool = true) -> String {
    let f = NumberFormatter()
    f.numberStyle = .currency
    f.currencyCode = "USD"
    f.locale = Locale(identifier: "en_US")
    f.minimumFractionDigits = decimals ? 2 : 0
    f.maximumFractionDigits = decimals ? 2 : 0
    return f.string(from: NSNumber(value: Double(cents) / 100)) ?? "—"
}
func percent(_ value: Double) -> String { String(format: "%.2f%%", value * 100) }
func deltaText(_ value: Double) -> String { String(format: "%.2f", value) }
func timestamp(_ text: String) -> Date? {
    let f = ISO8601DateFormatter()
    f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let date = f.date(from: text) { return date }
    f.formatOptions = [.withInternetDateTime]
    return f.date(from: text)
}
func age(_ text: String, now: Date = Date()) -> String {
    guard let date = timestamp(text) else { return "time unavailable" }
    let seconds = max(0, Int(now.timeIntervalSince(date)))
    if seconds < 60 { return "\(seconds)s ago" }
    if seconds < 3600 { return "\(seconds / 60)m ago" }
    if seconds < 86400 { return "\(seconds / 3600)h ago" }
    return "\(seconds / 86400)d ago"
}

struct ProfitPreview: Decodable {
    let id: String
    let expires_at: String
    let candidate: ProfitCandidate
    let closing_fee_cents: Int64
    let fee_allowance_cents: Int64
    let minimum_profit_cents: Int64
    let continue_live: Bool
}
struct ProfitCandidate: Decodable {
    let symbol: String
    let expiry: String
    let strike_cents: Int64
    let close_limit_cents: Int64
    let entry_credit_cents: Int64
}
