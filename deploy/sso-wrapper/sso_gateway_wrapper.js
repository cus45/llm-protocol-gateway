/**
 * 协议网关 (llm-protocol-gateway) 的 SSO 认证包装器。
 *
 * 部署形态：
 *   Cloudflare Tunnel (user.lucadesign.uk)
 *     -> 本包装器 (127.0.0.1:SSO_PORT)
 *       -> Go 网关自身 (127.0.0.1:GATEWAY_TARGET_PORT，有自己的账号密码系统)
 *
 * 网关本身已经有一套完整的账号密码 + session cookie（bcrypt 密码，
 * cookie 名 gateway_admin_session）。本包装器不替换它，而是叠加两条路径：
 *
 * 1. 独立登录（保持现状）：直接把请求原样转发给网关，网关自己的登录页/
 *    接口正常工作，用管理员密码登录，网关自己签发 cookie。
 * 2. 首页一键透传：URL 带 ?sso_token=xxx 时，本包装器用共享 SSO_SECRET
 *    校验令牌（签名 / 有效期 / 一次性防重放），通过后**代替用户调用网关
 *    自己的 /__auth/login 接口**换取网关官方 session cookie，再把这个
 *    cookie 转发给浏览器 —— 用户拿到的就是网关原生的 admin 会话，跟手动
 *    输入密码登录效果完全一致。
 *
 * 运行：
 *   GATEWAY_ADMIN_PASSWORD=xxx SSO_SECRET=xxx node sso_gateway_wrapper.js
 */

import { createHmac, timingSafeEqual } from 'node:crypto';
import { createServer } from 'node:http';
import { request as httpRequest } from 'node:http';

const SSO_PORT = Number(process.env.SSO_PORT || 18094);
const GATEWAY_TARGET = process.env.GATEWAY_TARGET || 'http://127.0.0.1:18093';
const GATEWAY_ADMIN_USERNAME = process.env.GATEWAY_ADMIN_USERNAME || 'admin';
const GATEWAY_ADMIN_PASSWORD = process.env.GATEWAY_ADMIN_PASSWORD || '';
const SSO_SECRET = process.env.SSO_SECRET || '';
const PUBLIC_BASE = (process.env.PUBLIC_BASE || 'https://user.lucadesign.uk').replace(/\/$/, '');

const targetUrl = new URL(GATEWAY_TARGET);

// 已使用过的一次性令牌 nonce（进程内存即可，令牌本身只有 120 秒有效期）。
const usedNonces = new Set();

if (!SSO_SECRET) {
  console.warn('[gateway-sso] Warning: SSO_SECRET not set, portal jump-through disabled.');
}
if (!GATEWAY_ADMIN_PASSWORD) {
  console.warn('[gateway-sso] Warning: GATEWAY_ADMIN_PASSWORD not set, portal jump-through disabled.');
}

function base64urlDecode(value) {
  return Buffer.from(value, 'base64url').toString('utf8');
}

function sign(payload) {
  return createHmac('sha256', SSO_SECRET).update(payload).digest('base64url');
}

function safeEqual(a, b) {
  const left = Buffer.from(a);
  const right = Buffer.from(b);
  return left.length === right.length && timingSafeEqual(left, right);
}

/** 校验首页签发的一次性跳转令牌；通过返回 true，否则返回 false。 */
function verifySsoToken(token) {
  if (!SSO_SECRET || !token || !token.includes('.')) return false;
  const [payload, signature] = token.split('.');
  if (!payload || !signature || !safeEqual(signature, sign(payload))) return false;

  try {
    const data = JSON.parse(base64urlDecode(payload));
    if (Number(data.expiresAt) < Date.now()) return false;
    if (usedNonces.has(data.nonce)) return false; // 一次性令牌不可重放

    const moduleUrl = String(data.moduleUrl || '');
    if (!moduleUrl.startsWith(PUBLIC_BASE)) return false; // 令牌不是签给本模块的

    usedNonces.add(data.nonce);
    if (usedNonces.size > 10000) {
      const it = usedNonces.values();
      for (let i = 0; i < 5000; i += 1) usedNonces.delete(it.next().value);
    }
    return true;
  } catch {
    return false;
  }
}

/** 代替用户调用网关自己的 /__auth/login，换取网关原生 session cookie。 */
function loginToGatewayAsAdmin() {
  return new Promise((resolve, reject) => {
    const body = JSON.stringify({ username: GATEWAY_ADMIN_USERNAME, password: GATEWAY_ADMIN_PASSWORD });
    const req = httpRequest(
      {
        hostname: targetUrl.hostname,
        port: targetUrl.port,
        path: '/__auth/login',
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Content-Length': Buffer.byteLength(body)
        }
      },
      (res) => {
        const chunks = [];
        res.on('data', (chunk) => chunks.push(chunk));
        res.on('end', () => {
          if (res.statusCode !== 200) {
            reject(new Error(`gateway login failed: ${res.statusCode} ${Buffer.concat(chunks)}`));
            return;
          }
          const setCookie = res.headers['set-cookie'];
          if (!setCookie || !setCookie.length) {
            reject(new Error('gateway login did not return a session cookie'));
            return;
          }
          resolve(setCookie);
        });
      }
    );
    req.on('error', reject);
    req.write(body);
    req.end();
  });
}

function proxyRequest(req, res) {
  const headers = { ...req.headers };
  delete headers.host;
  delete headers.connection;

  const proxyReq = httpRequest(
    {
      hostname: targetUrl.hostname,
      port: targetUrl.port,
      path: req.url,
      method: req.method,
      headers
    },
    (proxyRes) => {
      res.writeHead(proxyRes.statusCode || 502, proxyRes.headers);
      proxyRes.pipe(res);
    }
  );

  proxyReq.on('error', (err) => {
    console.error('[gateway-sso] proxy error:', err.message);
    if (!res.headersSent) {
      res.writeHead(502, { 'Content-Type': 'text/plain; charset=utf-8' });
    }
    res.end('上游网关不可达');
  });

  req.pipe(proxyReq);
}

const server = createServer(async (req, res) => {
  const url = new URL(req.url || '/', `http://${req.headers.host || 'localhost'}`);
  const ssoToken = url.searchParams.get('sso_token');

  if (ssoToken) {
    if (!SSO_SECRET || !GATEWAY_ADMIN_PASSWORD) {
      res.writeHead(503, { 'Content-Type': 'text/plain; charset=utf-8' });
      res.end('SSO 透传未配置完整（缺少 SSO_SECRET 或 GATEWAY_ADMIN_PASSWORD）。');
      return;
    }
    if (!verifySsoToken(ssoToken)) {
      res.writeHead(401, { 'Content-Type': 'text/plain; charset=utf-8' });
      res.end('跳转令牌无效或已过期，请从首页重新进入。');
      return;
    }

    try {
      const gatewayCookies = await loginToGatewayAsAdmin();
      const cleanUrl = new URL(url.toString());
      cleanUrl.searchParams.delete('sso_token');
      res.writeHead(302, {
        Location: cleanUrl.pathname + (cleanUrl.search || ''),
        'Set-Cookie': gatewayCookies
      });
      res.end();
    } catch (err) {
      console.error('[gateway-sso] admin auto-login failed:', err.message);
      res.writeHead(502, { 'Content-Type': 'text/plain; charset=utf-8' });
      res.end('自动登录网关失败，请直接使用网关密码登录。');
    }
    return;
  }

  proxyRequest(req, res);
});

server.listen(SSO_PORT, '127.0.0.1', () => {
  console.log(`[gateway-sso] listening on 127.0.0.1:${SSO_PORT} -> ${GATEWAY_TARGET}`);
});
