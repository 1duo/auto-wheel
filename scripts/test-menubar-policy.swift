import Foundation

@main
struct MenubarPolicyCheck {
    static func main() throws {
        func config(_ policy: String?) throws -> WheelConfig {
            var fields: [String: Any] = [
                "symbols": ["AMD"], "max_symbols": 3, "max_collateral_cents": 20_000_000,
                "cash_reserve_cents": 1_680_944, "fee_per_contract_cents": 100,
                "min_bid_cents": 1, "min_open_interest": 0, "max_spread_bps": 1500,
                "put_min_delta": 0.05, "put_max_delta": 0.15,
                "call_min_delta": 0.50, "call_max_delta": 0.85,
                "allow_calls_below_cost": false, "max_quote_age_seconds": 30,
                "max_dte_days": 45, "poll_seconds": 300,
            ]
            if let policy { fields["fixed_policy"] = ["spec": ["id": policy]] }
            return try JSONDecoder().decode(WheelConfig.self, from: JSONSerialization.data(withJSONObject: fields))
        }
        let hold = try config("conservative-rd1-floor-hold-v1")
        precondition(hold.selectionMode == .conservative)
        precondition(hold.fixed_policy?.label == "Conservative · floor + hold")
        precondition(hold.fixed_policy?.usesTP75 == false)
        let tp = try config("conservative-rd1-floor-tp75-v1")
        precondition(tp.selectionMode == .conservative)
        precondition(tp.fixed_policy?.label == "Conservative · floor + TP75")
        precondition(tp.fixed_policy?.scheduleLabel == "Daily TP75 · weekly openings")
        precondition(tp.fixed_policy?.readinessDetail.contains("Daily profit checks") == true)
        precondition(tp.max_collateral_cents == 20_000_000)
        let unknown = try config("future-policy")
        precondition(unknown.selectionMode == nil)
        precondition(unknown.fixed_policy?.label == "Unknown fixed policy")
        precondition(unknown.fixed_policy?.scheduleLabel == "Unknown schedule")
        let legacy = try config(nil)
        precondition(legacy.selectionMode == .balanced)
        print("Menubar policy checks passed: hold, TP75, unknown policy, legacy mode and capital.")
    }
}
