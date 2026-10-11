import AppKit
import SwiftUI

@MainActor
final class WheelController: NSObject {
    private let model: WheelModel
    private let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    private let popover = NSPopover()
    init(model: WheelModel) {
        self.model = model
        super.init()
        popover.behavior = .transient
        popover.animates = false
        let hosting = NSHostingController(rootView: WheelView(model: model))
        hosting.sizingOptions = .preferredContentSize
        popover.contentViewController = hosting
        item.button?.target = self
        item.button?.action = #selector(toggle)
        item.button?.title = ""
        item.button?.imagePosition = .imageOnly
        model.changed = { [weak self] in self?.update() }
        NSWorkspace.shared.notificationCenter.addObserver(self, selector: #selector(wake), name: NSWorkspace.didWakeNotification, object: nil)
        NSWorkspace.shared.notificationCenter.addObserver(self, selector: #selector(wake), name: NSWorkspace.sessionDidBecomeActiveNotification, object: nil)
        update()
    }
    private func update() {
        item.button?.toolTip = "AutoWheel · \(model.title) · \(model.status?.mode ?? "unverified")"
        item.button?.image = Self.makeTrayIcon(dot: model.trayColor)
    }
    // An A for AutoWheel, with OptionZero's 18-point size and status dot.
    private static func makeTrayIcon(dot: NSColor) -> NSImage {
        let image = NSImage(size: NSSize(width: 18, height: 18), flipped: false) { _ in
            NSColor.white.setStroke()
            let mark = NSBezierPath()
            mark.move(to: NSPoint(x: 2.8, y: 4.4))
            mark.line(to: NSPoint(x: 8, y: 14.4))
            mark.line(to: NSPoint(x: 13.2, y: 4.4))
            mark.move(to: NSPoint(x: 5, y: 8.7))
            mark.line(to: NSPoint(x: 11, y: 8.7))
            mark.lineWidth = 2.4
            mark.lineCapStyle = .round
            mark.lineJoinStyle = .round
            mark.stroke()
            dot.setFill()
            NSBezierPath(ovalIn: NSRect(x: 11.85, y: 1.1, width: 4.8, height: 4.8)).fill()
            return true
        }
        image.isTemplate = false
        image.accessibilityDescription = "AutoWheel"
        return image
    }
    @objc private func toggle() { if popover.isShown { popover.performClose(nil) } else { show() } }
    func show() {
        guard let button = item.button else { return }
        Task { await model.refresh() }
        NSApp.activate(ignoringOtherApps: true)
        popover.show(relativeTo: button.bounds, of: button, preferredEdge: .minY)
    }
    @objc private func wake() { Task { await model.refresh() } }
}

@MainActor
final class WheelDelegate: NSObject, NSApplicationDelegate {
    private let model = WheelModel()
    private var controller: WheelController?
    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)
        controller = WheelController(model: model)
        model.start()
        if CommandLine.arguments.contains("--show") { controller?.show() }
    }
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        controller?.show()
        return false
    }
    func applicationWillTerminate(_ notification: Notification) { model.stop() }
}

@main
struct AutoWheelEntry {
    @MainActor static func main() {
        if CommandLine.arguments.contains("--check-controls") {
            Task { @MainActor in
                do {
                    let model = WheelModel()
                    await model.refresh()
                    guard model.connected, model.readOnly else { throw OperatorError.message("Control check requires a connected read-only daemon") }
                    await model.control(.halt)
                    guard model.status?.halted == true else { throw OperatorError.message("Pause not confirmed") }
                    await model.service(.restart)
                    guard model.connected, model.status?.halted == true else { throw OperatorError.message("Pause did not survive restart") }
                    await model.service(.logs)
                    guard model.logs.contains("cycle") else { throw OperatorError.message("Daemon logs unavailable") }
                    await model.service(.stop)
                    guard !model.connected else { throw OperatorError.message("Stopped daemon still reported connected") }
                    await model.service(.start)
                    guard model.connected, model.status?.halted == true else { throw OperatorError.message("Start did not preserve HALT") }
                    await model.control(.resume)
                    guard model.connected, model.status?.halted == false, model.readOnly else { throw OperatorError.message("Read-only resume not confirmed") }
                    await model.control(.refresh)
                    guard model.connected, model.readOnly else { throw OperatorError.message("Read-only scan queue failed") }
                    print("Control check passed: durable pause/restart, SSH logs, stop/offline, guarded start, read-only resume and scan queue.")
                    exit(0)
                } catch {
                    fputs("Control check failed: \(error.localizedDescription)\n", stderr)
                    exit(1)
                }
            }
            RunLoop.main.run()
            return
        }
        // A focused integration check: decode the real daemon response and verify
        // precision, fractional timestamps, unknown broker values and live guards.
        if CommandLine.arguments.contains("--check-api") {
            do {
                let data = try Data(contentsOf: URL(string: "http://127.0.0.1:19841/status")!)
                let s = try JSONDecoder().decode(WheelStatus.self, from: data)
                guard timestamp(s.server_time) != nil, timestamp(s.at) != nil,
                      s.config.max_symbols == 3, s.config.max_collateral_cents == 8000000,
                      money(199) == "$1.99", timestamp("2026-10-04T14:21:40.123456789-07:00") != nil,
                      MSIService.command(.restart) == "sudo -n systemctl restart autowheel.service",
                      MSIService.command(.start) == "sudo -n systemctl start autowheel.service",
                      !MSIService.command(.stop).contains("--live") else {
                    throw OperatorError.message("Operator integration check failed")
                }
                if s.broker == nil && s.activeSymbols != nil { throw OperatorError.message("Unknown broker occupancy was fabricated") }
                print("Operator check passed: \(s.mode), \(s.stage), real API decoded; daemon owns activation guards.")
                exit(0)
            } catch {
                fputs("Operator check failed: \(error.localizedDescription)\n", stderr)
                exit(1)
            }
        }
        let delegate = WheelDelegate()
        NSApplication.shared.delegate = delegate
        // NSApplication's delegate is weak. Keep the controller/model/timer alive
        // across optimized builds and repeated popover activations.
        withExtendedLifetime(delegate) { NSApplication.shared.run() }
    }
}
