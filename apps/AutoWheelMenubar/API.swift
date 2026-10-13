import Foundation
import Darwin

enum OperatorAction: String { case halt, resume, refresh, live }
enum ServiceAction: String { case start, stop, restart, logs }

struct OperatorAPI {
    let base = URL(string: "http://127.0.0.1:19841")!
    var calendarURL: URL { base.appendingPathComponent("calendar") }
    func calendarAvailable() async -> Bool {
        var request = URLRequest(url: calendarURL)
        request.httpMethod = "HEAD"
        request.timeoutInterval = 3
        request.cachePolicy = .reloadIgnoringLocalCacheData
        guard let (_, response) = try? await URLSession.shared.data(for: request),
              let http = response as? HTTPURLResponse else { return false }
        return http.statusCode == 200 && http.mimeType == "text/html"
    }
    func status() async throws -> WheelStatus {
        let data = try await request("/status")
        let status = try JSONDecoder().decode(WheelStatus.self, from: data)
        guard ["read_only", "live"].contains(status.mode), timestamp(status.server_time) != nil else {
            throw OperatorError.message("Unsupported daemon response")
        }
        return status
    }
    func control(_ action: OperatorAction) async throws {
        _ = try await request("/control/\(action.rawValue)", post: true)
    }
    func selection(_ mode: SelectionMode) async throws {
        _ = try await request("/control/selection/\(mode.rawValue)", post: true)
    }
    private func request(_ path: String, post: Bool = false) async throws -> Data {
        var req = URLRequest(url: base.appendingPathComponent(String(path.dropFirst())))
        req.timeoutInterval = post ? 35 : 7
        req.cachePolicy = .reloadIgnoringLocalCacheData
        req.httpMethod = post ? "POST" : "GET"
        if post {
            req.setValue("confirm", forHTTPHeaderField: "X-AutoWheel-Control")
            req.setValue("Bearer \(try operatorToken())", forHTTPHeaderField: "Authorization")
        }
        let (data, response) = try await URLSession.shared.data(for: req)
        guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) else {
            let message = String(data: data.prefix(1024), encoding: .utf8) ?? "Request failed"
            throw OperatorError.message(message.trimmingCharacters(in: .whitespacesAndNewlines))
        }
        return data
    }
    private func operatorToken() throws -> String {
        let path = FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/Application Support/AutoWheel/operator-token.json").path
        let fd = Darwin.open(path, O_RDONLY | O_NOFOLLOW | O_NONBLOCK)
        guard fd >= 0 else { throw OperatorError.message("Private operator credential unavailable; pair this Mac with the daemon.") }
        let handle = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
        defer { try? handle.close() }
        var info = stat()
        guard fstat(fd, &info) == 0, info.st_mode & mode_t(S_IFMT) == mode_t(S_IFREG),
              info.st_uid == getuid(), info.st_mode & 0o077 == 0, info.st_size <= 4096,
              let data = try handle.read(upToCount: 4097),
              let token = try? JSONDecoder().decode(String.self, from: data), token.utf8.count == 64,
              token.utf8.allSatisfy({ (48...57).contains($0) || (97...102).contains($0) }) else {
            throw OperatorError.message("Operator credential must be a private regular file containing the daemon token.")
        }
        return token
    }
}

enum OperatorError: LocalizedError {
    case message(String)
    var errorDescription: String? { if case .message(let text) = self { return text }; return nil }
}

enum MSIService {
    // These commands are fixed by the app. Broker responses and editable UI text
    // can never become shell commands. The daemon validates its durable activation record on every start.
    static func command(_ action: ServiceAction) -> String {
        if action == .logs { return "journalctl -u autowheel.service -n 80 --reverse --no-pager -o cat" }
        let control = "sudo -n systemctl \(action.rawValue) autowheel.service"
        return control
    }
    static func run(_ action: ServiceAction) async throws -> String {
        try await Task.detached(priority: .utility) {
            let process = Process()
            process.executableURL = URL(fileURLWithPath: "/usr/bin/ssh")
            process.arguments = ["-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=yes", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=1", "yuduo@192.168.1.50", command(action)]
            let pipe = Pipe()
            process.standardOutput = pipe
            process.standardError = pipe
            process.standardInput = FileHandle.nullDevice
            try process.run()
            let timeout = DispatchWorkItem { if process.isRunning { process.terminate() } }
            DispatchQueue.global().asyncAfter(deadline: .now() + 15, execute: timeout)
            // Drain while running so a large log line cannot fill the pipe and deadlock.
            var output = Data()
            while true {
                let chunk = pipe.fileHandleForReading.availableData
                if chunk.isEmpty { break }
                if output.count < 131072 { output.append(chunk.prefix(131072 - output.count)) }
            }
            process.waitUntilExit()
            timeout.cancel()
            let text = String(data: output, encoding: .utf8) ?? "Output unavailable"
            guard process.terminationStatus == 0 else {
                throw OperatorError.message(text.isEmpty ? "SSH command failed or timed out" : text)
            }
            return text
        }.value
    }
}
