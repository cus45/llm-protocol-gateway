package gateway

import (
	"net/http"
	"net/url"
	"strings"
)

// 浏览器跨域：模型协议端点（/v1、/openai/v1、/anthropic/v1）与 /__health 是给
// 第三方网页/插件直接调的，放行任意来源；管理端 /__* 只认网关自己的来源。
//
// 以前所有路径都回 Access-Control-Allow-Origin: *，而控制台现在同时用 HttpOnly
// 会话 Cookie 与自定义请求头，星号意味着任何网站都能带着用户的浏览器往
// 网关发跨域请求。管理端改成来源白名单：同源（按请求 Host 判断，含端口）永远
// 放行——生产由网关自己托管控制台，开发走 Vite 同源代理，都不需要额外配置。

// corsHeaders 是所有跨域请求都允许的请求头与方法。
const (
	corsAllowedHeaders = "Content-Type, Authorization, x-api-key, anthropic-version, anthropic-beta, anthropic-dangerous-direct-browser-access"
	corsAllowedMethods = "GET, POST, PATCH, DELETE, OPTIONS"
)

// corsPreflightMaxAge 让浏览器缓存预检结果，避免每个请求都 OPTIONS。
const corsPreflightMaxAge = "600"

// adminCORSOriginAllowed 判断管理端请求的 Origin 是否可信：与请求 Host 同源
// （含端口）。管理端不提供跨域配置入口——跨站调用控制台本身就是该拦下的行为。
func adminCORSOriginAllowed(origin string, r *http.Request) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	return sameHost(parsed.Host, r.Host)
}

// sameHost 忽略大小写比较 Host（含端口）；两边都带或不带端口时必须一致。
func sameHost(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == b {
		return true
	}
	return stripDefaultPort(a) == stripDefaultPort(b)
}

func stripDefaultPort(host string) string {
	switch {
	case strings.HasSuffix(host, ":80"):
		return strings.TrimSuffix(host, ":80")
	case strings.HasSuffix(host, ":443"):
		return strings.TrimSuffix(host, ":443")
	}
	return host
}

// withCORS 按路径决定跨域来源：模型协议端点放行任意来源，管理端只放行同源。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		path := r.URL.Path
		if isModelCORSRequest(path) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else if adminCORSOriginAllowed(origin, r) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		w.Header().Set("Access-Control-Allow-Headers", corsAllowedHeaders)
		w.Header().Set("Access-Control-Allow-Methods", corsAllowedMethods)
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Max-Age", corsPreflightMaxAge)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isModelCORSRequest 判断路径是否属于模型协议端点（需要跨域直连的）。
func isModelCORSRequest(path string) bool {
	trimmed := strings.TrimRight(strings.TrimSpace(path), "/")
	switch {
	case trimmed == "/__health":
		return true
	case trimmed == "/v1" || strings.HasPrefix(trimmed, "/v1/"):
		return true
	case strings.HasPrefix(trimmed, "/openai/v1/"):
		return true
	case strings.HasPrefix(trimmed, "/anthropic/v1/"):
		return true
	case trimmed == "/chat/completions":
		return true
	case strings.HasPrefix(trimmed, "/images/"):
		return true
	}
	return false
}
