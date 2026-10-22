package gateway

import (
	"io"
	"net/http"
	"strings"
)

// gatewayRequestBodyLimit caps one client request body. A Claude Code turn with
// a large tool result, or an image-heavy request, is tens of MB; 256 MB leaves
// that room many times over while stopping a malformed or hostile client from
// making the gateway buffer without bound (io.ReadAll on r.Body used to grow
// until the process died).
const gatewayRequestBodyLimit int64 = 256 << 20

// readGatewayRequestBody reads a capped request body. tooLarge reports the
// cap was hit, so callers can answer 413 in their own protocol instead of a
// generic 400 "invalid json".
func readGatewayRequestBody(r *http.Request) (body []byte, tooLarge bool, err error) {
	if r == nil || r.Body == nil {
		return nil, false, nil
	}
	// 多读一个字节判超限：限流读不依赖底层是否可计数，错误也是我们的类型。
	body, err = io.ReadAll(io.LimitReader(r.Body, gatewayRequestBodyLimit+1))
	if err != nil {
		// 中间件已用 MaxBytesReader 包过：超限时读错误来自它，按超限处理。
		if r.ContentLength > gatewayRequestBodyLimit ||
			strings.Contains(err.Error(), "request body too large") ||
			strings.Contains(err.Error(), "http: request body too large") {
			return nil, true, nil
		}
		return nil, false, err
	}
	if int64(len(body)) > gatewayRequestBodyLimit {
		// 不把 256MB 原始字节留在内存里给上层用。
		return nil, true, nil
	}
	return body, false, nil
}

const requestBodyTooLargeMessage = "request body exceeds the gateway limit of 256 MiB"

// withBodyLimit wraps every request body in MaxBytesReader so a handler that
// reads it (directly or via json.Decoder) can't buffer past the cap.
func withBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, gatewayRequestBodyLimit)
		}
		next.ServeHTTP(w, r)
	})
}

// writeTooLargeError answers 413 when a handler's body read hit the cap.
func writeTooLargeError(w http.ResponseWriter) {
	writeOpenAIError(w, http.StatusRequestEntityTooLarge, requestBodyTooLargeMessage)
}
