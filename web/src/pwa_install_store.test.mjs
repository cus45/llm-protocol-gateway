// pwa_install_store.ts 的回归测试。沿用 lib.test.mjs 的办法：把真实源码用 tsc
// 编成 ESM 再跑，react 与 virtual:pwa-register 用替身顶掉（这里只测状态机，不渲染）。
// 替身类型声明在 .test-stubs/，由 pwa_install_store.test.tsconfig.json 用 paths 指过去。
//
//   node --test web/src/pwa_install_store.test.mjs
import {test} from 'node:test';
import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {dirname, join} from 'node:path';
import {fileURLToPath, pathToFileURL} from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const webDir = dirname(here);
const tsc = join(webDir, 'node_modules', 'typescript', 'bin', 'tsc');
const outDir = mkdtempSync(join(tmpdir(), 'gw-pwa-test-'));
process.on('exit', () => rmSync(outDir, {recursive: true, force: true}));

writeFileSync(join(outDir, 'package.json'), readFileSync(join(here, 'pwa_install_store.test.imports.json')));

// 编译后的 ESM 解析不到 web/node_modules，两个依赖都用替身顶掉。
const stubDir = join(outDir, 'node_modules');
mkdirSync(join(stubDir, 'react'), {recursive: true});
writeFileSync(join(stubDir, 'react', 'package.json'), JSON.stringify({name: 'react', type: 'module', main: 'index.js'}));
writeFileSync(join(stubDir, 'react', 'index.js'), 'export const useSyncExternalStore = () => undefined;\n');
// Node 的 ESM 加载器不认 virtual: 协议，产物里的 virtual:pwa-register 改指本地替身。
writeFileSync(join(outDir, 'pwa-register.js'), 'export const registerSW = () => () => {};\n');

execFileSync(process.execPath, [
  tsc, '--project', join(here, 'pwa_install_store.test.tsconfig.json'),
  '--outDir', outDir, '--ignoreDeprecations', '6.0',
], {cwd: webDir, stdio: 'inherit'});
const compiled = join(outDir, 'pwa_install_store.js');
writeFileSync(compiled, readFileSync(compiled, 'utf8').replaceAll("'virtual:pwa-register'", "'#pwa-register'"));

const store = await import(pathToFileURL(join(outDir, 'pwa_install_store.js')).href);

function fakeWindow() {
  const listeners = new Map();
  return {
    isSecureContext: true,
    matchMedia: () => ({matches: false}),
    navigator: {standalone: false},
    localStorage: {
      values: new Map(),
      getItem(key) {
        return this.values.get(key) ?? null;
      },
      setItem(key, value) {
        this.values.set(key, String(value));
      },
    },
    addEventListener(type, listener) {
      listeners.set(type, listener);
    },
    fire(type, event) {
      listeners.get(type)?.(event);
    },
  };
}

function installEvent(outcome) {
  let promptCalls = 0;
  return {
    get promptCalls() {
      return promptCalls;
    },
    preventDefault() {},
    async prompt() {
      promptCalls += 1;
    },
    userChoice: Promise.resolve({outcome, platform: 'web'}),
  };
}

async function withFakeWindow(fn) {
  const original = Object.getOwnPropertyDescriptor(globalThis, 'window');
  const win = fakeWindow();
  Object.defineProperty(globalThis, 'window', {configurable: true, writable: true, value: win});
  store.resetPwaInstallForTest();
  store.startPwaInstallCapture();
  try {
    return await fn(win);
  } finally {
    if (original) Object.defineProperty(globalThis, 'window', original);
    else delete globalThis.window;
  }
}

test('installed means either standalone display mode or iOS standalone flag', () => {
  assert.equal(store.isPwaInstalled({displayMode: true, standalone: false}), true);
  assert.equal(store.isPwaInstalled({displayMode: false, standalone: true}), true);
  assert.equal(store.isPwaInstalled({displayMode: false, standalone: false}), false);
});

test('beforeinstallprompt is captured and the browser default is suppressed', async () => {
  await withFakeWindow(async (win) => {
    const event = installEvent('dismissed');
    let prevented = false;
    event.preventDefault = () => {
      prevented = true;
    };
    win.fire('beforeinstallprompt', event);
    assert.equal(prevented, true);
    assert.equal(store.usePwaInstallSnapshot().canInstall, true);
  });
});

test('accepted install clears the entry and a dismissed one cannot be replayed', async () => {
  await withFakeWindow(async (win) => {
    const event = installEvent('accepted');
    win.fire('beforeinstallprompt', event);
    assert.equal(await store.promptPwaInstall(), 'accepted');
    assert.deepEqual(store.usePwaInstallSnapshot(), {canInstall: false, installed: true});
    assert.equal(await store.promptPwaInstall(), 'unavailable');
    assert.equal(event.promptCalls, 1);
  });
});

test('dismissed choice hides the entry until the next install event', async () => {
  await withFakeWindow(async (win) => {
    win.fire('beforeinstallprompt', installEvent('dismissed'));
    assert.equal(await store.promptPwaInstall(), 'dismissed');
    assert.deepEqual(store.usePwaInstallSnapshot(), {canInstall: false, installed: false});
  });
});

test('a throwing prompt() keeps the event so the user can retry', async () => {
  await withFakeWindow(async (win) => {
    const event = installEvent('accepted');
    event.prompt = async () => {
      throw new Error('not allowed');
    };
    win.fire('beforeinstallprompt', event);
    assert.equal(await store.promptPwaInstall(), 'unavailable');
    assert.equal(store.usePwaInstallSnapshot().canInstall, true);
  });
});

test('plain http reports insecure instead of pretending to be unavailable', async () => {
  await withFakeWindow(async (win) => {
    win.isSecureContext = false;
    win.fire('beforeinstallprompt', installEvent('accepted'));
    assert.equal(await store.promptPwaInstall(), 'insecure');
  });
});

test('dismissal is remembered in localStorage and tolerates blocked storage', async () => {
  await withFakeWindow(async (win) => {
    assert.equal(store.isPwaInstallDismissed(), false);
    store.dismissPwaInstall();
    assert.equal(win.localStorage.getItem(store.PWA_INSTALL_DISMISSED_KEY), '1');
    assert.equal(store.isPwaInstallDismissed(), true);

    win.localStorage.getItem = () => {
      throw new Error('blocked');
    };
    win.localStorage.setItem = () => {
      throw new Error('blocked');
    };
    assert.equal(store.isPwaInstallDismissed(), false);
    assert.doesNotThrow(() => store.dismissPwaInstall());
  });
});
