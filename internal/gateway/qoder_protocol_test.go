package gateway

import (
	"strings"
	"testing"
)

// qoderDecodeBody is the inverse of qoderEncodeBody, written here as an
// independent implementation rather than by reusing the encoder's internals, so
// a round-trip test says something about the codec and not only about itself.
func qoderDecodeBody(t *testing.T, wire string) string {
	t.Helper()
	third := len(wire) / 3
	if third > 0 {
		wire = wire[len(wire)-third:] + wire[third:len(wire)-third] + wire[:third]
	}
	var bits []int
	for group := 0; group+4 <= len(wire); group += 4 {
		values := make([]int, 0, 4)
		for _, char := range wire[group : group+4] {
			if char == '$' {
				continue
			}
			index := strings.IndexRune(qoderBodyAlphabet, char)
			if index < 0 {
				t.Fatalf("encoded body holds %q, which is not in Qoder's alphabet", char)
			}
			values = append(values, index)
		}
		if len(values) == 0 {
			continue
		}
		acc := 0
		for _, value := range values {
			acc = acc<<6 | value
		}
		bytesOut := (6 * len(values)) / 8
		for k := 0; k < bytesOut; k++ {
			shift := uint(6*len(values) - 8*(k+1))
			bits = append(bits, (acc>>shift)&0xFF)
		}
	}
	out := make([]byte, len(bits))
	for index, value := range bits {
		out[index] = byte(value)
	}
	return string(out)
}

func TestQoderEncodeBodyRoundTrips(t *testing.T) {
	cases := []string{
		"",
		`{"a":1}`,
		"hello 你好",
		strings.Repeat("x", 512),
		strings.Repeat("日本語テキスト", 40),
	}
	for _, plaintext := range cases {
		wire := qoderEncodeBody([]byte(plaintext))
		if len(wire)%4 != 0 {
			t.Fatalf("%q encoded to %d chars, not a multiple of four", plaintext, len(wire))
		}
		if got := qoderDecodeBody(t, wire); got != plaintext {
			t.Fatalf("round trip changed the body:\n got %q\nwant %q", got, plaintext)
		}
	}
}

// The outer-thirds swap is what tells Qoder's codec from ordinary base64: a
// body encoded without it is rejected by the endpoint.
func TestQoderEncodeBodySwapsOuterThirds(t *testing.T) {
	plaintext := []byte(`{"messages":[{"role":"user","content":"say something"}]}`)
	wire := qoderEncodeBody(plaintext)
	if !strings.HasSuffix(wire, "$") && len(wire)%4 != 0 {
		t.Fatalf("expected padding to a multiple of four, got %q", wire)
	}
	plain := qoderBodyAlphabetEncodeOnly(plaintext)
	third := len(plain) / 3
	want := plain[len(plain)-third:] + plain[third:len(plain)-third] + plain[:third]
	if wire != want {
		t.Fatalf("outer thirds not swapped:\n got %q\nwant %q", wire, want)
	}
}

// qoderBodyAlphabetEncodeOnly is the un-swapped half of the codec, kept here so
// the swap can be asserted against it.
func qoderBodyAlphabetEncodeOnly(data []byte) string {
	var out strings.Builder
	var acc, bits int
	for _, b := range data {
		acc = (acc << 8) | int(b)
		bits += 8
		for bits >= 6 {
			bits -= 6
			out.WriteByte(qoderBodyAlphabet[(acc>>bits)&0x3F])
		}
	}
	if bits > 0 {
		out.WriteByte(qoderBodyAlphabet[(acc<<(6-bits))&0x3F])
	}
	for out.Len()%4 != 0 {
		out.WriteByte('$')
	}
	return out.String()
}

func TestQoderSignaturePath(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://api3.qoder.sh/algo/api/v2/model/list?Encode=1", "/api/v2/model/list"},
		{"https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=1", "/api/v2/service/pro/sse/agent_chat_generation"},
		{"https://gateway.qoder.com.cn/api/v1/ping", "/api/v1/ping"},
		{"https://api3.qoder.sh/algo", ""},
	}
	for _, tc := range cases {
		if got := qoderSignaturePath(tc.raw); got != tc.want {
			t.Errorf("qoderSignaturePath(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// The envelope's shape is what the endpoint checks: a bearer of the form
// COSY.<payload>.<md5>, the wrapped key beside it, and the account named in
// Cosy-User. A made-up identity gets a 403 with nothing to explain it.
func TestQoderCOSYHeadersEnvelopeShape(t *testing.T) {
	endpoint := qoderGlobalSite.qoderModelsURL()
	headers, err := qoderCOSYHeaders(endpoint, qoderAccount{
		UID:       "user-1",
		Name:      "Example User",
		Email:     "user@example.com",
		Token:     "jt-token",
		MachineID: "machine-1",
	}, "", 1_700_000_000)
	if err != nil {
		t.Fatal(err)
	}
	authorization := headers["Authorization"]
	if !strings.HasPrefix(authorization, "Bearer COSY.") {
		t.Fatalf("unexpected Authorization: %q", authorization)
	}
	parts := strings.Split(strings.TrimPrefix(authorization, "Bearer COSY."), ".")
	if len(parts) != 2 {
		t.Fatalf("expected payload and signature, got %d parts", len(parts))
	}
	if len(parts[1]) != 32 {
		t.Fatalf("signature is not an md5 hex string: %q", parts[1])
	}
	for _, required := range []string{"Cosy-Key", "Cosy-Date", "Cosy-User", "Cosy-MachineId", "Cosy-Version"} {
		if strings.TrimSpace(headers[required]) == "" {
			t.Errorf("%s missing from the envelope", required)
		}
	}
	if headers["Cosy-User"] != "user-1" || headers["Cosy-MachineId"] != "machine-1" {
		t.Fatalf("account identity wrong: %+v", headers)
	}
	if headers["Cosy-Date"] != "1700000000" {
		t.Errorf("Cosy-Date should carry the signed timestamp, got %q", headers["Cosy-Date"])
	}
}

func TestQoderCOSYHeadersRefuseAnAccountWithoutAMachineID(t *testing.T) {
	if _, err := qoderCOSYHeaders(qoderGlobalSite.qoderChatURL(), qoderAccount{UID: "u", Token: "t"}, "", 0); err == nil {
		t.Fatal("expected an error when the account has no machine id")
	}
}

func TestQoderSiteForAuthType(t *testing.T) {
	global, ok := qoderSiteForAuthType("qoder_pat")
	if !ok || global.API != "https://api3.qoder.sh" || global.RedirectURI == "" {
		t.Fatalf("unexpected global site: %+v", global)
	}
	cn, ok := qoderSiteForAuthType("qoder_cn")
	if !ok || cn.API != "https://gateway.qoder.com.cn" || !cn.DeviceChatFallback || cn.RedirectURI != "" {
		t.Fatalf("unexpected Qoder CN site: %+v", cn)
	}
	if _, ok := qoderSiteForAuthType("trae_cn"); ok {
		t.Fatal("a Trae auth type must not resolve to a Qoder site")
	}
}
