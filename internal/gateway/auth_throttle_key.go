package gateway

import (
	"net"
	"net/http"
	"strings"
)

// 登录限流的「来源」。限流键必须是网关自己观测到的、调用方伪造不了的东西：
//
//   - 直连的公网对端：就是它的 TCP 地址。它写在请求头里的 X-Forwarded-For 之类
//     一律不参与，否则每次换一个值就是一个新桶，限流形同虚设。
//   - 受信任的代理对端（回环/内网/链路本地：cloudflared、本机 nginx、Docker 桥）：
//     取转发头里最早的那个公网地址 —— 它由最外层的反代写入，内层链路无法改写。
//   - /__auth/* 在 loopback 上还有一条「本机免鉴权」旁路（登录页只对远程开放），
//     所以内网对端在本机旁路成立时的来源只按「内网」记账：否则谁都能报一个和
//     运维本机相同的转发地址，把运维锁在登录页外面。
//
// 账号维度另有一份全局计数（authThrottleAccountKey），与来源无关：来源可以变，
// 账号只有一个，两个维度任一到阈值都挡。
func authThrottleSource(r *http.Request) string {
	if r == nil {
		return ""
	}
	peer := net.ParseIP(stripHostPort(strings.TrimSpace(r.RemoteAddr)))
	if peer == nil {
		return ""
	}
	if !isTrustedProxyPeer(r.RemoteAddr) {
		return peer.String()
	}
	// 先看转发链里有没有公网地址：本机上的 cloudflared 把连接转给 127.0.0.1 时，
	// 对端同样是回环，但真正的调用方在转发链里。
	if forwarded := forwardedClientChain(r); len(forwarded) > 0 {
		for _, candidate := range forwarded {
			if !candidate.IsPrivate() && !candidate.IsLoopback() && !candidate.IsLinkLocalUnicast() {
				return candidate.String()
			}
		}
		// 转发链里全是内网/回环：按链尾（离网关最近的那一跳记录）算，而不是链首。
		// 链首是调用方自己就能写的第一段，谁都能报一个和运维本机相同的值把运维
		// 锁在登录页外面；链尾由最后的反代写入，离网关最近。回环地址与旁路来源
		// 记成同一个桶，报 127.0.0.1 也就落回运维自己。
		if last := forwarded[len(forwarded)-1]; last.IsLoopback() {
			return authThrottleInternalSource
		}
		return forwarded[len(forwarded)-1].String()
	}
	if isLocalAdminBypass(r) {
		return authThrottleInternalSource
	}
	return peer.String()
}

// forwardedClientChain reads the forwarded addresses in the order the hops wrote
// them: CF-Connecting-IP first (Cloudflare always writes it), then X-Forwarded-For
// left to right, then X-Real-IP. Values that are not addresses are dropped, so a
// client cannot smuggle text into the key.
func forwardedClientChain(r *http.Request) []net.IP {
	var chain []net.IP
	add := func(value string) {
		if ip := net.ParseIP(stripHostPort(strings.TrimSpace(value))); ip != nil {
			chain = append(chain, ip)
		}
	}
	add(r.Header.Get("CF-Connecting-IP"))
	for _, part := range strings.Split(r.Header.Get("X-Forwarded-For"), ",") {
		add(part)
	}
	add(r.Header.Get("X-Real-IP"))
	return chain
}

// authThrottleAccountKey is the account dimension of the same window,
// authThrottleAnyUserKey the per-source dimension that ignores which name was
// tried, and authAccountMaxFailures the account limit: higher than the per-source one because a
// single machine is already stopped by the source dimension, and this bucket is
// there for many sources against one account (and for changing the name to dodge
// a blocked source).
// authThrottleInternalSource is the source an internal peer gets when it could
// act as the loopback operator (the local admin bypass) or reports a loopback
// address: one bucket, so neither side can lock the other out.
const authThrottleInternalSource = "internal"

const (
	authThrottleAccountKey = "\x00account"
	authThrottleAnyUserKey = "\x00any"
	authAccountMaxFailures = 32
)
