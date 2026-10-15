# Android anchor

An Android phone can serve as a Varde anchor — a node that only stores and
serves save replicas, never hosts games. Anchors don't need to be online
every second; they need to answer quickly (seconds) when another machine
comes online wanting the latest save. A foreground Android service with a
partial wake lock comfortably meets that.

## What's here

`apps/android` is a minimal Kotlin app (no AndroidX, no Compose) that:

- enrolls the phone as an anchor (`varde-agent enroll --anchor --token`),
- runs `varde-agent run` inside a `specialUse` foreground service with a
  partial wake lock, restarting it with backoff if it dies (Android 15+
  caps `dataSync` services at ~6 h/day and won't start them from
  BOOT_COMPLETED — `specialUse` is the exempt category),
- starts on boot when the anchor was left enabled,
- surfaces the agent's latest log line as the ongoing notification text,
- nudges the user to exempt the app from battery optimization (Doze is the
  real anchor-killer).

The binaries ship inside the APK as `libagent.so`/`libmesh.so` under
`jniLibs/arm64-v8a`. Naming them `lib*.so` is required: since Android 10,
an app can no longer `exec()` arbitrary files it wrote to its private
storage, but files extracted to `nativeLibraryDir` remain executable by
the owning app. `useLegacyPackaging` + `extractNativeLibs` keep them as
real files on disk.

## Build

```sh
make android          # cross-compiles varde-agent + varde-mesh for arm64,
                      # stages them into apps/android/.../jniLibs
cd apps/android
gradle wrapper && ./gradlew assembleDebug
adb install app/build/outputs/apk/debug/app-debug.apk
```

`make android` needs `cargo install cross` (uses the NDK image pinned in
`Cross.toml`) and a normal Go toolchain — the mesh builds with plain
`GOOS=android GOARCH=arm64 CGO_ENABLED=0` and needs no NDK.

## Enroll

In the dashboard, create an invite for the group, paste the invite token
into the app, tap **Enroll as anchor**. The phone then serves replicas like
any other anchor node. Enroll uses the token flow — the device flow needs a
browser and is a desktop path.

## Termux dev path (no app required)

On devices running Android ≤ 9 — or with an OEM that still allows exec in
app-private dirs — the same binaries run under Termux:

```sh
pkg install termux-api
termux-wake-lock
./varde-agent enroll --data-dir $HOME/varde --anchor --token <tok>
./varde-agent run --data-dir $HOME/varde --mesh-bin ./varde-mesh --anchor
```

This is a development path, not a product one: modern Android kills the
`exec()` of files Termux downloaded itself (the W^X / targetSdk≥29
restriction), which is exactly the constraint the lib*.so packaging works
around inside the real app.

## Releases and CI signing

The `android` job in `build-release.yml` cross-compiles the binaries,
assembles `app-release-unsigned.apk`, zipaligns it, and signs it when the
repository secrets are set:

- `ANDROID_KEYSTORE_BASE64` — base64-encoded `.jks`/`.keystore`
- `ANDROID_KEYSTORE_PASSWORD`, `ANDROID_KEY_PASSWORD`
- `ANDROID_KEY_ALIAS`

Generate a release keystore once and keep it safe — every installable update
must be signed by the same certificate:

```sh
keytool -genkeypair -v -keystore anchor.jks -alias varde-anchor \
  -keyalg RSA -keysize 3072 -validity 10000
base64 -w0 anchor.jks   # -> ANDROID_KEYSTORE_BASE64
```

`versionName` is the release version; `versionCode` is the build's epoch
seconds, which stays strictly increasing across nightly and release builds.
The signed asset lands as `Varde-<ver>-android.apk`; without the secrets the
pipeline still emits `Varde-<ver>-android-unsigned.apk` (not installable —
sign it locally with `apksigner` or set the secrets).

## Limits

- No self-update: `varde-agent update` on Android reports that updates are
  delivered by the app (APK upgrades replace the bundled binaries).
- No parent-death kill for the mesh child (PR_SET_PDEATHSIG is
  Linux-gated); Android killing the app removes the whole process anyway.
- The phone never hosts games — anchor nodes only store/serve saves, so
  none of the runtime/executor paths apply.
- Cellular = CGNAT: direct mesh paths are unlikely, so replication flows
  through the opaque relay. On home Wi-Fi, hole punching works as usual.
