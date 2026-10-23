# Spike (b, iOS): Varde mesh + Gryt SFU in one iOS app (Simulator)

**Result: feasible.** Both Go services run inside one SwiftUI app, in one Go runtime, from a single
`gomobile bind` xcframework. No Go source changes were needed for iOS. `/health` answered HTTP 200 from
inside the app (URLSession) and from the Mac host (curl), and the mesh IPC Unix socket accepted
connections from both the app and the host.

**Background:** a plain app keeps serving for about **31 s** after backgrounding, which is the
`beginBackgroundTask` budget. Then it is suspended. With `UIBackgroundModes=audio` it kept serving for the
full **240 s** test window. However, the Simulator also kept the app alive when no audio session was active
at all (control run), and this VM has no audio device. **The audio result says nothing about real devices.**
See "Device expectations" below.

Environment: macOS 26.6.2 (arm64 VM), Xcode 26.6 (17F113), iOS 26.5 Simulator (iPhone 17), Go 1.27.1,
gomobile `v0.0.0-20261011034528-197770444867` (same x/mobile version as `mobilehost/go.mod`).

## 1. gomobile bind

```sh
cd spikes/mobile-host/mobilehost
GOWORK=off gomobile bind -target=ios,iossimulator -o ../ios/Mobilehost.xcframework .
```

- Built on the first try in about 60 s (wall clock). **No code changes and no extra build flags.**
- **`-ldflags=-checklinkname=0` is not needed on iOS.** The only offending `//go:linkname`
  (`net.zoneCache` in `github.com/wlynxg/anet`) lives in `interface_android.go`, so it is never compiled
  for `ios`.
- cgo, syscalls, Pion (ICE/DTLS/SRTP), quic-go, and gRPC all compiled and linked for `ios/arm64`,
  `iossimulator/arm64`, and `iossimulator/amd64`.
- App side: link the framework statically (`embed: false`) and add `-lresolv` (Go's `net` package uses
  res_9_* on darwin). Bundle IDs: `games.varde.spike.mobilehost` (plain) and `…mobilehost.audio`.

Framework sizes. These are static archives, so they are much larger than what ends up in the app:

| Slice | Archive size |
|---|---:|
| `ios-arm64` (device) | 54,256,272 B |
| `ios-arm64_x86_64-simulator` (fat) | 106,858,664 B |
| → simulator arm64 (lipo thin) | 54,269,520 B |
| → simulator x86_64 (lipo thin) | 52,589,096 B |
| Whole `.xcframework` on disk | 154 MB |

Linked app sizes (Release, unsigned, no `-s -w` / bitcode / extra stripping tried):

| Artifact | Size |
|---|---:|
| Device `MobileHostSpike.app/MobileHostSpike` (arm64) | 23,906,392 B |
| Device `.ipa` (zipped Payload) | 8,068,144 B |
| Simulator app binary (arm64) | 24,077,600 B |

For comparison, Android `libgojni.so` (arm64) was 31.5 MB uncompressed and 15.3 MB compressed.

## 2. App and verification

Project: `spikes/mobile-host/ios/`. `project.yml` (XcodeGen) generates the committed
`MobileHostSpike.xcodeproj`, with two targets that share the sources in `MobileHostSpike/`.
`build.sh` builds the xcframework (gitignored) and both apps. On launch, `Host.launch()` calls `StartSFU`
on a background queue, using the hostcheck environment (`SFU_PORT=18505`, control `127.0.0.1:18506`,
metrics off, `ICE_UDP_MUX_PORT=3478`, `DISABLE_STUN=true`). It then calls `StartMesh(["--ipc", <socket>])`,
connects to the socket with a raw `AF_UNIX` client, and calls `/health` through URLSession. ATS needs
`NSAllowsLocalNetworking`.

**Socket path / 104-byte limit:** in the Simulator, the container `tmp/mesh.sock` path is **173 bytes**
(`/Users/…/CoreSimulator/Devices/<UDID>/data/Containers/Data/Application/<UUID>/tmp/mesh.sock`), which is
over the 104-byte `sun_path` limit. The app checks the length. If the path is too long, it `chdir`s into the
container tmp dir and passes the relative path `mesh.sock`. Mesh binds it, and the in-app and host clients
connect to it. On a device, the equivalent path
(`/private/var/mobile/Containers/Data/Application/<UUID>/tmp/mesh.sock`, about 85 bytes) should fit as an
absolute path. A production host should still use a short name, or the relative-path trick, because the
process cwd is global state.

Launch log (`logs/plain/spike.log`):

```text
2026-10-23T15:42:43.081Z launch pid=5924 title=HostSpike (plain)
2026-10-23T15:42:43.348Z StartSFU ok (0.27s)
2026-10-23T15:42:43.348Z socket path 173 bytes >= 104; chdir …/Application/F7E364B7-…/tmp and use relative mesh.sock
2026-10-23T15:42:43.459Z StartMesh ok (0.11s) ipc=mesh.sock
2026-10-23T15:42:43.459Z mesh IPC connect: connected
2026-10-23T15:42:43.502Z URLSession /health: HTTP 200 {"status":"healthy","service":"sfu","version":"dev","timestamp":"2026-10-23T15:42:43Z"}
```

Host side (the Simulator shares the Mac's network stack and filesystem):

```text
$ curl -si http://127.0.0.1:18505/health
HTTP/1.1 200 OK
{"status":"healthy","service":"sfu","version":"dev","timestamp":"2026-10-23T15:42:23Z"}
$ cd <container>/tmp && python3 -c "import socket;s=socket.socket(socket.AF_UNIX);s.connect('mesh.sock')"
host connect ok, peer mesh.sock
```

The screenshot of the running app is attached to the session message. It is not committed.

## 3. Background budget

Method (`ios/bgtest.sh`): terminate both apps, launch the target with `simctl launch` (no debugger
attached, because a debugger prevents suspension), and start a host loop that curls `/health` every 500 ms
(`-m 0.4`; `000` = no answer). Background the app by launching Settings (`com.apple.Preferences`), wait N
seconds, then relaunch the app. The app logs `didEnterBackground` + `beginBackgroundTask`, the expiration
handler, a 1 Hz heartbeat with `backgroundTimeRemaining` (gaps in the heartbeat mean the process was
suspended), and `willEnterForeground`. Raw logs are in `ios/logs/<run>/`.

### a. Plain app (`beginBackgroundTask` only), 240 s in background

```text
15:43:38.408Z didEnterBackground beginBackgroundTask=3 backgroundTimeRemaining=unlimited
15:43:39.389Z heartbeat bg+1.0s backgroundTimeRemaining=29.6
15:44:04.235Z expirationHandler fired bg+25.9s
15:44:08.388Z heartbeat bg+30.0s backgroundTimeRemaining=0.5
15:44:09.387Z heartbeat bg+31.0s backgroundTimeRemaining=0.0      <- last heartbeat
15:47:37.361Z heartbeat bg+239.0s backgroundTimeRemaining=unlimited   <- resumed on foreground
15:47:37.362Z willEnterForeground after bg+239.0s
15:47:37.366Z URLSession /health: HTTP 200
```

Host curl (`logs/plain/curl.log`, transitions only): `200` until `15:44:09.225`, then `000` from
`15:44:09.756` (219 consecutive misses), then `200` again from `15:47:37.610` after foregrounding.

- `backgroundTimeRemaining` read `unlimited` inside `didEnterBackground`, and **≈30 s** one second later.
- The **expiration handler fired at bg+25.9 s**, about 4–5 s before the budget ran out (26.3 s in the other
  two runs).
- **`/health` kept answering until bg+≈30.8 s.** After that the process was suspended: no heartbeat and no
  TCP answers. It was *not* killed. The same pid resumed after 4 minutes, and the Go runtime, SFU, and mesh
  all continued serving with no restart.

### b. `UIBackgroundModes=audio` + `AVAudioSession(.playAndRecord)` active, 240 s in background

```text
15:52:53.683Z audio: category=AVAudioSessionCategoryPlayAndRecord active, silence playing=false
15:53:02.608Z didEnterBackground beginBackgroundTask=4 backgroundTimeRemaining=unlimited
15:53:28.891Z expirationHandler fired bg+26.3s
15:53:33.621Z heartbeat bg+31.0s backgroundTimeRemaining=0.0
15:53:37.763Z heartbeat bg+35.2s backgroundTimeRemaining=0.0
…
15:57:01.761Z heartbeat bg+239.2s backgroundTimeRemaining=0.0
15:57:02.070Z willEnterForeground after bg+239.5s
```

Host curl: **462 × `200`, 0 misses**, from 15:52:58.548 to 15:57:06.550. **`/health` kept answering for
the full 4 minutes (> 3 min) in the background.** The heartbeat continued at 1 Hz after the
background-task budget expired.

Caveats, which make this result weak even for the Simulator:

- **Silence was not actually playing.** `AVAudioPlayer.play()` returned `false`, because this macOS VM has
  no audio devices (`system_profiler SPAudioDataType` is empty). The session category was set and the session
  was activated successfully. My first attempt used `AVAudioEngine`: under `playAndRecord`, the I/O node
  reported a 0 Hz format here (`-66680 finding/initializing Default-InputOutput`), `connect` raised an ObjC
  exception, and the app crashed at launch. The app now uses `AVAudioPlayer` looping a generated silent WAV
  instead.
- **Control run (`-noAudioSession`): same audio target, audio key in Info.plist, no session activated
  (category stays `SoloAmbient`), 90 s in background.** It also kept serving: 184 × `200`, 0 misses, with
  the heartbeat running through bg+89.2 s (`logs/audio-nosession/`). So in the Simulator, **the Info.plist
  key alone prevents suspension**, whether or not audio is active. Run (b) therefore does not show that
  playing silence keeps a host alive.

## 4. Memory

The plain app had been idle about 25 s, with SFU + mesh running and no clients. Measured with
`footprint -p <pid>` and `vmmap --summary` (a Simulator process is a host macOS process):

```text
MobileHostSpike [5924]: 64-bit    Footprint: 21 MB (16384 bytes per page)
Physical footprint:         21.2M
Physical footprint (peak):  21.8M
```

The largest contributors are `__DATA` (6.3 MB dirty: Go binary data/bss), `MALLOC_SMALL` (4.7 MB, mostly
UIKit), and VM_ALLOCATE (4.2 MB, which includes the Go heap and goroutine stacks). This is idle memory only.
Under load, the SFU will grow with participants. Device numbers should be similar for the Go part, but
iOS jetsam limits for background apps are much tighter than for foreground apps.

## Device expectations: what will differ from the Simulator

The Simulator does not enforce iOS power or background policy the way a device does. Apple's documentation
only describes device behaviour, and the observations above should not be read as device timings.

1. **`beginBackgroundTask` budget (≈30 s here):** I expect about the same ballpark on a device. Apple
   documents the budget as limited and system-determined, and says not to rely on any fixed number. It can be
   shorter under load or low power. The expiration handler fires before the deadline, and an app that
   doesn't end its task is killed rather than suspended.
   [beginBackgroundTask](https://developer.apple.com/documentation/uikit/uiapplication/beginbackgroundtask(withname:expirationhandler:)),
   [backgroundTimeRemaining](https://developer.apple.com/documentation/uikit/uiapplication/backgroundtimeremaining),
   [Extending your app's background execution time](https://developer.apple.com/documentation/uikit/extending-your-app-s-background-execution-time).
2. **Suspended app resumes with sockets intact (seen here):** on a device, the kernel may reclaim the
   resources of a suspended app's listening sockets. The app then has to detect this and re-listen on resume.
   It can also be jettisoned under memory pressure, which means a cold restart.
   [TN2277 Networking and Multitasking](https://developer.apple.com/library/archive/technotes/tn2277/_index.html).
3. **Audio background mode (> 3 min here, even with no session):** expected to be **very different on a
   device.** The audio mode keeps an app running only while it is actually playing or recording audible
   content. An app with only the key, or with an idle session, is suspended like a plain app. Playing silence
   to stay alive may work technically, but it is the documented misuse case: App Review guideline 2.5.4 limits
   background modes to their intended purpose. `.playAndRecord` also needs microphone permission and shows
   the recording indicator. A voice-chat host that really carries audio (Gryt) is the legitimate case.
   [UIBackgroundModes](https://developer.apple.com/documentation/bundleresources/information-property-list/uibackgroundmodes),
   [Configuring your app for media playback](https://developer.apple.com/documentation/avfoundation/configuring-your-app-for-media-playback),
   [App Review Guidelines 2.5.4](https://developer.apple.com/app-review/guidelines/#software-requirements),
   [Choosing background strategies](https://developer.apple.com/documentation/backgroundtasks/choosing-background-strategies-for-your-app).
4. **Networking:** the Simulator uses the Mac's network stack. On a device, peers on the LAN connecting to
   the phone's SFU/QUIC ports will trigger the Local Network privacy prompt (`NSLocalNetworkUsageDescription`),
   and cellular NAT will block inbound connections. Neither was testable here.
5. **Memory:** expect similar idle numbers. Background jetsam limits apply only on a device.

Bottom line for the hosting model: a phone can host while the app is in the foreground, plus about 30 s
after it goes to the background. Background hosting beyond that needs a device test of the audio mode
*while real call audio flows*. Otherwise the host should hand over as soon as `didEnterBackground` fires and
the expiration handler arrives.

## Blockers and caveats

- No build, link, or runtime blocker on iOS. No Go changes, and no `-checklinkname=0`.
- Simulator-only: no device run (needs signing + hardware). Background timings are not authoritative (see above).
- No audio output device on the VM, so silence playback could not actually start.
- Process-global state from REPORT-b still applies (env vars, SFU globals). Add to that: the
  relative-socket workaround changes the process cwd.

## Files

- `ios/project.yml`, `ios/MobileHostSpike.xcodeproj`, `ios/Generated/*-Info.plist`: two targets,
  `MobileHostSpike` (plain) and `MobileHostSpikeAudio` (`UIBackgroundModes=audio`, `AUDIO_MODE`).
- `ios/MobileHostSpike/App.swift`, `ios/MobileHostSpike/Host.swift`: UI, service start, socket check,
  background logging, audio.
- `ios/build.sh`: gomobile bind + xcodegen + xcodebuild. `ios/bgtest.sh`: background harness (writes to
  `~/spike-out/<label>`).
- `ios/logs/{plain,audio,audio-nosession}/`: raw `spike.log` (app), `curl.log` (host), `host.log`.
