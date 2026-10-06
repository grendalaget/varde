// Package netns holds the root-only Linux network-namespace e2e demo
// (networking.md §64): real NAT'd nodes behind per-node masquerade
// namespaces proving QUIC hole punching (DIRECT) and relay fallback
// (RELAYED). The test file carries the `netns` build tag and runs via
// `make e2e-netns`; it is intentionally not part of `make test`.
package netns
