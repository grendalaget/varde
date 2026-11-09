# Public gateway contract

`apps/gateway` terminates wildcard HTTPS and serves `<label>.<domain>`. Hosts
connect outbound to `wss://<gateway>/v1/tunnel` with `Authorization: Bearer <token>`.
There are no URL/query credentials. The CP key flag accepts base64 (standard or
raw URL encoding) or an HTTP(S) JSON public-key endpoint, like the relay.

`go/gatewaytoken.Claims` JSON fields are `aud` (exactly `varde.gateway.v1`),
`domain`, `label`, `group_id`, `server_id`, `epoch` (positive uint64), and
`exp_unix_ms` (exclusive Unix-millisecond expiry). Domain and label are lowercase
DNS names; the label is one DNS component. Token wire format is
`base64url(JSON) + "." + base64url(Ed25519 signature over JSON)`, without padding,
matching relay tokens but with a distinct required audience and claims.
`gatewaytoken.Sign` and `gatewaytoken.Verify` are the shared signer/verifier API.

The CP must allocate monotonically increasing epochs **per label**, including
across host/server/group reassignment. A higher epoch immediately replaces the
old tunnel and closes its streams. An equal epoch may reconnect only for the
same group/server; lower epochs remain fenced even after disconnect/expiry.
Expiry closes existing tunnels and upgrades, not only subsequent handshakes.
Fences live for the gateway process lifetime; see deployment restrictions.

WebSocket binary messages carry yamux: gateway is yamux **client**, mesh is
yamux **server**. Each upstream TCP connection is one stream. HTTP/1.1 pooling
and upgraded browser/fleet sockets use the same transport; yamux heartbeats run
every 15 seconds. Forwarded headers are regenerated from the public connection,
never trusted from the internet. AT's Caddy trusts only loopback peers.

Mesh IPC adds `varde.mesh.v1.MeshService/SetGatewayTunnels`, with request
`SetGatewayTunnelsRequest { repeated GatewayTunnel tunnels = 1; }` and empty
`SetGatewayTunnelsResponse`. `GatewayTunnel` fields:

1. `service_id` string: replacement-map identity, unique and nonempty.
2. `gateway_url` string: `wss://<gateway>/v1/tunnel`, no userinfo/query/fragment.
3. `token` string: opaque signed capability; never logged.
4. `target_port` uint32: agent-allocated local TCP port on `127.0.0.1`.

Calls are full replacements; an empty list removes all tunnels. Validation is
all-or-nothing. Token-only changes reconnect immediately; endpoint/port changes
replace the client. Node shutdown closes tunnels and local connections.
`go/gatewaytunnel.Client` exposes `New(Config)`, `Run(context.Context)` and
`UpdateToken(string)`; reconnects use jittered exponential backoff (250 ms–30 s).
The agent reconciles this RPC separately from `SetHostedServices` and withdraws
tunnels on stop, fencing, or lease expiry. The target is the mapped local port,
not necessarily the catalog's service port.

`--dev-http` is loopback-only, for tests. Production requires a configured
certificate/key; mesh IPC never permits insecure WebSocket URLs.

## Control-plane configuration

For a separate gateway, start the control plane with:

```sh
varde-control-plane --gateway url=wss://gateway.play.varde.games/v1/tunnel,domain=play.varde.games
```

The environment equivalent is `VARDE_GATEWAY`. Configure the separate gateway's
`--control-plane-key` with the CP's `/v1/relay/public-key` endpoint: gateway and
relay tokens reuse the persisted CP Ed25519 key, with different audiences.

Alternatively, run the TLS gateway in the CP process:

```sh
varde-control-plane --embedded-gateway --gateway-domain play.varde.games \
  --gateway-addr :443 --gateway-tls-cert wildcard.pem --gateway-tls-key wildcard.key
```

The corresponding environment variables are `VARDE_EMBEDDED_GATEWAY`,
`VARDE_GATEWAY_DOMAIN`, `VARDE_GATEWAY_ADDR`, `VARDE_GATEWAY_TLS_CERT`, and
`VARDE_GATEWAY_TLS_KEY`. Embedded mode advertises the hostname from the CP's
`--public-url` and the gateway listener's port. If that is not an agent-reachable
address, also supply `--gateway` with the public tunnel URL and the same domain.
Production uses `play.varde.games`. Wildcard DNS must point `*.play.varde.games`
at the gateway, and its certificate must cover `*.play.varde.games`, including
`gateway.play.varde.games` when that is the advertised tunnel endpoint. A wildcard
certificate does not cover the bare `play.varde.games` name; advertise a covered
hostname or include the bare name as another certificate subject.

Catalog games declare a TCP `gateway_port` and can set `exclude_anchors` to require
a compatible group PC. Auto Tournament sets both; without a gateway configured,
create/start fails with `gateway_unconfigured`. Placement failure returns
`group_pc_required`, which clients can display as “Tournaments need a PC in your
group”.

Each server's immutable `gateway_label` is persisted independently of its name:
the canonical random server ID's underscore becomes a hyphen. This preserves its
100 bits of randomness and is injective over canonical IDs; the database also
enforces label uniqueness. Existing servers are backfilled by the same rule.
Allocated labels always start with `srv-`, so server names cannot claim reserved
hostnames such as `gateway`, `www`, `cp`, `api`, `auth`, `admin`, or `docs`.
Keep operator endpoints outside the reserved `srv-` application namespace.
The server API exposes `public_url = https://<gateway_label>.<domain>`, and the CP
overrides the driver's `public_url` with it in desired state.

Only the active serving lease receives an `ExecutionDirective.gateway` containing
`gateway_url`, `token`, `service_port`, and `expires_at_unix_ms`. Tokens carry the
lease epoch. Stable eight-minute refresh windows produce two-to-ten-minute token
lifetimes, refreshing at least two minutes before expiry without reconnecting on
every heartbeat. Lease fencing withdraws the tunnel independently of token expiry.

## Browser isolation

The gateway drops every upstream `Set-Cookie` header that contains a `Domain`
attribute, including mixed-case attributes, empty values and leading dots.
Host-only cookies pass through unchanged, including multiple separate cookies.
Dropping rather than rewriting fails closed without changing an app's intended
cookie scope. Apps must omit `Domain` to use cookies through the gateway.
Legacy `Set-Cookie2` and all `Clear-Site-Data` headers are removed too: the latter's
cookie clearing can reach the entire registrable domain. The same filtering
applies to trailers and WebSocket upgrade response headers.

HSTS `includeSubDomains` can cover only descendants of the hosted hostname,
not `play.varde.games` or its siblings. CORS, CSP, service workers, redirects and
alternative services do not grant an app authority over another origin.
No shared-domain CORS or authentication trust should be granted to hosted apps.

These hosts still share the `varde.games` site, not an independent security
boundary. App JavaScript can try to write unprefixed parent-domain cookies;
response filtering cannot prevent that. The site must use Secure, host-only
`__Host-` authentication cookies and validate request origins against an explicit
allowlist. Do not accept old domain-wide auth cookies as a migration fallback.
Cookie quota/header flooding and same-site requests remain risks of hosting
untrusted apps beneath the same registrable domain.
Custom site `/api/cp/*` mutation handlers need their own Origin/CSRF guard;
Better Auth's endpoint allowlist does not cover them. Do not enable untrusted
wildcard apps until that guard is in place and legacy site sessions are revoked.
