package gateway

// Qoder's subscription protocol, ported from the Qoder support that magpie
// ships (internal/qoder, which ports CLIProxyAPI's reverse-engineered client,
// MIT). A Qoder account is served through the API Qoder's own desktop client
// talks to — not through a documented OpenAI-compatible endpoint — so every
// call has to carry the client's COSY envelope and its request body has to go
// out in the client's body codec.
//
// Qoder runs two sites whose accounts exist only on their own: qoder.com
// (Google/GitHub/email sign-in) and qoder.cn, "Qoder CN" (Alibaba Cloud or a
// phone number). Same protocol, different hosts and a different device-flow
// client id, hence the two connection kinds.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// Qoder's endpoints, relative to the site's three hosts (web = the sign-in
// page, openAPI = accounts and tokens, api = models and chat).
const (
	qoderDeviceSelectPath  = "/device/selectAccounts"
	qoderDevicePollPath    = "/api/v1/deviceToken/poll"
	qoderDeviceRefreshPath = "/api/v1/deviceToken/refresh"
	qoderJobTokenPath      = "/api/v1/me/jobToken"
	qoderJobRefreshPath    = "/api/v1/jobToken/refresh"
	qoderJobExchangePath   = "/api/v1/jobToken/exchange"
	qoderUserInfoPath      = "/api/v1/userinfo"
	qoderUsagePath         = "/sash/api/v2/me/usage"

	// qoderChatPath asks for the LLM result to be fetched, names the common
	// agent, and Encode=1 says the request body arrives in Qoder's own codec.
	// The reply is plain server-sent events: only the request is encoded.
	qoderChatPath   = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	qoderModelsPath = "/algo/api/v2/model/list?Encode=1"

	// qoderCOSYVersion is the client version the envelope claims to be. The
	// signature covers it, so it must match what the endpoint expects.
	qoderCOSYVersion = "1.1.49"
)

// qoderSite is one Qoder deployment.
type qoderSite struct {
	// AuthType is the connection kind a provider of this site is stored under.
	AuthType string
	Name     string
	Web      string
	OpenAPI  string
	API      string
	// ClientID is the device-flow client the sign-in names. Qoder CN's is the
	// one Qoder CN's own CLI sends, because Qoder CN may not have registered
	// the desktop client's id.
	ClientID string
	// RedirectURI goes on the authorization page; Qoder CN's CLI sends none.
	RedirectURI string
	// DeviceChatFallback marks a site where a refused job-token exchange is
	// expected rather than fatal: Qoder CN's CLI chats on the device token
	// itself, never making a job token, so an account there can work the same
	// way when qoder.cn won't trade the device token (a 4xx).
	DeviceChatFallback bool
}

var (
	qoderGlobalSite = qoderSite{
		AuthType:    domain.AuthTypeQoderPAT,
		Name:        "Qoder",
		Web:         "https://qoder.com",
		OpenAPI:     "https://openapi.qoder.sh",
		API:         "https://api3.qoder.sh",
		ClientID:    "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa",
		RedirectURI: "qoder-app://",
	}
	qoderCNSite = qoderSite{
		AuthType:           domain.AuthTypeQoderCN,
		Name:               "Qoder CN",
		Web:                "https://qoder.cn",
		OpenAPI:            "https://openapi.qoder.com.cn",
		API:                "https://gateway.qoder.com.cn",
		ClientID:           "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb",
		DeviceChatFallback: true,
	}
)

func qoderSiteForAuthType(authType string) (qoderSite, bool) {
	switch authType {
	case domain.AuthTypeQoderPAT:
		return qoderGlobalSite, true
	case domain.AuthTypeQoderCN:
		return qoderCNSite, true
	default:
		return qoderSite{}, false
	}
}

// qoderSiteOf is the site a provider signs in on. Anything that is not the CN
// kind is the global site, matching qoderSiteForAuthType's default.
func qoderSiteOf(provider domain.Provider) qoderSite {
	site, _ := qoderSiteForAuthType(provider.AuthType)
	return site
}

func isQoderAuthType(authType string) bool {
	_, ok := qoderSiteForAuthType(authType)
	return ok
}

func isQoderProvider(provider domain.Provider) bool {
	return isQoderAuthType(provider.AuthType)
}

// qoderChatURL is the site's inference endpoint.
func (s qoderSite) qoderChatURL() string { return s.API + qoderChatPath }

// qoderModelsURL is the site's model-listing endpoint.
func (s qoderSite) qoderModelsURL() string { return s.API + qoderModelsPath }

// qoderRSAPublicKeyPEM is Qoder's embedded 1024-bit RSA public key, dumped from
// the client's COSY source. It wraps the per-request AES key that encrypts the
// account blob inside the Authorization header — the endpoint checks the
// envelope, so a made-up key yields 403 rather than a useful error.
const qoderRSAPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

var qoderRSAKey = mustQoderRSAKey()

func mustQoderRSAKey() *rsa.PublicKey {
	block, _ := pem.Decode([]byte(qoderRSAPublicKeyPEM))
	if block == nil {
		panic("qoder: failed to decode embedded RSA public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		panic("qoder: parse RSA public key: " + err.Error())
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		panic("qoder: embedded key is not an RSA public key")
	}
	return key
}

// qoderAccount is the signed-in account a COSY envelope is built for.
type qoderAccount struct {
	UID       string
	Name      string
	Email     string
	Token     string
	MachineID string
}

// qoderCOSYHeaders signs a call to rawURL whose body is already in its wire
// (codec) form; an empty body is what a GET signs.
func qoderCOSYHeaders(rawURL string, account qoderAccount, wireBody string, timestamp int64) (map[string]string, error) {
	if account.MachineID == "" {
		return nil, fmt.Errorf("qoder: 该账号没有 machine id，请重新登录")
	}
	if account.UID == "" || strings.TrimSpace(account.Token) == "" {
		return nil, fmt.Errorf("qoder: 签名缺少 uid 或令牌")
	}
	if timestamp == 0 {
		timestamp = time.Now().Unix()
	}
	info, key, err := qoderUserBlob(account)
	if err != nil {
		return nil, err
	}
	payloadJSON, err := json.Marshal(map[string]any{
		"version":     "v1",
		"requestId":   randomHex(16),
		"info":        info,
		"cosyVersion": qoderCOSYVersion,
		"ideVersion":  "",
	})
	if err != nil {
		return nil, fmt.Errorf("qoder: marshal COSY payload: %w", err)
	}
	payload := base64.StdEncoding.EncodeToString(payloadJSON)
	signature := qoderCOSYSignature(payload, key, timestamp, wireBody, qoderSignaturePath(rawURL))
	machineID := account.MachineID
	return map[string]string{
		"Accept":                "application/json",
		"Accept-Encoding":       "identity",
		"Content-Type":          "application/json",
		"Authorization":         "Bearer COSY." + payload + "." + signature,
		"Cosy-Business-Product": "app",
		"Cosy-Business-Type":    "agent",
		"Cosy-ClientIp":         machineID,
		"Cosy-ClientType":       "10",
		"Cosy-Data-Policy":      "disagree",
		"Cosy-Date":             fmt.Sprintf("%d", timestamp),
		"Cosy-Key":              key,
		"Cosy-MachineId":        machineID,
		"Cosy-MachineToken":     machineID,
		"Cosy-MachineType":      "5",
		"Cosy-MachineOS":        qoderMachineOS(),
		"Cosy-Scene":            "app",
		"Cosy-User":             account.UID,
		"Cosy-Version":          qoderCOSYVersion,
		"Login-Version":         "v2",
	}, nil
}

// qoderSignaturePath is the path the signature covers: no query, and no
// leading /algo — the gateway host serves /algo/x but signs it as /x.
func qoderSignaturePath(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return strings.TrimSpace(rawURL)
	}
	path := parsed.Path
	if strings.HasPrefix(path, "/algo") {
		path = strings.TrimPrefix(path, "/algo")
	}
	return path
}

func qoderCOSYSignature(payload, key string, timestamp int64, wireBody, path string) string {
	sum := md5.Sum([]byte(fmt.Sprintf("%s\n%s\n%d\n%s\n%s", payload, key, timestamp, wireBody, path)))
	return hex.EncodeToString(sum[:])
}

// qoderUserBlob is the account as the envelope carries it: JSON encrypted with
// AES-128-CBC under a fresh key that doubles as its own IV, that key then
// wrapped with Qoder's RSA public key. Both halves come back base64'd.
func qoderUserBlob(account qoderAccount) (info string, key string, err error) {
	raw, err := json.Marshal(map[string]string{
		"uid":                  account.UID,
		"aid":                  "",
		"name":                 account.Name,
		"email":                account.Email,
		"security_oauth_token": account.Token,
	})
	if err != nil {
		return "", "", fmt.Errorf("qoder: marshal user blob: %w", err)
	}
	// The AES key doubles as the IV, and is 16 hex characters because that is
	// the length the client's key has.
	aesKey := []byte(randomHex(8))
	if len(aesKey) != aes.BlockSize {
		return "", "", fmt.Errorf("qoder: 生成 COSY 的 AES 密钥失败")
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", "", fmt.Errorf("qoder: aes cipher: %w", err)
	}
	padLen := aes.BlockSize - len(raw)%aes.BlockSize
	padded := append(append([]byte{}, raw...), bytes.Repeat([]byte{byte(padLen)}, padLen)...)
	encrypted := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, aesKey).CryptBlocks(encrypted, padded)
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, qoderRSAKey, aesKey)
	if err != nil {
		return "", "", fmt.Errorf("qoder: rsa encrypt key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(encrypted), base64.StdEncoding.EncodeToString(wrapped), nil
}

// qoderBodyAlphabet is Qoder's shuffled base64 alphabet, dumped from a live
// client. '_' is index 0 and '!' index 63; '$' pads without carrying data.
const qoderBodyAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"

// qoderEncodeBody is the wire form of a request body: standard base64
// grouping (four chars per three bytes, MSB-first bit stream) through Qoder's
// alphabet, padded to a multiple of four with '$', then its first and last
// thirds swapped. The endpoint decodes the same way and rejects anything else.
func qoderEncodeBody(plaintext []byte) string {
	var out strings.Builder
	var acc, bits int
	for _, b := range plaintext {
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
	encoded := out.String()
	third := len(encoded) / 3
	if third == 0 {
		return encoded
	}
	return encoded[len(encoded)-third:] + encoded[third:len(encoded)-third] + encoded[:third]
}

// qoderMachineOS names this machine the way Qoder's desktop clients do:
// Windows is "win32", and the architectures carry their own spellings.
func qoderMachineOS() string {
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "386":
		arch = "x86"
	case "arm64":
		arch = "aarch64"
	}
	os := runtime.GOOS
	if os == "windows" {
		os = "win32"
	}
	return arch + "_" + os
}
