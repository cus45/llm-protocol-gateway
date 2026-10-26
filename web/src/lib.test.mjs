// lib.tsx 纯函数的回归测试。前端没有测试运行器（web/package.json 只有 tsc + vite），
// 所以这里用 node:test 直接跑：把真实的 lib.tsx 用 tsc 编成 ESM 再去调用，
// 测的是仓库里那份代码本身，不是它的复制品。
//
//   node --test web/src/lib.test.mjs
//
// 编译产物落在系统临时目录，不会进仓库，也不会被 vite 打进 web/dist（只有
// index.html 是入口）。
import {test} from 'node:test';
import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {dirname, join} from 'node:path';
import {fileURLToPath, pathToFileURL} from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const webDir = dirname(here);
// tsc 直接用 node 跑它的 JS 入口：Windows 上 .bin/tsc.cmd 没法被 execFile 直接 spawn。
const tsc = join(webDir, 'node_modules', 'typescript', 'bin', 'tsc');
const outDir = mkdtempSync(join(tmpdir(), 'gw-lib-test-'));
process.on('exit', () => rmSync(outDir, {recursive: true, force: true}));

// 拿一个测试替身顶掉 lib.tsx 里的 react：这里只测纯函数，不渲染组件。
// （编译产物在系统临时目录，解析不到 web/node_modules，所以连加载器一起钉。）
writeFileSync(join(outDir, 'package.json'), JSON.stringify({type: 'module'}));
const stubDir = join(outDir, 'node_modules', 'react');
mkdirSync(stubDir, {recursive: true});
writeFileSync(join(stubDir, 'package.json'), JSON.stringify({name: 'react', type: 'module', main: 'index.js', exports: {
  '.': './index.js',
  './jsx-runtime': './jsx-runtime.js',
  './jsx-dev-runtime': './jsx-runtime.js',
}}));
writeFileSync(join(stubDir, 'jsx-runtime.js'), `
export const Fragment = (props) => props.children;
export const jsx = (type, props) => ({type, props});
export const jsxs = jsx;
export const jsxDEV = jsx;
`);
writeFileSync(join(stubDir, 'index.js'), `
export const createContext = (value) => ({Provider: ({children}) => children, _value: value});
export const useContext = () => undefined;
export const useState = (value) => [typeof value === 'function' ? value() : value, () => {}];
export const useEffect = () => {};
export const useLayoutEffect = () => {};
export const useMemo = (fn) => fn();
export const useCallback = (fn) => fn;
export const useRef = (value) => ({current: value});
export const useId = () => 'id';
export const Fragment = (props) => props.children;
export default {};
`);

execFileSync(process.execPath, [
  tsc,
  '--target', 'es2022', '--module', 'es2022', '--moduleResolution', 'bundler',
  '--jsx', 'react-jsx', '--skipLibCheck', '--ignoreConfig', '--outDir', outDir, join(here, 'lib.tsx'), join(here, 'pollers.ts')
], {cwd: webDir, stdio: 'inherit'});
const lib = await import(pathToFileURL(join(outDir, 'lib.js')).href);
// 编译产物里的 import './lib' 没有扩展名，ESM 解析不了：把它补成 './lib.js'。
writeFileSync(join(outDir, 'pollers.js'), readFileSync(join(outDir, 'pollers.js'), 'utf8').replace("from './lib'", "from './lib.js'"));
const pollers = await import(pathToFileURL(join(outDir, 'pollers.js')).href);

test('cloudflareAuthPollOutcome: 授权成功就收尾，不再等待', () => {
  assert.equal(lib.cloudflareAuthPollOutcome({authorized: true, elapsedMs: 0, timeoutMs: 600000}), 'authorized');
  // 超时与授权同时成立时，授权优先：用户刚点完授权不该被判超时。
  assert.equal(lib.cloudflareAuthPollOutcome({authorized: true, elapsedMs: 700000, timeoutMs: 600000}), 'authorized');
});

test('cloudflareAuthPollOutcome: 没授权且未超时继续等，超过时限收尾', () => {
  assert.equal(lib.cloudflareAuthPollOutcome({authorized: false, elapsedMs: 0, timeoutMs: 600000}), 'wait');
  assert.equal(lib.cloudflareAuthPollOutcome({authorized: false, elapsedMs: 600000, timeoutMs: 600000}), 'wait');
  assert.equal(lib.cloudflareAuthPollOutcome({authorized: false, elapsedMs: 600001, timeoutMs: 600000}), 'timeout');
});

// 驱动真实的 runCloudflareAuthPoll（pollers.ts）。两条契约：已授权的控制台查一次
// 就收尾、绝不留下定时器（每次 tick 都 setState 重渲染整个控制台）；检查在飞的
// 时候不叠加第二次（每 2 秒一次请求对慢后端会堆起来）。
function fakePollEnv(authorizedAnswers) {
  const timers = new Map();
  let nextId = 1;
  let calls = 0;
  const settledWith = [];
  return {
    timers,
    settledWith,
    calls: () => calls,
    env: {
      timeoutMs: 600000,
      now: () => 0,
      // 检查是异步的（真的要打后端），结果至少晚一个宏任务回来，和浏览器里
      // 一次网络往返一致：同步 resolve 会让「先查后装」和「先装后查」分辨不出来。
      checkStatus: () => {
        calls += 1;
        const answer = authorizedAnswers[calls - 1] ?? false;
        return new Promise((resolve) => setTimeout(() => resolve(answer), 0));
      },
      installTimer: (tick) => { const id = nextId++; timers.set(id, tick); return id; },
      clearTimer: (id) => { timers.delete(id); },
      onSettled: (authorized) => settledWith.push(authorized),
    },
    fire: () => { for (const tick of [...timers.values()]) tick(); },
  };
}
const settle = () => new Promise((r) => setTimeout(r, 0));

test('已授权：只查一次，收尾为成功，绝不留下定时器', async () => {
  // 这一条同时是旧顺序（先装定时器、再等第一次检查）的反例：那种写法里定时器
  // 在检查回来之前就已装上，收尾时若不在回调里清掉，它就会一直留在表里。
  const f0 = fakePollEnv([true]);
  pollers.runCloudflareAuthPoll(f0.env);
  assert.equal(f0.timers.size, 0, '第一次检查还没回来就装上了定时器：收尾那条路上没人清它');
  await settle();
  const f = fakePollEnv([true]);
  pollers.runCloudflareAuthPoll(f.env);
  await settle();
  assert.equal(f.calls(), 1);
  assert.deepEqual(f.settledWith, [true]);
  assert.equal(f.timers.size, 0, '已授权后不应安装定时器');
  // 即便旧实现装上过一只，收尾后的每一次跳也必须没有任何请求跟出来。
  f.fire();
  f.fire();
  await settle();
  assert.equal(f.calls(), 1, '收尾后定时器仍在校验后端');
});

test('未授权：安装定时器并继续查，授权后停止', async () => {
  const f = fakePollEnv([false, false, true]);
  pollers.runCloudflareAuthPoll(f.env);
  await settle();
  assert.equal(f.timers.size, 1, '未授权时应安装定时器');
  f.fire();
  await settle();
  assert.equal(f.timers.size, 1);
  f.fire();
  await settle();
  assert.equal(f.calls(), 3);
  assert.deepEqual(f.settledWith, [true]);
  assert.equal(f.timers.size, 0, '授权后定时器必须停掉');
});

test('检查还在飞时不叠加第二次，避免慢查询堆积请求', async () => {
  const timers = new Map();
  let nextId = 1;
  let calls = 0;
  let release;
  const gate = new Promise((r) => { release = r; });
  pollers.runCloudflareAuthPoll({
    timeoutMs: 600000,
    now: () => 0,
    checkStatus: () => { calls += 1; return gate.then(() => false); },
    installTimer: (tick) => { const id = nextId++; timers.set(id, tick); return id; },
    clearTimer: (id) => timers.delete(id),
    onSettled: () => {},
  });
  await settle();
  // 第一次还没回来，定时器已经装上：此刻再触发两次，不应再发请求
  for (const tick of [...timers.values()]) tick();
  for (const tick of [...timers.values()]) tick();
  await settle();
  assert.equal(calls, 1, '正在飞的那次没回来之前不应再发请求');
  release();
  await settle();
});

test('超时收尾为失败，并停掉定时器', async () => {
  const timers = new Map();
  let nextId = 1;
  let now = 0;
  const settled = [];
  pollers.runCloudflareAuthPoll({
    timeoutMs: 1000,
    now: () => now,
    checkStatus: async () => false,
    installTimer: (tick) => { const id = nextId++; timers.set(id, tick); return id; },
    clearTimer: (id) => timers.delete(id),
    onSettled: (authorized) => settled.push(authorized),
  });
  await settle();
  now = 1001;
  for (const tick of [...timers.values()]) tick();
  await settle();
  assert.deepEqual(settled, [false]);
  assert.equal(timers.size, 0);
});
