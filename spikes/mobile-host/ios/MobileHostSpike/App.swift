import SwiftUI
import UIKit

@main
struct MobileHostSpikeApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) var delegate

    var body: some Scene {
        WindowGroup { ContentView(host: Host.shared) }
    }
}

final class AppDelegate: NSObject, UIApplicationDelegate {
    func application(_ application: UIApplication,
                     didFinishLaunchingWithOptions _: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
        Host.shared.launch()
        let nc = NotificationCenter.default
        nc.addObserver(forName: UIApplication.didEnterBackgroundNotification, object: nil, queue: .main) { _ in
            Host.shared.didEnterBackground()
        }
        nc.addObserver(forName: UIApplication.willEnterForegroundNotification, object: nil, queue: .main) { _ in
            Host.shared.willEnterForeground()
        }
        nc.addObserver(forName: UIApplication.willTerminateNotification, object: nil, queue: .main) { _ in
            Host.shared.log("willTerminate")
        }
        return true
    }
}

struct ContentView: View {
    @ObservedObject var host: Host

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(host.title).font(.title2.bold())
            row("SFU start", host.sfuStart)
            row("SFU /health (URLSession)", host.health)
            row("Mesh start", host.meshStart)
            row("Mesh IPC connect", host.ipc)
            Text("IPC socket").font(.caption.bold())
            Text(host.socketPath).font(.caption2.monospaced())
            Text("Log").font(.caption.bold())
            Text(host.logPath).font(.caption2.monospaced())
            Spacer()
        }
        .padding()
    }

    private func row(_ label: String, _ value: String) -> some View {
        HStack(alignment: .top) {
            Text(label).font(.callout.bold())
            Spacer()
            Text(value).font(.callout.monospaced()).multilineTextAlignment(.trailing)
        }
    }
}
