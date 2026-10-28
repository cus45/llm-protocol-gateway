import {spawn} from 'node:child_process';
import type {Connect} from 'vite';
import {defineConfig, type Plugin} from 'vite';
import {dirname, join} from 'node:path';
import {fileURLToPath} from 'node:url';
import react from '@vitejs/plugin-react';
import {VitePWA} from 'vite-plugin-pwa';

const gatewayTarget = 'http://127.0.0.1:18093';
const gatewayBind = process.env.GATEWAY_ADDR || '0.0.0.0:18093';
const rootDir = dirname(fileURLToPath(new URL('.', import.meta.url)));
const repoDir = dirname(rootDir);
const ensureGatewayScript = join(repoDir, 'scripts', 'ensure-gateway.sh');

async function gatewayIsHealthy() {
  try {
    const response = await fetch(`${gatewayTarget}/__health`);
    if (!response.ok) return false;
    const body = await response.text();
    return body.includes('"status":"ok"');
  } catch {
    return false;
  }
}

function runEnsureGateway(): Promise<void> {
  return new Promise((resolve, reject) => {
    const child = spawn('bash', [ensureGatewayScript], {
      cwd: repoDir,
      env: { ...process.env, GATEWAY_ADDR: gatewayBind },
      stdio: 'inherit',
    });
    child.on('error', reject);
    child.on('exit', (code) => {
      if (code === 0) resolve();
      else reject(new Error(`ensure-gateway.sh exited with code ${code ?? 'null'}`));
    });
  });
}

function gatewayRecoveryMiddleware(): Connect.NextHandleFunction {
  let recovering = false;
  return (req, _res, next) => {
    const url = req.url ?? '';
    if (!url.startsWith('/__')) {
      next();
      return;
    }
    void (async () => {
      if (await gatewayIsHealthy()) {
        next();
        return;
      }
      if (recovering) {
        next();
        return;
      }
      recovering = true;
      try {
        console.warn(`[gateway-dev] ${url} requested while gateway down, recovering...`);
        await runEnsureGateway();
      } catch (error) {
        console.error(`[gateway-dev] recovery failed: ${String(error)}`);
      } finally {
        recovering = false;
        next();
      }
    })();
  };
}

function gatewayDevPlugin(): Plugin {
  let watchdogTimer: NodeJS.Timeout | undefined;
  let ensuring = false;

  async function ensureGatewayRunning(reason: string) {
    if (ensuring) return;
    if (await gatewayIsHealthy()) return;
    ensuring = true;
    try {
      console.warn(`[gateway-dev] ${reason}, running ensure-gateway.sh`);
      await runEnsureGateway();
      console.log(`[gateway-dev] gateway ready at ${gatewayTarget}`);
    } catch (error) {
      console.error(`[gateway-dev] failed to start gateway: ${String(error)}`);
    } finally {
      ensuring = false;
    }
  }

  function startWatchdog() {
    if (watchdogTimer) return;
    watchdogTimer = setInterval(() => {
      void ensureGatewayRunning('gateway unhealthy');
    }, 2000);
  }

  return {
    name: 'gateway-dev',
    apply: 'serve',
    configureServer(server) {
      if (process.env.GATEWAY_DEV_SKIP === '1') return;
      server.middlewares.use(gatewayRecoveryMiddleware());
      void ensureGatewayRunning('dev server starting');
      startWatchdog();
    },
  };
}

// 版本号由 scripts/build-version.sh 通过 GATEWAY_APP_VERSION 注入；未设置时为空串，
// 前端据此判定为开发构建，不提示升级。
const appVersion = (process.env.GATEWAY_APP_VERSION ?? '').trim();

export default defineConfig({
  plugins: [
    react(),
    gatewayDevPlugin(),
    VitePWA({
      registerType: 'autoUpdate',
      injectRegister: false,
      devOptions: { enabled: false },
      manifest: {
        name: 'LLM 协议网关',
        short_name: '协议网关',
        description: 'LLM Protocol Gateway 管理与调度控制台',
        lang: 'zh-CN',
        start_url: '/',
        scope: '/',
        display: 'standalone',
        launch_handler: { client_mode: 'focus-existing' },
        background_color: '#080c16',
        theme_color: '#0f1424',
        icons: [
          { src: '/icon-192.png', sizes: '192x192', type: 'image/png' },
          { src: '/icon-512.png', sizes: '512x512', type: 'image/png' },
        ],
      },
      workbox: {
        // 只预缓存带 hash 的构建产物：index.html 必须每次都上网取，
        // 否则升级提示刷新后仍是旧页，看不到新版本。
        globPatterns: ['assets/**/*.{js,css}', '*.{ico,svg,png,ttf}'],
        navigateFallback: null,
        cleanupOutdatedCaches: true,
      },
    }),
  ],
  define: {
    __APP_VERSION__: JSON.stringify(appVersion),
  },
  server: {
    host: '127.0.0.1',
    port: 5173,
    proxy: {
      '/__': { target: gatewayTarget, changeOrigin: true },
    },
  },
  preview: {
    host: '127.0.0.1',
    port: 4173,
    proxy: {
      '/__': { target: gatewayTarget, changeOrigin: true },
    },
  },
});
