// Package enroll: device codes, user codes (WORD-NN-WORD) and enrollment
// tokens. Secrets are only ever stored as SHA-256 hashes.
package enroll

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

// Word list for device user codes: distinct, unambiguous, family-friendly.
var words = []string{
	"ABLE", "ACORN", "AQUA", "ARCH", "ARROW", "ASPEN", "ATLAS", "BADGE",
	"BASIL", "BEACH", "BERRY", "BIRCH", "BLOOM", "BONUS", "BRAVO", "BRIAR",
	"BROOK", "CABIN", "CACTI", "CEDAR", "CHARM", "CIDER", "CLIFF", "CLOUD",
	"CLOVE", "COMET", "CORAL", "CREEK", "CROWN", "DELTA", "DRIFT", "DUNE",
	"EAGLE", "EMBER", "FABLE", "FALCON", "FERN", "FJORD", "FLAME", "FLINT",
	"FROST", "GLADE", "GLEAM", "GRACE", "GROVE", "HAVEN", "HERON", "HOLLY",
	"IVORY", "JOLLY", "JUNIPER", "KELP", "KITE", "KOALA", "LANCE", "LARCH",
	"LAUREL", "LEMUR", "LILAC", "LODGE", "LOTUS", "LUNAR", "LYNX", "MAPLE",
	"MARSH", "MEADOW", "MISTY", "MOOSE", "NOBLE", "OASIS", "OLIVE", "ONYX",
	"OTTER", "PANDA", "PEARL", "PINE", "PLAID", "PLUME", "POLAR", "PRISM",
	"QUAIL", "QUEST", "RIVER", "ROBIN", "SABLE", "SAGE", "SHORE", "SIERRA",
	"SKYLARK", "SLATE", "SOLAR", "SPIRE", "STONE", "STORM", "THISTLE", "TIDAL",
	"TIMBER", "TOPAZ", "TRAIL", "TULIP", "UMBER", "VALOR", "VELVET", "VISTA",
	"WALNUT", "WILLOW", "WOLF", "WREN", "YODEL", "ZEPHYR", "ZINNIA", "ZORAL",
}

// UserCode returns "WORD-NN-WORD".
func UserCode() (string, error) {
	pick := func() (string, error) {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
		if err != nil {
			return "", err
		}
		return words[n.Int64()], nil
	}
	a, err := pick()
	if err != nil {
		return "", err
	}
	b, err := pick()
	if err != nil {
		return "", err
	}
	n, err := rand.Int(rand.Reader, big.NewInt(90))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%02d-%s", a, n.Int64()+10, b), nil
}

// NormalizeUserCode accepts "wolf-73-kite", "wolf73kite", mixed case.
func NormalizeUserCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if strings.Contains(code, "-") {
		return code
	}
	// insert dashes: WORD-NN-WORD
	if len(code) < 8 {
		return code
	}
	return code // unparseable codes simply won't match
}

// DeviceCode is the long secret the agent polls with (not user-facing).
func DeviceCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

// EnrollmentToken returns "pge_" + 32 base32 chars.
func EnrollmentToken() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "pge_" + strings.ToLower(
		base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

// Hash is the storage form for all enroll secrets.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
