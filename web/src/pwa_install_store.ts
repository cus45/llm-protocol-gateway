// 「安装到桌面」（PWA）的安装事件捕获与状态。
//
// 浏览器只在满足条件时（HTTPS 或 localhost、有可用的 manifest 与 Service Worker）
// 触发一次 beforeinstallprompt；event.preventDefault() 把它拦下来，手动调用
// prompt() 才能展示浏览器原生安装框。事件只发一次，所以必须在应用挂载前就监听，
// 否则首屏渲染慢一点就永远等不到。
import {useSyncExternalStore} from 'react';
import {registerSW} from 'virtual:pwa-register';

export const PWA_INSTALL_DISMISSED_KEY = 'gateway:pwa-install-dismissed';

export interface BeforeInstallPromptEvent extends Event {
    prompt: () => Promise<void>;
    userChoice: Promise<{outcome: 'accepted' | 'dismissed'; platform: string}>;
}

export type PwaInstallState = {
    canInstall: boolean;
    installed: boolean;
};

export type PwaInstallOutcome = 'accepted' | 'dismissed' | 'unavailable' | 'insecure';

export function isPwaInstalled(env: {displayMode: boolean; standalone: boolean}): boolean {
    return env.displayMode || env.standalone;
}

let promptEvent: BeforeInstallPromptEvent | null = null;
let state: PwaInstallState = {canInstall: false, installed: false};
let started = false;
const listeners = new Set<() => void>();

function emit(next: PwaInstallState) {
    state = next;
    listeners.forEach((listener) => listener());
}

export function startPwaInstallCapture(): void {
    if (started || typeof window === 'undefined') return;
    started = true;
    state = {
        canInstall: false,
        installed: isPwaInstalled({
            displayMode: window.matchMedia('(display-mode: standalone)').matches,
            standalone: (window.navigator as Navigator & {standalone?: boolean}).standalone === true,
        }),
    };
    window.addEventListener('beforeinstallprompt', (raw) => {
        raw.preventDefault();
        promptEvent = raw as BeforeInstallPromptEvent;
        emit({canInstall: !state.installed, installed: state.installed});
    });
    window.addEventListener('appinstalled', () => {
        promptEvent = null;
        emit({canInstall: false, installed: true});
    });
}

/** 生产构建才会生成 sw.js；dev 下 registerSW 自动空转。 */
export function registerPwaServiceWorker(): void {
    registerSW({immediate: true});
}

/** 用户点过「以后再说」：同一浏览器不再弹出。 */
export function isPwaInstallDismissed(): boolean {
    try {
        return window.localStorage.getItem(PWA_INSTALL_DISMISSED_KEY) === '1';
    } catch {
        return false;
    }
}

export function dismissPwaInstall(): void {
    try {
        window.localStorage.setItem(PWA_INSTALL_DISMISSED_KEY, '1');
    } catch {
        /* 隐私模式下写不进去，本次关掉即可 */
    }
}

export async function promptPwaInstall(): Promise<PwaInstallOutcome> {
    if (!window.isSecureContext) {
        // http 下浏览器根本不触发安装事件，只能提示用户换地址
        return 'insecure';
    }
    if (!promptEvent) return 'unavailable';
    try {
        await promptEvent.prompt();
        const {outcome} = await promptEvent.userChoice;
        promptEvent = null;
        if (outcome === 'accepted') {
            emit({canInstall: false, installed: true});
        } else {
            emit({canInstall: false, installed: state.installed});
        }
        return outcome;
    } catch {
        return 'unavailable';
    }
}

function subscribe(listener: () => void) {
    listeners.add(listener);
    return () => {
        listeners.delete(listener);
    };
}

function getSnapshot(): PwaInstallState {
    return state;
}

export function usePwaInstall(): PwaInstallState {
    return useSyncExternalStore(subscribe, getSnapshot, getSnapshot);
}

/** 测试用：不开渲染器也能读到当前状态。 */
export function usePwaInstallSnapshot(): PwaInstallState {
    return getSnapshot();
}

/** 测试用：模块状态是单例，用例之间要清空。 */
export function resetPwaInstallForTest(): void {
    promptEvent = null;
    state = {canInstall: false, installed: false};
    started = false;
    listeners.clear();
}
