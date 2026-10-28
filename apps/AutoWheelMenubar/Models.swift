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
    let fixed_policy: FixedPolicyDescriptor?
	let order_coverage_policy: String?

	var restrictedOrders: Bool { order_coverage_policy == "exclusive_standard" }

    // Omitted/empty legacy config means balanced. Unknown raw values stay nil
    // so the selector disables instead of mislabeling.
    var selectionMode: SelectionMode? {
        if let fixed_policy {
            return fixed_policy.isConservative ? .conservative : nil
        }
        guard let raw = selection_mode, !raw.isEmpty else { return .balanced }
        return SelectionMode(rawValue: raw)
    }
}

struct FixedPolicyDescriptor: Decodable {
    struct Spec: Decodable { let id: String }
    let spec: Spec
    var usesTP75: Bool { spec.id == "conservative-rd1-floor-tp75-v1" || usesRestingTP75 }
    var usesRestingTP75: Bool { spec.id == "conservative-rd1-floor-tp75-resting-v2" || replenishesASAP }
    var replenishesASAP: Bool { spec.id == "conservative-rd1-floor-tp75-asap-v3" }
    var isConservative: Bool { spec.id == "conservative-rd1-floor-hold-v1" || usesTP75 }
    var label: String {
        switch spec.id {
        case "conservative-rd1-floor-hold-v1": return "Conservative · floor + hold"
        case "conservative-rd1-floor-tp75-v1": return "Conservative · floor + TP75"
        case "conservative-rd1-floor-tp75-resting-v2": return "Conservative · floor + resting TP75"
        case "conservative-rd1-floor-tp75-asap-v3": return "Conservative · five slots + TP75 + ASAP"
        default: return "Unknown fixed policy"
        }
    }
    var scheduleLabel: String {
        guard isConservative else { return "Unknown schedule" }
        if replenishesASAP { return "Resting TP75 · funded slot replenishment" }
        if usesRestingTP75 { return "Resting TP75 · weekly openings" }
        return usesTP75 ? "Daily TP75 · weekly openings" : "Weekly openings"
    }
    var readinessDetail: String {
        guard isConservative else { return "Unknown policy. Native readiness and release binding are required." }
        if usesRestingTP75 {
            return "Resting targets and new openings require fresh market data, current account checks and authenticated native activation."
        }
        return usesTP75
            ? "Integration verified. Daily profit checks and weekly openings still require their scheduled signals, fresh market data and current account checks."
            : "Integration verified. Openings still require their weekly signal, fresh market data and current account checks."
    }
}

struct WheelPosition: Decodable {
    let put_id: String?
    let call_id: String?
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
    let closing: Bool?
    let close_limit_cents: Int64?
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
    let id: String?
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
    let side: String?
    let security_type: String?
    let ownership: String?
    let currency: String?
}
struct AccountInfo: Decodable {
    let at: String
    let account_last4: String
    let options_level: String
    let available_cash_cents: Int64?
    let reported_funds: String?
    let holdings: [AccountHolding]?
    let snapshot_complete: Bool
	let execution_snapshot_usable: Bool?
	let coverage_warning: String?
    let balance_label: String?
    let holdings_source_time_known: Bool?
    let holdings_scope: String?
    let holdings_complete: Bool?

    var balanceText: String {
        if let reported_funds { return "USD \(reported_funds)" }
        return available_cash_cents.map { money($0) } ?? "Unavailable"
    }
}

struct LiveReadiness: Decodable {
    let at: String
    let ready: Bool
    let scope: String?
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
    let broker_id: String?
    let deployment_id: String?
    let execution_policy_id: String?
    let version: String
    let mode: String
    let stage: String
    let reason: String?
    let at: String
    let server_time: String
    let halted: Bool
    let live_capable: Bool?
    let execution_installed: Bool?
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

    var executionTargetLabel: String? {
        guard let execution_policy_id, !execution_policy_id.isEmpty else { return nil }
        return execution_policy_id == "conservative-rd1-floor-tp75-asap-v3"
            ? "Conservative · five slots + TP75 + ASAP" : "Unknown execution target"
    }

    func canActivate(deployment: BrokerDeployment, now: Date = Date()) -> Bool {
        guard deployment.acceptsStatus(self), !hasPendingSelection, live_capable == true,
              let check = live_readiness, check.ready,
              timestampFresh(check.at, maxAge: deployment == .moomoo ? 30 : 1200, now: now),
              let broker, timestampFresh(broker.at, maxAge: deployment == .moomoo ? 30 : 600, now: now),
              (intents ?? [:]).isEmpty else { return false }
        if deployment == .moomoo {
            return execution_installed == true && config.fixed_policy?.replenishesASAP == true &&
                config.order_coverage_policy == "moomoo_complete"
        }
        return true
    }

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

func holdingsFresh(_ text: String, connected: Bool, now: Date = Date()) -> Bool {
    connected && timestampFresh(text, maxAge: 600, now: now)
}

func timestampFresh(_ text: String, maxAge: TimeInterval, now: Date = Date()) -> Bool {
    guard let at = timestamp(text) else { return false }
    let seconds = now.timeIntervalSince(at)
    return seconds >= -2 && seconds < maxAge
}

struct HoldingRow: Identifiable {
    let id: String
    let symbol: String
    let detail: String
    let at: String
}

struct HoldingsPresentation {
    var rows: [HoldingRow] = []
    var emptyMessage = "Holdings unavailable · awaiting broker read"
    var warning: String?
}

func holdingsPresentation(_ s: WheelStatus?, deployment: BrokerDeployment) -> HoldingsPresentation {
    guard let s else { return HoldingsPresentation() }
    var result = HoldingsPresentation()
    if let info = s.account_info, let holdings = info.holdings {
        result.rows += holdings.enumerated().map { index, h in
            let owned = s.positions?[h.symbol]?.owned_shares ?? 0
            let owner = h.ownership == "external_unclaimed" ? "unclaimed" : (owned > 0 ? "AutoWheel records \(owned) owned shares" : "external / unclaimed")
            let unit = h.security_type ?? (deployment == .robinhood ? "shares" : "units")
            let side = h.side.map { "\($0.lowercased()) · " } ?? ""
            return HoldingRow(id: "account-\(index)-\(h.symbol)", symbol: h.symbol,
                detail: "\(h.quantity) \(unit.lowercased()) · \(side)\(owner)", at: info.at)
        }
    } else if let broker = s.broker {
        result.rows += (broker.shares ?? []).map { h in
            let owned = s.positions?[h.symbol]?.owned_shares ?? 0
            let owner = owned > 0 ? "AutoWheel records \(owned) owned shares" : "external / unclaimed"
            return HoldingRow(id: "stock-\(h.symbol)", symbol: h.symbol,
                detail: "\(h.quantity) shares · \(owner)", at: broker.at)
        }
    }
    if deployment == .robinhood {
        if let broker = s.broker {
            result.rows += (broker.options ?? []).map { option in
                let position = s.positions?[option.symbol]
                let managed = option.id != nil && (option.id == position?.put_id || option.id == position?.call_id)
                return HoldingRow(id: "option-\(option.symbol)-\(option.kind)-\(option.expiry)-\(option.strike_cents)",
                    symbol: "\(option.symbol) · \(option.kind.uppercased())",
                    detail: "\(option.quantity) short contract · \(money(option.strike_cents)) · \(option.expiry) · \(managed ? "managed" : "ownership unverified")", at: broker.at)
            }
            result.emptyMessage = "No holdings in the last reported snapshot"
        } else {
            result.warning = "Option holdings unavailable. Stock rows alone do not establish an empty account."
            if s.account_info?.holdings != nil { result.emptyMessage = "No reported stock holdings · option holdings unavailable" }
        }
    } else if s.account_info?.holdings_complete == true, s.account_info?.holdings_scope == "all_positions", s.account_info?.holdings != nil {
        result.emptyMessage = "No holdings in the last reported snapshot"
    } else {
        result.warning = "Complete position coverage has not been established."
    }
    return result
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
