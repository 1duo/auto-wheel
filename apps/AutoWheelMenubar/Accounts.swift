import AppKit
import SwiftUI

@MainActor
final class AccountsModel: ObservableObject {
    let robinhood = WheelModel(deployment: .robinhood)
    let moomoo = WheelModel(deployment: .moomoo)
    @Published var selected: BrokerDeployment?
    var changed: (() -> Void)?

    init() {
        for account in [robinhood, moomoo] {
            account.changed = { [weak self] in
                self?.objectWillChange.send()
                self?.changed?()
            }
        }
    }
    func model(for deployment: BrokerDeployment) -> WheelModel {
        deployment == .robinhood ? robinhood : moomoo
    }
    var title: String { "Robinhood: \(robinhood.title) · Moomoo: \(moomoo.title)" }
    var trayColor: NSColor {
        let models = [robinhood, moomoo]
        if models.contains(where: { !$0.connected && $0.error != nil }) { return .systemRed }
        if models.contains(where: { !$0.connected || $0.status?.halted == true || ["blocked", "waiting_configuration"].contains($0.status?.stage ?? "") }) { return .systemYellow }
        if models.allSatisfy({ $0.status?.mode == "live" }) { return .systemGreen }
        return .systemCyan
    }
    func start() { robinhood.start(); moomoo.start() }
    func stop() { robinhood.stop(); moomoo.stop() }
    func refresh() async {
        async let first: Void = robinhood.refresh()
        async let second: Void = moomoo.refresh()
        _ = await (first, second)
    }
}

struct AccountsView: View {
    @ObservedObject var accounts: AccountsModel

    var body: some View {
        if let selected = accounts.selected {
            VStack(spacing: 0) {
                Button { accounts.selected = nil } label: {
                    HStack {
                        Image(systemName: "chevron.left")
                        Text("Both accounts")
                        Spacer()
                        Text(selected.name).fontWeight(.semibold)
                    }.padding(.horizontal, 14).padding(.vertical, 10)
                }.buttonStyle(.plain)
                Divider()
                WheelView(model: accounts.model(for: selected)).id(selected)
            }.frame(width: 390)
        } else {
            VStack(alignment: .leading, spacing: 0) {
                HStack {
                    Text("AutoWheel · Accounts").font(.system(size: 14, weight: .semibold))
                    Spacer()
                    Button { Task { await accounts.refresh() } } label: { Image(systemName: "arrow.clockwise") }
                        .buttonStyle(.plain).help("Refresh both account displays")
                }.padding(14)
                Divider()
                VStack(spacing: 6) {
                    summary(accounts.robinhood)
                    summary(accounts.moomoo)
                }.padding(.horizontal, 14).padding(.vertical, 8)
                Divider()
                ScrollView {
                    VStack(spacing: 10) {
                        AccountCard(model: accounts.robinhood) { accounts.selected = .robinhood }
                        AccountCard(model: accounts.moomoo) { accounts.selected = .moomoo }
                    }.padding(12)
                }.frame(maxHeight: 540)
                Divider()
                Text("Both statuses refresh every 20s, including while closed. Holdings show their last successful broker read.")
                    .font(.system(size: 10)).foregroundStyle(.secondary).padding(12)
                Button("Quit AutoWheel") { NSApp.terminate(nil) }.buttonStyle(.plain).padding(.horizontal, 14).padding(.bottom, 12)
            }.frame(width: 390)
        }
    }

    private func summary(_ model: WheelModel) -> some View {
        Button { accounts.selected = model.deployment } label: {
            HStack {
                Circle().fill(model.color).frame(width: 7, height: 7)
                Text(model.deployment.name).fontWeight(.medium)
                Spacer()
                Text(model.title).lineLimit(1).foregroundStyle(.secondary)
            }.font(.system(size: 11))
        }.buttonStyle(.plain)
    }
}

private struct AccountCard: View {
    @ObservedObject var model: WheelModel
    let open: () -> Void

    private var presentation: HoldingsPresentation {
        holdingsPresentation(model.status, deployment: model.deployment)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Circle().fill(model.color).frame(width: 8, height: 8)
                Text(model.deployment.name).font(.system(size: 13, weight: .semibold))
                if let suffix = model.status?.account_info?.account_last4 { Text("••••\(suffix)").font(.system(size: 11)).foregroundStyle(.secondary) }
                Spacer()
                Button("Controls", action: open).buttonStyle(.plain).font(.system(size: 11, weight: .medium))
            }
            Text(model.title).font(.system(size: 11, weight: .medium))
            if let reason = model.error ?? model.status?.reason {
                Text(reason).font(.system(size: 10)).foregroundStyle(model.error == nil ? Color.secondary : .red)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if let info = model.status?.account_info {
                HStack {
                    Text(info.balance_label ?? "Available cash · last known")
                    Spacer()
                    Text(info.balanceText).monospacedDigit()
                }.font(.system(size: 11))
            }
            Divider()
            TimelineView(.periodic(from: .now, by: 1)) { context in
                VStack(alignment: .leading, spacing: 6) {
                    if presentation.rows.isEmpty {
                        Text(presentation.emptyMessage)
                            .font(.system(size: 11)).foregroundStyle(.secondary)
                    }
                    ForEach(presentation.rows) { holding in
                        VStack(alignment: .leading, spacing: 2) {
                            Text(holding.symbol).font(.system(size: 11, weight: .medium, design: .monospaced))
                            Text(holding.detail).font(.system(size: 10)).foregroundStyle(.secondary)
                            let fresh = holdingsFresh(holding.at, connected: model.connected && model.status?.stage != "blocked", now: context.date)
                            Text("\(fresh ? "Observed" : "Last known · stale") \(age(holding.at, now: context.date))")
                                .font(.system(size: 10)).foregroundStyle(fresh ? Color.secondary : .orange)
                        }
                    }
                    if let warning = presentation.warning {
                        Text(warning).font(.system(size: 10)).foregroundStyle(.secondary)
                    }
                    if let info = model.status?.account_info, info.holdings_source_time_known == false {
                        Text("Times show when the API was read; provider holding timestamps are unavailable.")
                            .font(.system(size: 10)).foregroundStyle(.secondary)
                    }
                }
            }
            HStack {
                Text(model.connected ? "Connected · msi" : "Disconnected · last known data")
                    .foregroundStyle(model.connected ? Color.secondary : .orange)
                Spacer()
                if model.connected {
                    Button("Pause \(model.deployment.name)") { Task { await model.control(.halt) } }
                        .buttonStyle(.plain).disabled(model.busy)
                }
            }.font(.system(size: 10))
        }.padding(12).background(.quaternary.opacity(0.35)).clipShape(RoundedRectangle(cornerRadius: 8))
    }
}
