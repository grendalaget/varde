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
CP/agent wiring will call this new RPC separately from `SetHostedServices`.

`--dev-http` is loopback-only, for tests. Production requires a configured
certificate/key; mesh IPC never permits insecure WebSocket URLs.
