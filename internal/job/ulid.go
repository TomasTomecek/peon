package job

import (
	"io"
	"math/big"
	"regexp"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var idRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// NewID returns a ULID: a 48-bit millisecond timestamp followed by 80
// random bits, in Crockford base32. IDs sort by creation time.
func NewID(t time.Time, rnd io.Reader) (string, error) {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := io.ReadFull(rnd, b[6:]); err != nil {
		return "", err
	}
	n := new(big.Int).SetBytes(b[:])
	thirtyTwo := big.NewInt(32)
	mod := new(big.Int)
	out := make([]byte, 26)
	for i := 25; i >= 0; i-- {
		n.DivMod(n, thirtyTwo, mod)
		out[i] = crockford[mod.Int64()]
	}
	return string(out), nil
}

// ValidID reports whether s is a well-formed job ID.
func ValidID(s string) bool { return idRe.MatchString(s) }
