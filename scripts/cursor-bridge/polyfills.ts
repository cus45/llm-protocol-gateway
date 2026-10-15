// 老 node 的运行时补丁。@bufbuild/protobuf v2 用 Promise.withResolvers
// （Node 22+ / ES2024）；部署机上的 node 20 没有它，RPC 会在运行期直接抛错。
// 这个模块必须最先被引入，保证任何 protobuf 调用发生前补丁已就位。
if (typeof (Promise as { withResolvers?: unknown }).withResolvers !== "function") {
  (Promise as unknown as { withResolvers: () => { promise: Promise<unknown>; resolve: (v: unknown) => void; reject: (e: unknown) => void } }).withResolvers = function withResolvers() {
    let resolve!: (value: unknown) => void;
    let reject!: (reason: unknown) => void;
    const promise = new Promise((res, rej) => {
      resolve = res;
      reject = rej;
    });
    return { promise, resolve, reject };
  };
}

export {};
