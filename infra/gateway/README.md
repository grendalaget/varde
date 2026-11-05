# Public gateway (not deployed)

Build context is the repository root, as for `infra/cp`. Fly requires `fly.toml`.
Replace `servers.example.com` with the public domain, point wildcard DNS at the
app, and provision `GATEWAY_TLS_CERT` / `GATEWAY_TLS_KEY` as Fly file secrets
containing the wildcard PEM certificate and private key. The certificate must
also cover the tunnel hostname (e.g. `gateway.<domain>`). The CP public key URL
returns `{ "public_key": "<base64 Ed25519>" }`, like the relay endpoint.

Run **one machine**, with no autostop. Epoch fencing is process-local, including
disconnected-label high-water marks; independent gateways cannot safely share
a label. Replicated/persistent lease routing is future work. A restart loses
high-water marks, so the CP must use short-lived capabilities and persist epoch
allocation. No CP issuance or agent directive wiring is included in this PR.

Public port 443 is raw TCP; Go terminates TLS, not Fly's HTTP proxy. Metrics on
9090 are private (no public Fly service). Metrics bind the VM's IPv4 interface
for Fly's configured Prometheus scraper, not the organization's IPv6 6PN.
Do not add a public service for the metrics port. The default body cap is 2 GiB per
request, uniformly across labels; `--max-body-bytes` and `--max-streams` tune the
limits. No hostname/group/token labels are emitted in metrics.

See `docs/gateway.md` for the signed capability and mesh IPC contracts.
