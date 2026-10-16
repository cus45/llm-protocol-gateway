package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// credentialJSON renders a subscription credential for the store's oauth_json
// column. A credential that cannot be encoded is stored as nothing: the
// sign-in then has to be done again, rather than the gateway trusting a
// half-written session on the next start.
func credentialJSON(credential any) string {
	raw, err := json.Marshal(credential)
	if err != nil {
		return ""
	}
	return string(raw)
}

// randomHex returns n random bytes as hex, "" if the system source failed.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

// newUUIDV4 is a v4 UUID in the 8-4-4-4-12 form. Trae and ZCode both name a
// device or a request with one, and both read the string back out of a JSON
// field they cannot type.
func newUUIDV4() string {
	buf := []byte(randomHex(16))
	if len(buf) != 32 {
		return ""
	}
	// version 4 in the high nibble of time_hi_and_version, RFC 4122 variant in
	// the two high bits of clock_seq_hi_and_reserved.
	buf[12] = '4'
	switch buf[16] {
	case '0', '1', '2', '3':
		buf[16] = '8'
	case '4', '5', '6', '7':
		buf[16] = '9'
	case '8', '9':
		buf[16] = 'a'
	default:
		buf[16] = 'b'
	}
	var out strings.Builder
	out.Grow(36)
	for i := 0; i < 32; i++ {
		if i == 8 || i == 12 || i == 16 || i == 20 {
			out.WriteByte('-')
		}
		out.WriteByte(buf[i])
	}
	return out.String()
}
