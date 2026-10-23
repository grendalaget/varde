# Spike (b): Varde mesh + Gryt SFU in one Go runtime

**Result: feasible on Linux/amd64 and Android/x86_64; the gomobile AAR also builds for Android/arm64.** Both services ran concurrently in one `hostcheck` process and one Go runtime. The original Varde and Gryt working trees were not modified; all source changes and artifacts are under `/home/ubuntu/spikes`.

## Deliverables

- `/home/ubuntu/spikes/mesh` — copied mesh module, converted to an importable `package mesh`.
- `/home/ubuntu/spikes/sfu` — copied SFU module, with new `sfu-v2/embed`.
- `/home/ubuntu/spikes/mobilehost` — gomobile-facing host and `cmd/hostcheck`.
- `/home/ubuntu/spikes/mobilehost/mobilehost.aar` — Android arm64 + amd64 bindings.
- `/home/ubuntu/spikes/mobilehost/hostcheck-linux-amd64`
- `/home/ubuntu/spikes/mobilehost/hostcheck-android-amd64`

## Embedding changes

**Mesh**

- Changed the copied mesh Go files and tests to `package mesh`.
- Replaced the process entrypoint with `Main(ctx context.Context, args []string) error`. It uses a local `flag.FlagSet`, a local slog logger, returns parse/config/listen/serve errors, and stops its gRPC server when the context is canceled. Signal registration and command-level fatal/exit behavior were removed from the library entrypoint.
- Updated the parent-lifetime test helper to call `Main`.
- `--parent-pid` remains optional; when nonzero, `watchParent` still polls `os.Getppid()`. `PR_SET_PDEATHSIG` is not used. No process-global flag registry or signal handler remains in the mesh library.
- Mesh metrics use a private Prometheus registry per node. Mesh still requires an IPC listener: use a distinct Unix socket path for each instance. Android shell could not bind a filesystem socket under `/data/local/tmp` (`bind: permission denied`), so the copied Unix IPC implementation now supports Linux/Android abstract socket names (for example, `@mobilehost-mesh`).

**SFU**

- Moved the command startup body into `sfu-v2/embed.Run(ctx context.Context) error`; initialization and serve errors are returned. Configuration remains environment-variable based.
- `cmd/sfu` remains runnable as a thin wrapper around `embed.Run`; its `log.Fatal` is confined to the standalone CLI.
- Added context-aware recovery monitoring and cancellation handling for service loops and HTTP servers, plus an audio transport `Close` path for shutdown. The public HTTP server uses a dedicated mux.
- Remaining process-wide SFU state: readiness uses a package-global atomic flag and an unscoped STUN verification goroutine; recovery uses a global logger initialized with `sync.Once`; SFU metrics use `promauto` and the default Prometheus registry. These are shared within the process, so multiple independently configured SFU instances are not isolated. The spike host starts at most one SFU.

**Host**

- `mobilehost.StartSFU(envJSON string) error` sets the supplied environment for the SFU lifetime, waits for `/health`, and restores previous environment values on shutdown.
- `mobilehost.StartMesh(argsJSON string) error` calls `mesh.Main` and confirms that the IPC listener accepts a local Unix-domain connection. `mobilehost.Stop()` cancels both services and waits for them.
- Environment variables are process-global while the SFU runs; other code in the same process can observe them.

## Dependency resolution

Go module graph selection unified the dependency versions without vendoring or forking dependencies:

| Dependency | Module requirements | Selected in `mobilehost` |
|---|---|---|
| `quic-go` | Mesh `v0.63.0`, SFU `v0.64.1` | `v0.64.1` |
| Prometheus client | Mesh `v1.24.1`, SFU `v1.23.2` | `v1.24.1` |
| Pion WebRTC / ICE | SFU only | `webrtc/v4 v4.2.19`, `ice/v4 v4.4.1` |
| gRPC | Mesh only | `v1.84.0` |
| `google.golang.org/protobuf` | Mesh `v1.36.12`, SFU `v1.36.8` | `v1.36.12` |
| Prometheus common / procfs | Lower SFU versions and higher mesh versions | `common v0.70.1`, `procfs v0.21.1` |

There was no mesh/SFU duplicate metric registration: the mesh registry is private, while SFU metrics register once on Prometheus’s process-default registry. No registration collision occurred in the shared-process smoke test.

The copied mesh module needs Varde’s local `go` module. Its replacement is `/home/ubuntu/repos/varde/go`; a matching top-level replacement was also needed in `mobilehost/go.mod` because Go replacements are not inherited transitively. Both replacements are read-only dependencies on the original tree.

Android’s Pion dependency `github.com/wlynxg/anet v0.0.5` uses `//go:linkname` for `net.zoneCache`; Go 1.27 rejects it by default. The dependency’s README documents `-ldflags=-checklinkname=0`, which was required for Android builds. The Android host build also requires cgo enabled and the NDK clang compiler. `gomobile bind` required adding `tool golang.org/x/mobile/cmd/gobind` to the spike module; that tool dependency raises the selected `golang.org/x/net` version to `v0.61.0`.

## Build and artifact sizes

Installed Android NDK `27.2.12479018` and used `gomobile bind -target=android/arm64,android/amd64 -androidapi 24` with the Android linker flag above. Binding completed successfully.

The plain hostcheck builds also completed:

- Linux: `go build -o hostcheck-linux-amd64 ./cmd/hostcheck`
- Android/amd64: `GOOS=android GOARCH=amd64 CGO_ENABLED=1`, NDK `x86_64-linux-android24-clang`, and `-ldflags=-checklinkname=0`.

| Artifact | Size |
|---|---:|
| `mobilehost.aar` total | 31,813,896 bytes |
| `jni/arm64-v8a/libgojni.so` | 31,495,136 bytes uncompressed; 15,318,053 bytes compressed |
| `jni/x86_64/libgojni.so` | 33,515,360 bytes uncompressed; 16,485,914 bytes compressed |

## Verification and smoke tests

- `go test ./...` in `/home/ubuntu/spikes/mesh`: passed (`ok github.com/grendalaget/varde/apps/mesh 7.330s`).
- `go test ./...` in `/home/ubuntu/spikes/sfu`: passed for all packages, including `cmd/sfu`, audio transport, auth, config, room, and service packages.
- Linux/amd64 and Android/amd64 hostcheck builds: passed.
- Gomobile Android arm64 + amd64 binding: passed.

**Linux/amd64, same process:** the host printed the mesh listener and readiness lines; `/health` returned HTTP 200 and the Unix socket existed.

```text
{"time":"2026-10-23T15:34:29.662846683Z","level":"INFO","msg":"mesh IPC listening","ipc":"/tmp/mobilehost-mesh.sock"}
SFU ready at http://127.0.0.1:18505/health; mesh IPC listening at /tmp/mobilehost-mesh.sock
HTTP/1.1 200 OK
{"status":"healthy","service":"sfu","version":"dev","timestamp":"2026-10-23T15:34:43Z"}
mesh_socket=present
```

**Android emulator:** booted the `spike-b-android35-x86_64` AVD (Android 35, x86_64; `sys.boot_completed=1`), pushed and ran the Android/amd64 hostcheck, and forwarded port 18505 to the host. The filesystem IPC path failed with `bind: permission denied`; switching to an abstract Unix socket worked:

```text
{"time":"2026-10-23T15:33:53.372994947Z","level":"INFO","msg":"mesh IPC listening","ipc":"@mobilehost-mesh"}
SFU ready at http://127.0.0.1:18505/health; mesh IPC listening at @mobilehost-mesh
HTTP/1.1 200 OK
{"status":"healthy","service":"sfu","version":"dev","timestamp":"2026-10-23T15:34:20Z"}
0000000000000000: 00000002 00000000 00010000 0001 01 23898 @mobilehost-mesh
```

The process was stopped with `SIGTERM`; `Stop()` completed and the hostcheck exited successfully. The emulator is still running, as requested.

## Blockers and remaining caveats

- No build, binding, or runtime blocker remains for this spike.
- Android filesystem Unix sockets were denied for the shell process; abstract IPC names work and appear in `/proc/net/unix`, but do not create a filesystem socket node.
- SFU readiness, recovery logging, metrics, and the host’s temporary environment configuration are process-global. Treat the host API as a single-SFU/single-mesh configuration unless those globals are made instance-scoped.
- `-checklinkname=0` is required for the current Android dependency graph because of `anet`’s `net.zoneCache` linkname.

`git status --short` was empty for `/home/ubuntu/repos/varde`, `/tmp/gryt`, and `/tmp/gryt/packages/sfu`. No commit or PR was created.
