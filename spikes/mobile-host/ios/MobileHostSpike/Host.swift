import AVFoundation
import Foundation
import Mobilehost
import UIKit

let sfuPort = 18505

final class Host: ObservableObject {
    static let shared = Host()

    #if AUDIO_MODE
    let title = "HostSpike (audio background mode)"
    #else
    let title = "HostSpike (plain)"
    #endif

    @Published var sfuStart = "…"
    @Published var meshStart = "…"
    @Published var health = "…"
    @Published var ipc = "…"
    @Published var socketPath = ""
    @Published var logPath = ""

    private let logURL: URL
    private let logQueue = DispatchQueue(label: "log")
    private var heartbeat: DispatchSourceTimer?
    private var bgTask: UIBackgroundTaskIdentifier = .invalid
    private var backgroundedAt: Date?
    #if AUDIO_MODE
    private var audioPlayer: AVAudioPlayer?
    #endif

    private init() {
        let docs = FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0]
        logURL = docs.appendingPathComponent("spike.log")
        logPath = logURL.path
    }

    func log(_ message: String) {
        let line = "\(ISO8601DateFormatter.spike.string(from: Date())) \(message)\n"
        logQueue.async { [logURL] in
            if let h = try? FileHandle(forWritingTo: logURL) {
                h.seekToEndOfFile()
                h.write(line.data(using: .utf8)!)
                try? h.close()
            } else {
                try? line.data(using: .utf8)!.write(to: logURL)
            }
        }
        NSLog("spike: %@", message)
    }

    func launch() {
        log("launch pid=\(getpid()) title=\(title)")
        startHeartbeat()
        #if AUDIO_MODE
        if ProcessInfo.processInfo.arguments.contains("-noAudioSession") {
            log("audio: session not activated (-noAudioSession)")
        } else {
            startAudio()
        }
        #endif
        DispatchQueue.global(qos: .userInitiated).async { self.startServices() }
    }

    // sun_path is 104 bytes on Darwin. The Simulator's container tmp path is
    // longer than that, so fall back to a path relative to the cwd.
    private func meshSocket() -> String {
        let tmp = FileManager.default.temporaryDirectory
        let absolute = tmp.appendingPathComponent("mesh.sock").path
        if absolute.utf8.count < 104 { return absolute }
        FileManager.default.changeCurrentDirectoryPath(tmp.path)
        log("socket path \(absolute.utf8.count) bytes >= 104; chdir \(tmp.path) and use relative mesh.sock")
        return "mesh.sock"
    }

    private func startServices() {
        let env: [String: String] = [
            "SFU_PORT": String(sfuPort),
            "SFU_HOST": "127.0.0.1",
            "SFU_CONTROL_PORT": "18506",
            "SFU_CONTROL_HOST": "127.0.0.1",
            "SFU_METRICS_PORT": "0",
            "ICE_UDP_MUX_PORT": "3478",
            "DISABLE_STUN": "true",
            "SFU_CALL_ALONE_TIMEOUT": "0",
        ]
        var err: NSError?
        let t0 = Date()
        let sfuOK = MobilehostStartSFU(json(env), &err)
        let sfuMsg = sfuOK ? String(format: "ok (%.2fs)", Date().timeIntervalSince(t0)) : "error: \(err?.localizedDescription ?? "?")"
        log("StartSFU \(sfuMsg)")

        let socket = meshSocket()
        err = nil
        let t1 = Date()
        let meshOK = MobilehostStartMesh(json(["--ipc", socket]), &err)
        let meshMsg = meshOK ? String(format: "ok (%.2fs)", Date().timeIntervalSince(t1)) : "error: \(err?.localizedDescription ?? "?")"
        log("StartMesh \(meshMsg) ipc=\(socket)")

        let resolved = socket.hasPrefix("/") ? socket : FileManager.default.currentDirectoryPath + "/" + socket
        let ipcResult = connectUnix(socket)
        log("mesh IPC connect: \(ipcResult)")
        DispatchQueue.main.async {
            self.sfuStart = sfuMsg
            self.meshStart = meshMsg
            self.socketPath = resolved
            self.ipc = ipcResult
        }
        checkHealth()
    }

    func checkHealth(completion: ((String) -> Void)? = nil) {
        let url = URL(string: "http://127.0.0.1:\(sfuPort)/health")!
        URLSession.shared.dataTask(with: url) { data, response, error in
            let result: String
            if let http = response as? HTTPURLResponse {
                result = "HTTP \(http.statusCode) \(String(data: data ?? Data(), encoding: .utf8) ?? "")"
            } else {
                result = "error: \(error?.localizedDescription ?? "?")"
            }
            self.log("URLSession /health: \(result)")
            DispatchQueue.main.async { self.health = result.components(separatedBy: " ").prefix(2).joined(separator: " ") }
            completion?(result)
        }.resume()
    }

    // Logs once a second; gaps in the log mean the process was suspended.
    private func startHeartbeat() {
        let t = DispatchSource.makeTimerSource(queue: DispatchQueue.global())
        t.schedule(deadline: .now() + 1, repeating: 1)
        t.setEventHandler { [weak self] in
            guard let self, let since = self.backgroundedAt else { return }
            DispatchQueue.main.async {
                let remaining = UIApplication.shared.backgroundTimeRemaining
                let r = remaining > 1e8 ? "unlimited" : String(format: "%.1f", remaining)
                self.log(String(format: "heartbeat bg+%.1fs backgroundTimeRemaining=%@", Date().timeIntervalSince(since), r))
            }
        }
        t.resume()
        heartbeat = t
    }

    func didEnterBackground() {
        backgroundedAt = Date()
        bgTask = UIApplication.shared.beginBackgroundTask(withName: "spike-host") { [weak self] in
            guard let self else { return }
            let since = self.backgroundedAt.map { Date().timeIntervalSince($0) } ?? -1
            self.log(String(format: "expirationHandler fired bg+%.1fs", since))
            UIApplication.shared.endBackgroundTask(self.bgTask)
            self.bgTask = .invalid
        }
        let remaining = UIApplication.shared.backgroundTimeRemaining
        log("didEnterBackground beginBackgroundTask=\(bgTask.rawValue) backgroundTimeRemaining=\(remaining > 1e8 ? "unlimited" : String(format: "%.1f", remaining))")
        #if AUDIO_MODE
        log("audio playing=\(audioPlayer?.isPlaying ?? false) category=\(AVAudioSession.sharedInstance().category.rawValue)")
        #endif
    }

    func willEnterForeground() {
        guard let since = backgroundedAt.map({ Date().timeIntervalSince($0) }) else { return }
        log(String(format: "willEnterForeground after bg+%.1fs", since))
        backgroundedAt = nil
        if bgTask != .invalid {
            UIApplication.shared.endBackgroundTask(bgTask)
            bgTask = .invalid
        }
        checkHealth()
    }

    #if AUDIO_MODE
    // AVAudioPlayer rather than AVAudioEngine: the Simulator often has no
    // input device, so an engine I/O node under playAndRecord reports a 0 Hz
    // format and AVAudioEngine.connect raises an ObjC exception.
    private func startAudio() {
        do {
            let session = AVAudioSession.sharedInstance()
            try session.setCategory(.playAndRecord, mode: .default, options: [.mixWithOthers, .defaultToSpeaker])
            try session.setActive(true)
            let url = FileManager.default.temporaryDirectory.appendingPathComponent("silence.wav")
            try silentWAV(seconds: 1).write(to: url)
            let p = try AVAudioPlayer(contentsOf: url)
            p.numberOfLoops = -1
            let playing = p.play()
            audioPlayer = p
            log("audio: category=\(session.category.rawValue) active, silence playing=\(playing)")
        } catch {
            log("audio: failed \(error)")
        }
    }

    private func silentWAV(seconds: Int) -> Data {
        let rate: UInt32 = 44100, channels: UInt16 = 1, bits: UInt16 = 16
        let dataSize = rate * UInt32(seconds) * UInt32(channels * bits / 8)
        var d = Data()
        func put<T>(_ v: T) { withUnsafeBytes(of: v) { d.append(contentsOf: $0) } }
        d.append(contentsOf: Array("RIFF".utf8)); put(UInt32(36 + dataSize).littleEndian)
        d.append(contentsOf: Array("WAVEfmt ".utf8)); put(UInt32(16).littleEndian); put(UInt16(1).littleEndian)
        put(channels.littleEndian); put(rate.littleEndian); put((rate * UInt32(channels * bits / 8)).littleEndian)
        put((channels * bits / 8).littleEndian); put(bits.littleEndian)
        d.append(contentsOf: Array("data".utf8)); put(dataSize.littleEndian)
        d.append(Data(count: Int(dataSize)))
        return d
    }
    #endif

    private func json(_ value: Any) -> String {
        String(data: try! JSONSerialization.data(withJSONObject: value), encoding: .utf8)!
    }

    private func connectUnix(_ path: String) -> String {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { return "socket() errno \(errno)" }
        defer { close(fd) }
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8)
        guard bytes.count < MemoryLayout.size(ofValue: addr.sun_path) else { return "path too long" }
        withUnsafeMutableBytes(of: &addr.sun_path) { buf in
            for (i, b) in bytes.enumerated() { buf[i] = b }
        }
        let rc = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        return rc == 0 ? "connected" : "connect errno \(errno)"
    }
}

extension ISO8601DateFormatter {
    static let spike: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }()
}
