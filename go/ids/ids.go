// Package ids generates prefixed resource identifiers: a lowercase prefix
// identifying the resource kind plus 20 lowercase base32 random characters.
package ids

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

const (
	alphabet   = "abcdefghijklmnopqrstuvwxyz234567" // RFC 4648 base32, lowercase
	randomSize = 20
)

// Prefixes for resource kinds.
const (
	User       = "usr_"
	Group      = "grp_"
	Node       = "node_"
	Server     = "srv_"
	Deployment = "dep_"
	Execution  = "exec_"
	Snapshot   = "snap_"
	Service    = "svc_"
)

// New returns "<prefix><20 random base32 chars>", e.g. "srv_k7xq...z".
func New(prefix string) (string, error) {
	out := make([]byte, randomSize)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("ids: read random: %w", err)
		}
		out[i] = alphabet[n.Int64()]
	}
	return prefix + string(out), nil
}

// Must is New that panics on failure; suitable for tests and init code.
func Must(prefix string) string {
	id, err := New(prefix)
	if err != nil {
		panic(err)
	}
	return id
}
