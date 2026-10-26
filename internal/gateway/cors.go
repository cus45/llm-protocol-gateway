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
//
// 预检（OPTIONS）只在「这条路径真的接受预检里声明的那个方法」时才由中间件
// 回 204。以前任何 OPTIONS 都在 mux 之前回 204：控制台路由的存不存在一探便知
// （OPTIONS /__unknown 也是 204），错误方法的探测（POST /__state）还会被
// net/http 回 405 + Allow，等于免鉴权地列出路由表。其余 OPTIONS 交给 mux，
// 由它按真实路由回 405/404。Allow-Methods 也改成逐路径计算，只宣告这条路径
// 实际支持的方法；预检请求的可变头写进 Vary，避免缓存串味。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		path := r.URL.Path
		modelRequest := isModelCORSRequest(path)
		if modelRequest {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			// Model endpoints only take POST (the lists are GET, the health probe
			// answers without a method): a preflight answer is cached by the
			// browser and must not advertise methods the path cannot serve.
			w.Header().Set("Access-Control-Allow-Methods", "POST")
		} else if adminCORSOriginAllowed(origin, r) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", corsAllowedMethods)
		}
		w.Header().Set("Access-Control-Allow-Headers", corsAllowedHeaders)
		if r.Method == http.MethodOptions {
			w.Header().Add("Vary", "Access-Control-Request-Method")
			w.Header().Add("Vary", "Access-Control-Request-Headers")
			// A preflight names the method it is asking about; without one it is
			// not a cross-origin preflight at all, so the mux answers it like any
			// other unsupported method. Anything not answered below (no method
			// named, or a method the path does not accept) is forwarded to the
			// mux instead of being short-circuited here.
			requested := strings.ToUpper(strings.TrimSpace(r.Header.Get("Access-Control-Request-Method")))
			if requested != "" {
				if corsPathSupportsMethod(path, requested) {
					w.Header().Set("Access-Control-Max-Age", corsPreflightMaxAge)
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// corsPathSupportsMethod 回答「这条路径接受这个方法吗」。模型端点与鉴权中间件
// 同一套口径（isModelProtocolPath，另加几个只读的列表 GET）；控制台路由只认
// 控制台自己会发出的方法——对它们回 405 等于免鉴权地列出路由表，所以只要
// 方法合理就交给中间件回预检。
func corsPathSupportsMethod(path string, method string) bool {
	if isModelProtocolPath(path) {
		if method == http.MethodPost {
			return true
		}
		if method != http.MethodGet {
			return false
		}
		// Model lists are the only model-path GETs (the health probe additionally
		// serves any method through its own handler, but browsers only ever GET it).
		trimmed := strings.TrimRight(strings.TrimSpace(path), "/")
		return trimmed == "/__health" || strings.HasSuffix(trimmed, "/v1/models") || strings.HasSuffix(trimmed, "/models")
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	}
	return false
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
