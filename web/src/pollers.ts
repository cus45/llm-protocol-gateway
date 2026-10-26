// 控制台里那些「先查一次、再决定要不要装定时器」的轮询：判定逻辑抽成纯函数放在这里，
// 让 web/src/lib.test.mjs 能直接测到真实代码（前端没有测试运行器，见该文件说明）。
import {cloudflareAuthPollOutcome} from './lib';

/** 授权轮询一次检查的结果：还没授权就继续等，授权成功或超时则收尾。 */
export type CloudflareAuthPollOutcome = 'wait' | 'authorized' | 'timeout'

/**
 * waitForCloudflareAuthorization 的驱动：先检查一次，只有仍在等待时才装上
 * 2 秒的定时器；检查没回来就不叠第二次（慢查询下请求会堆起来）。
 *
 * 早先第一次检查是 `void check()`（不等它的结果）就装了定时器，而 check 内部
 * 收尾时清的是「当时已经装上的那只」：控制台本来就已经授权时，第一次检查在
 * 多数情况下比 `installTimer` 更晚回来，装上来的定时器再也没人停 —— 每 2 秒
 * 请求一次 status（成功后还连带取 zones），每次都 setState 重渲染整个控制台，
 * 直到关掉标签页。
 */
export function runCloudflareAuthPoll(input: {
  timeoutMs: number
  now: () => number
  checkStatus: () => Promise<boolean>
  installTimer: (tick: () => void) => number
  clearTimer: (id: number) => void
  onSettled: (authorized: boolean) => void
}): {stop: () => void} {
  const started = input.now();
  let timer: number | null = null;
  let settled = false;
  let checking = false;
  const stop = () => {
    if (timer != null) {
      input.clearTimer(timer);
      timer = null;
    }
  };
  const finish = (authorized: boolean) => {
    if (settled) return;
    settled = true;
    stop();
    input.onSettled(authorized);
  };
  const check = async () => {
    if (settled || checking) return;
    checking = true;
    try {
      const authorized = await input.checkStatus();
      switch (cloudflareAuthPollOutcome({authorized, elapsedMs: input.now() - started, timeoutMs: input.timeoutMs})) {
        case 'authorized':
          finish(true);
          return;
        case 'timeout':
          finish(false);
          return;
      }
    } catch {
      // 一次检查抛错不该让轮询停摆：不装定时器的话「等待授权」会一直转，直到关掉
      // 标签页。抛错当作「还没授权」继续等，超时仍由 timeoutMs 收口。
    } finally {
      checking = false;
    }
  };
  // 等第一次检查回来再决定装不装：它在飞的时候停掉轮询，回来后也会看到
  // settled 而不会补装一只没人管的定时器。
  void check().then(() => {
    if (!settled && timer == null) {
      timer = input.installTimer(() => {
        void check();
      });
    }
  });
  return {stop};
}
