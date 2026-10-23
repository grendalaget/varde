// Package iceguard decides which gathered ICE candidates may leave the SFU. The rewrite rule
// covers host candidates only, so addresses are checked on the way out as well (GRYT-768).
package iceguard

// Allowed reports whether a candidate carrying this address may be sent. An empty advertise
// list allows everything; with a list, membership is exact — not a prefix or a subnet.
func Allowed(address string, advertise []string) bool {
	if len(advertise) == 0 {
		return true
	}

	for _, allowed := range advertise {
		if address == allowed {
			return true
		}
	}

	return false
}
