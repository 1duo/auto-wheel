import Foundation

@main
struct MenubarPolicyCheck {
    static func main() throws {
        func configFields(_ policy: String?) -> [String: Any] {
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
            return fields
        }
        func config(_ policy: String?) throws -> WheelConfig {
            try JSONDecoder().decode(WheelConfig.self, from: JSONSerialization.data(withJSONObject: configFields(policy)))
        }
        func status(_ extra: [String: Any]) throws -> WheelStatus {
            let now = Date().ISO8601Format()
            var fields: [String: Any] = [
                "version":"fixture", "mode":"read_only", "stage":"blocked", "at":now, "server_time":now,
                "halted":true, "account_configured":true, "token_present":true, "config":configFields(nil),
            ]
            fields.merge(extra, uniquingKeysWith: { _, new in new })
            return try JSONDecoder().decode(WheelStatus.self, from: JSONSerialization.data(withJSONObject: fields))
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
        let asap = try config("conservative-rd1-floor-tp75-asap-v3")
        precondition(asap.selectionMode == .conservative)
        precondition(asap.fixed_policy?.usesRestingTP75 == true)
        precondition(asap.fixed_policy?.replenishesASAP == true)
        precondition(asap.fixed_policy?.scheduleLabel == "Resting TP75 · funded slot replenishment")
        let target = try status(["execution_policy_id":"conservative-rd1-floor-tp75-asap-v3"])
        precondition(target.executionTargetLabel == "Conservative · five slots + TP75 + ASAP")
        precondition(target.live_capable != true)
        let unknown = try config("future-policy")
        precondition(unknown.selectionMode == nil)
        precondition(unknown.fixed_policy?.label == "Unknown fixed policy")
        precondition(unknown.fixed_policy?.scheduleLabel == "Unknown schedule")
        let legacy = try config(nil)
        precondition(legacy.selectionMode == .balanced)
        precondition(BrokerDeployment.robinhood.baseURL.port == 19841)
        precondition(BrokerDeployment.moomoo.baseURL.port == 19842)
        precondition(BrokerDeployment.robinhood.operatorCredentialName == "operator-token.json")
        precondition(BrokerDeployment.moomoo.operatorCredentialName == "moomoo-operator-token.json")
        precondition(MSIService.command(.restart) == "sudo -n systemctl restart autowheel.service")
        precondition(MSIService.command(.restart, deployment: .moomoo) == "sudo -n systemctl restart autowheel-moomoo.service")
        precondition(MSIService.command(.logs, deployment: .moomoo).contains("-u autowheel-moomoo.service"))
        let now = Date()
        var nativeConfig = configFields("conservative-rd1-floor-tp75-asap-v3")
        nativeConfig["max_symbols"] = 5
        nativeConfig["order_coverage_policy"] = "moomoo_complete"
        var nativeFields: [String: Any] = ["broker_id":"moomoo", "deployment_id":"autowheel-moomoo", "execution_installed":true,
            "live_capable":true, "live_readiness":["at":now.ISO8601Format(), "ready":true],
            "broker":["at":now.ISO8601Format(), "available_cash_cents":0], "config":nativeConfig]
        let nativeReady = try status(nativeFields)
        precondition(nativeReady.canActivate(deployment: .moomoo, now: now))
        nativeFields["broker"] = ["at":now.addingTimeInterval(-31).ISO8601Format(), "available_cash_cents":0]
        let nativeStale = try status(nativeFields)
        precondition(!nativeStale.canActivate(deployment: .moomoo, now: now))
        nativeFields["broker"] = ["at":now.ISO8601Format(), "available_cash_cents":0]
        nativeFields["execution_installed"] = false
        let nativeMissing = try status(nativeFields)
        precondition(!nativeMissing.canActivate(deployment: .moomoo, now: now))
        nativeFields["execution_installed"] = true
        nativeConfig["order_coverage_policy"] = "exclusive_standard"
        nativeFields["config"] = nativeConfig
        let nativeForeignCoverage = try status(nativeFields)
        precondition(!nativeForeignCoverage.canActivate(deployment: .moomoo, now: now))
        precondition(holdingsFresh(now.ISO8601Format(), connected: true, now: now))
        precondition(!holdingsFresh(now.ISO8601Format(), connected: false, now: now))
        precondition(!holdingsFresh(now.addingTimeInterval(-601).ISO8601Format(), connected: true, now: now))
        precondition(!holdingsFresh(now.addingTimeInterval(60).ISO8601Format(), connected: true, now: now))
        let info: [String:Any] = ["at":now.ISO8601Format(), "account_last4":"1234", "options_level":"unverified", "available_cash_cents":0, "snapshot_complete":false, "holdings":[]]
        let partial = try status(["account_info":info])
        let partialView = holdingsPresentation(partial, deployment:.robinhood)
        precondition(partialView.emptyMessage.contains("option holdings unavailable") && partialView.warning != nil)
        precondition(!BrokerDeployment.moomoo.acceptsStatus(partial))
        precondition(BrokerDeployment.robinhood.acceptsStatus(partial))
        var complete = info
        complete["holdings_complete"] = true
        complete["holdings_scope"] = "all_positions"
        let moomoo = try status(["broker_id":"moomoo", "deployment_id":"autowheel-moomoo", "account_info":complete])
        precondition(BrokerDeployment.moomoo.acceptsStatus(moomoo))
        precondition(!BrokerDeployment.robinhood.acceptsStatus(moomoo))
        let empty = holdingsPresentation(moomoo, deployment:.moomoo)
        precondition(empty.rows.isEmpty && empty.warning == nil && empty.emptyMessage.contains("No holdings"))
        var fractionalFunds = complete
        fractionalFunds.removeValue(forKey:"available_cash_cents")
        fractionalFunds["reported_funds"] = "12.3456789"
        let exactFunds = try status(["broker_id":"moomoo", "deployment_id":"autowheel-moomoo", "account_info":fractionalFunds])
        precondition(exactFunds.account_info?.available_cash_cents == nil)
        precondition(exactFunds.account_info?.balanceText == "USD 12.3456789")
        precondition(partial.account_info?.balanceText == money(0))
        let wrongDeployment = try status(["broker_id":"moomoo", "deployment_id":"other"])
        precondition(!BrokerDeployment.moomoo.acceptsStatus(wrongDeployment))
        var stock = info
        stock["holdings"] = [["symbol":"AMD", "quantity":"100", "available":"100"]]
        let ownedPutOnly = try status(["account_info":stock, "positions":["AMD":["put_id":"owned-put", "owned_shares":0]]])
        let unclaimedStock = holdingsPresentation(ownedPutOnly, deployment:.robinhood)
        precondition(unclaimedStock.rows.count == 1 && unclaimedStock.rows[0].detail.contains("unclaimed"))
        let options = try status(["broker":["at":now.ISO8601Format(), "available_cash_cents":0, "options":[["id":"outside-put", "symbol":"AMD", "kind":"put", "quantity":1, "strike_cents":10000, "expiry":"2026-11-20"]]], "positions":["AMD":["put_id":"owned-put", "owned_shares":0]]])
        precondition(holdingsPresentation(options, deployment:.robinhood).rows[0].detail.contains("ownership unverified"))
        print("Menubar policy checks passed: strategy compatibility, broker/deployment routes, tokens, services, freshness, partial holdings and exact ownership.")
    }
}
