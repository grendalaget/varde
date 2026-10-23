# Spike (a): Gryt server on embeddable JavaScript runtimes

## Result

The server bundle runs on the box's Node 22 and serves both HTTP and Socket.IO.
The Node 18 trial passes the initial `node:sqlite` failure with a small
`better-sqlite3` shim, but then fails at import time because Node 18 lacks the
global `File` expected by the bundled `undici`. The Node 22 `--jitless` trial
fails in Node's internal Undici with `WebAssembly is not defined`. Bare 1.36.0
could not parse/load the current bundle, so its HTTP, Socket.IO, and SQLite
paths were not reached.

All work was done in `/home/ubuntu/spikes/gryt-server` or
`/home/ubuntu/spikes/bare-try`. The source trees `/tmp/gryt` and
`~/repos/varde` were not modified.

## Node 22 baseline

Built and bundled the isolated server copy with Yarn (`yarn install
--frozen-lockfile`, `yarn build`, `yarn bundle`). With Node `v22.23.4`,
`DATA_DIR` in the spike copy, `GRYT_IDENTITY_TIERS=local`, uploads disabled,
and no SFU configured, the benchmark helper recorded:

| Measure | Result |
|---|---:|
| Startup to first successful `/health` | 1,030.4 ms |
| Idle server RSS | 222,672 KiB |
| Socket.IO | connected |
| Sequential `GET /health` requests | 200 |
| Throughput | 3,087.41 requests/s |

The helper and run settings are in `gryt-server/scripts/spike-bench.mjs`.
An earlier run with the SFU host configured measured 1,246.7 ms startup,
223,420 KiB RSS, and 1,315.20 `/health` requests/s.

`/info` was initially used for the throughput trial, but request 61 returned
HTTP 429. The public HTTP limiter is 60 requests per 60 seconds, so the
benchmark switched to `/health`. The baseline still included a successful
Socket.IO connection. Expected local-only warnings included missing STUN/SFU
configuration and the Avahi service-file permission fallback to
`bonjour-service`.

## Node 22 `--jitless`

Tried first with the harness and server under `--jitless`, then with only the
server child under `--jitless`. The second attempt was repeated with the SFU
host unset; the same failure occurred, so the SFU configuration was not
required to reproduce it. Node logged:

```text
Warning: disabling flag --expose_wasm due to conflicting flags
...
node:internal/deps/undici/undici:6052
        mod = await WebAssembly.compile(llhttpWasmData || require_llhttp_wasm());
...
ReferenceError: WebAssembly is not defined
```

The server logged its listener-ready message, but exited before the helper
recorded a successful health check, RSS, Socket.IO connection, or throughput.
The failure is in Node's internal Undici path; the particular application call
that triggers it was not isolated. Metrics are therefore unavailable for this
configuration.

## Node 18.20.4

The official Linux x64 v18.20.4 archive was downloaded and its SHA-256 matched
the official `SHASUMS256.txt`. Running the bundle first failed with:

```text
Error [ERR_UNKNOWN_BUILTIN_MODULE]: No such built-in module: node:sqlite
```

For a compatibility probe, Yarn added `better-sqlite3@11.10.0` to the isolated
server copy. `scripts/node18-sqlite-shim.cjs` maps `node:sqlite` to a minimal
`DatabaseSync` subclass and exposes `isTransaction` from
`better-sqlite3`'s `inTransaction`. The bundle then advanced past SQLite
module loading but failed before server startup:

```text
ReferenceError: File is not defined
    at node_modules/undici/lib/web/webidl/index.js
```

Node 18.20.4 has `File` under `node:buffer` but not as a global. This shows
that the SQLite builtin is not the only Node 18 compatibility issue. No
HTTP/Socket.IO or performance measurements were possible after this failure.

## Bare

Installed `bare@1.36.0`, `bare-runtime@1.36.0`, `bare-node-runtime@1.5.1`, and
`bare-sqlite@0.1.5` in `bare-try` using Yarn. The `bare-node-runtime` README
documents loading `bare-node-runtime/global` and passing
`{ with: { imports: 'bare-node-runtime/imports' } }` when requiring a package;
the trial used that import-map form. The import map already aliases
`node:sqlite` to `bare-sqlite`, along with Node aliases for HTTP, net, DNS,
TLS, zlib, and worker threads.

Bare itself and `bare-node-runtime/global` loaded, but requiring the server
bundle failed before its entry point ran:

```text
Uncaught SyntaxError: Invalid or unexpected token
    at ScriptModule._createFunction
    at .../bare-module/lib/module.js
```

No server startup, SQLite, HTTP, or Socket.IO behavior was reached. The loader
error did not identify a source location, so whether it is the bundle's CJS
shape or a particular syntax construct remains unresolved. Since the attempt
stopped before module resolution reached the server's runtime dependencies,
the Bare import-map aliases do not establish that those APIs work for this
bundle. No performance measurements are available.

## Phone-sensitive runtime dependencies

Paths below are in the isolated copy (`gryt-server/src`):

| Dependency | Evidence | Startup behavior / phone concern |
|---|---|---|
| `sharp` | `routes/server.ts:8`; `index.ts:35` imports that router; additional uses include `services/avatarImage.ts:2` and `jobs/emojiQueueWorker.ts:2` | Statically imported while the entry point loads, including when uploads are disabled. Native image-processing bindings are a mobile-runtime portability concern. |
| `jsdom` | `utils/svgSanitize.ts:7,11`; imported by `routes/server.ts:20` and `routes/uploads.ts:21`, both imported by `index.ts:35,37` | Static import and process-level `JSDOM` creation during module loading; not gated by whether an upload request occurs. |
| `bonjour-service` / mDNS | `mdns.ts:131-155` | Loaded dynamically after discovery is enabled and the Avahi service-file attempt fails (`mdns.ts:40-52, 57-80`). The dynamic import is inside `try/catch`, so failure is warned and tolerated; mDNS itself is optional for serving. |
| `child_process` | Search of production `src` for `child_process`, `execFile`, `execSync`, `spawn`, and `fork` found no matches | No production use found in the searched server source. |

## Artifacts and caveats

- Server copy and benchmark helper: `/home/ubuntu/spikes/gryt-server`
- Node 18 SQLite shim: `/home/ubuntu/spikes/gryt-server/scripts/node18-sqlite-shim.cjs`
- Bare attempt: `/home/ubuntu/spikes/bare-try`
- Report: `/home/ubuntu/spikes/REPORT-a.md`

These are feasibility probes, not production changes. The Node 18 and Bare
results identify early blockers only; their later runtime surfaces remain
untested.

## Round 2

### Node 22 `--jitless`: startup fetch/Undici path and benchmark

A preload getter captured the startup read of global `fetch` and its stack:

```text
refreshWorkerCapabilities
startWorkerCapabilityPolling
Server.<anonymous> (listen callback)
emitListeningNT
```

`src/index.ts:492-493` starts `startWorkerCapabilityPolling()` in the listen
callback. That calls `refreshWorkerCapabilities()` unconditionally at
`src/services/workerCapabilities.ts:48-51`; its default argument at line 26
reads `fetch` before the function can return for the unset
`IMAGE_WORKER_URL` at lines 27-30. This startup read is enough to initialize
Node's internal Undici path, even though no worker fetch is needed.

Stubbing that global read alone exposed a second eager import path:
`src/utils/publicOnlyAgent.ts:5` imports `undici`, reached through
`previewUrlSafety` / `richActivity` / `memberCard` / socket utilities; the
externally loaded `jsdom` also imports Undici (`src/utils/svgSanitize.ts:7`).
Those modules still failed under `--jitless` because Undici's HTTP/1 parser
uses WebAssembly. For the benchmark only, a separate bundle aliased `undici`
and `jsdom` to local stubs, and a preload replaced global `fetch` with a
logging stub. `sharp` remained external. This leaves outbound preview fetching
and SVG sanitization untested/disabled in this run.

With that spike-only bundle, Node 22.23.4 `--jitless` reached health and
Socket.IO:

| Measurement | Result |
|---|---:|
| Startup to healthy `/health` | 414.7 ms |
| Idle RSS | 112,844 KiB |
| Socket.IO connect | connected |
| Sequential `/health` requests | 200 |
| `/health` throughput | 2,195.34 requests/s |

The global fetch trace saw the worker-capability default-argument read above;
no network fetch was called. I skipped 50 `chat:send` round-trips: the harness
only has an anonymous Socket.IO client, and it does not establish the local
identity/authentication flow required for that event.

### Node 18.20.4: post-`File` failures and adapted benchmark

Added the requested
`globalThis.File ??= require('node:buffer').File` to the isolated SQLite shim.
The unmodified bundle then progressed beyond the prior `File` failure and
failed in Undici 8 because Node 18 lacks
`worker_threads.markAsUncloneable`:

```text
TypeError: webidl.util.markAsUncloneable is not a function
    at new _CacheStorage (.../dist/bundle.js:48158:21)
```

A spike-only no-op shim for that worker-thread method exposed the next
unmodified-bundle failure: external `jsdom`'s `html-encoding-sniffer` tried to
`require()` ESM-only `@exodus/bytes/encoding-lite.js`, producing
`ERR_REQUIRE_ESM` before startup. For a further diagnostic, I reused the
separate `undici`/`jsdom` stub bundle from the jitless trial and rebuilt the
copy's `better-sqlite3` native addon for Node 18 (the Node 22 binary had ABI
127; Node 18 requires ABI 108). This let the service answer `/health` and
accept Socket.IO, but startup still logged `Server identity initialization
failed: crypto is not defined` from the bundled `jose` key-generation path.

Measurements below therefore apply to the adapted bundle with outbound
preview/SVG functions stubbed, the Node 18 SQLite shim/addon, and the worker
thread compatibility shim; they are not an as-is Node 18 compatibility
result:

| Measurement | Node 18.20.4 | Node 18.20.4 `--jitless` |
|---|---:|---:|
| Startup to healthy `/health` | 416.7 ms | 419.6 ms |
| Idle RSS | 112,176 KiB | 106,488 KiB |
| Socket.IO connect | connected | connected |
| Sequential `/health` requests | 200 | 200 |
| `/health` throughput | 2,846.33 requests/s | 2,531.43 requests/s |

The jitless run used the same global-fetch getter/stub and emitted the same
startup read stack. Server-identity initialization remains a known failure in
both adapted Node 18 runs.

### Bare 1.36.0: syntax-error diagnosis

The current Node 22 bundle starts with `"use strict";` and has no hashbang.
Using the `bare-node-runtime` README's documented
`require(..., { with: { imports: 'bare-node-runtime/imports' } })` form, both
the `.js` bundle and a copied `.cjs` bundle passed parsing and entered module
initialization. Both reached the same failure:

```text
TypeError: Cannot read properties of undefined (reading 'HTTP2_HEADER_AUTHORITY')
    at node_modules/undici/lib/dispatcher/client-h2.js
    at node_modules/undici/lib/dispatcher/client.js
    at dist/utils/publicOnlyAgent.js
```

`bare-node-runtime/imports.json` maps `node:http2` to its unsupported-module
stub, so Undici's eager HTTP/2 import is the first reachable Bare blocker.
The earlier `SyntaxError: Invalid or unexpected token` did not reproduce with
the documented import-map loader; the original invocation is unavailable, so
its exact cause remains unverified. Stripping a hashbang was unnecessary
because this bundle has none.

A minimal Express + Socket.IO HTTP app bundled with the same esbuild settings
(`--bundle --platform=node --target=node22 --format=cjs`) loaded without a
syntax or module-resolution error and kept running for the 15-second probe.
That probe did not issue an HTTP request, so successful health serving and
Socket.IO connectivity remain unverified. No Bare benchmark was attempted.
