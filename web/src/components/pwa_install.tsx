// 「安装到桌面」外壳：侧栏入口 + 右下角提议卡片 + 由浏览器原生安装框决定的结果提示。
import {useState} from 'react';
import {dismissPwaInstall, isPwaInstallDismissed, promptPwaInstall, usePwaInstall} from '../pwa_install_store';

function InstallGlyph({size = 16}: {size?: number}) {
    return (
        <svg viewBox="0 0 24 24" width={size} height={size} fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <path d="M12 3v11"/>
            <path d="M7.5 9.5 12 14l4.5-4.5"/>
            <path d="M5 17v2a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2v-2"/>
        </svg>
    );
}

function PwaGlyph({size = 34}: {size?: number}) {
    return <img src="/logo.svg" width={size} height={size} alt="" aria-hidden="true"/>;
}

export function PwaInstallButton() {
    const {canInstall, installed} = usePwaInstall();
    const [busy, setBusy] = useState(false);

    // 只有浏览器真的发来了安装事件才显示；装好后入口消失
    if (!canInstall || installed) return null;

    const install = async () => {
        setBusy(true);
        await promptPwaInstall();
        setBusy(false);
    };

    return (
        <button
            type="button"
            className="pwa-install-entry"
            disabled={busy}
            title="安装到桌面"
            aria-label="安装到桌面"
            onClick={() => void install()}
        >
            <InstallGlyph size={15}/>
        </button>
    );
}

export function PwaInstallPrompt({dark = false}: {dark?: boolean}) {
    const {canInstall, installed} = usePwaInstall();
    const [dismissed, setDismissed] = useState(() => typeof window !== 'undefined' && isPwaInstallDismissed());
    const [note, setNote] = useState('');

    if (!canInstall || installed || dismissed) return null;

    const close = () => {
        dismissPwaInstall();
        setDismissed(true);
    };

    const install = async () => {
        const outcome = await promptPwaInstall();
        if (outcome === 'insecure') {
            setNote('当前是 HTTP 访问，浏览器不允许安装，请改用 HTTPS 地址');
            return;
        }
        if (outcome === 'unavailable') {
            setNote('浏览器暂未允许安装，可稍后再试或从地址栏的安装图标安装');
            return;
        }
        close();
    };

    return (
        <div className={dark ? 'pwa-install-prompt is-dark' : 'pwa-install-prompt'} role="dialog" aria-label="安装协议网关到桌面">
            <span className="pwa-install-icon"><PwaGlyph/></span>
            <div className="pwa-install-content">
                <div className="pwa-install-copy">
                    <strong>把协议网关装到桌面</strong>
                    <span>一键安装为独立窗口，不占浏览器标签页，启动更迅速。</span>
                </div>
                {note ? <div className="pwa-install-note" role="status">{note}</div> : null}
                <div className="pwa-install-actions">
                    <button type="button" className="btn primary btn-sm" onClick={() => void install()}>
                        <InstallGlyph size={14}/>
                        立即安装
                    </button>
                    <button type="button" className="btn text btn-sm" onClick={close}>
                        以后再说
                    </button>
                </div>
            </div>
            <button
                type="button"
                className="pwa-install-close"
                aria-label="关闭安装提示"
                title="关闭安装提示"
                onClick={close}
            >
                {"\u00d7"}
            </button>
        </div>
    );
}
