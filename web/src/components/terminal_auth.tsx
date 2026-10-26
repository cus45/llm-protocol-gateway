import React, { useState, useEffect, useRef } from 'react';

export interface TerminalAuthGateProps {
  authSetupMode: boolean;
  authUsername: string;
  setAuthUsername: (val: string) => void;
  authPassword: string;
  setAuthPassword: (val: string) => void;
  authPasswordConfirm: string;
  setAuthPasswordConfirm: (val: string) => void;
  authBusy: boolean;
  authError: string;
  setAuthError: (val: string) => void;
  // 「信任此安全设备」由 App 持有：登录请求要把它带给服务端决定会话 cookie 是否持久。
  trustDevice: boolean;
  setTrustDevice: (val: boolean) => void;
  onSubmit: (mode: 'setup' | 'login') => Promise<void> | void;
}

export function TerminalAuthGate({
  authSetupMode,
  authUsername,
  setAuthUsername,
  authPassword,
  setAuthPassword,
  authPasswordConfirm,
  setAuthPasswordConfirm,
  authBusy,
  authError,
  setAuthError,
  trustDevice,
  setTrustDevice,
  onSubmit,
}: TerminalAuthGateProps) {

  const usernameRef = useRef<HTMLInputElement>(null);
  const passwordRef = useRef<HTMLInputElement>(null);
  const confirmRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (authSetupMode) {
      passwordRef.current?.focus();
    } else if (authUsername && authUsername.trim() !== '') {
      passwordRef.current?.focus();
    } else {
      usernameRef.current?.focus();
    }
  }, [authSetupMode]);

  // 「30 天内免登录」是服务端决定的（会话 cookie 是否持久），所以勾选状态交给 App
  // 随登录请求一起提交；localStorage 只记住这台设备上的偏好。
  const handleTrustDeviceChange = (checked: boolean) => {
    setTrustDevice(checked);
  };

  const handleUsernameKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      passwordRef.current?.focus();
    }
  };

  const handlePasswordKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      if (authSetupMode) {
        confirmRef.current?.focus();
      } else {
        setAuthError('');
        void onSubmit('login');
      }
    }
  };

  const handleConfirmKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      setAuthError('');
      void onSubmit('setup');
    }
  };

  return (
    <div className="auth-terminal-shell">
      <div className="auth-terminal-window">
        {/* macOS window title bar */}
        <div className="auth-terminal-titlebar">
          <div className="auth-terminal-dots">
            <span className="auth-terminal-dot close" />
            <span className="auth-terminal-dot minimize" />
            <span className="auth-terminal-dot maximize" />
          </div>
          <div className="auth-terminal-title">gateway-login — zsh — 80x24</div>
          <div className="auth-terminal-titlebar-spacer" />
        </div>

        {/* Terminal body */}
        <div className="auth-terminal-body">
          {/* Header with Protocol Gateway Logo + Title */}
          <div className="auth-terminal-header">
            <div className="auth-terminal-logo-wrap">
              <svg className="auth-terminal-logo-icon" viewBox="0 0 40 40" width="38" height="38" fill="none">
                <rect width="40" height="40" rx="10" fill="url(#pg-logo-bg)" />
                <circle cx="20" cy="20" r="9.5" stroke="url(#pg-logo-ring)" strokeWidth="2.8" strokeLinecap="round" />
                <path d="M12 20H15M25 20H28M20 12V15M20 25V28" stroke="#38bdf8" strokeWidth="2" strokeLinecap="round" />
                <circle cx="20" cy="20" r="4" fill="url(#pg-logo-core)" />
                <defs>
                  <linearGradient id="pg-logo-bg" x1="0" y1="0" x2="40" y2="40" gradientUnits="userSpaceOnUse">
                    <stop stopColor="#1e1b4b" />
                    <stop offset="1" stopColor="#0f172a" />
                  </linearGradient>
                  <linearGradient id="pg-logo-ring" x1="10" y1="10" x2="30" y2="30" gradientUnits="userSpaceOnUse">
                    <stop stopColor="#38bdf8" />
                    <stop offset="0.5" stopColor="#6366f1" />
                    <stop offset="1" stopColor="#a855f7" />
                  </linearGradient>
                  <linearGradient id="pg-logo-core" x1="16" y1="16" x2="24" y2="24" gradientUnits="userSpaceOnUse">
                    <stop stopColor="#38bdf8" />
                    <stop offset="1" stopColor="#818cf8" />
                  </linearGradient>
                </defs>
              </svg>
            </div>
            <div className="auth-terminal-brand">
              <div className="auth-terminal-brand-title">协议网关</div>
              <div className="auth-terminal-brand-sub">LLM Protocol Gateway 工作台</div>
            </div>
          </div>

          {/* Terminal Session Logs */}
          <div className="auth-terminal-logs">
            <div className="auth-terminal-line muted">Last login: today on ttys001</div>
            <div className="auth-terminal-line muted">Welcome to LLM Protocol Gateway secure shell.</div>

            {/* Prompt Line 1: Username */}
            <div className="auth-terminal-prompt-line" onClick={() => usernameRef.current?.focus()}>
              <span className="auth-terminal-prompt">gateway %</span>
              <input
                ref={usernameRef}
                type="text"
                autoComplete="username"
                className="auth-terminal-cmd-input"
                value={authUsername}
                onChange={(e) => setAuthUsername(e.target.value)}
                onKeyDown={handleUsernameKeyDown}
                placeholder="admin"
                disabled={authBusy}
                spellCheck={false}
              />
            </div>

            {/* Prompt Line 2: Password */}
            <div className="auth-terminal-prompt-line" onClick={() => passwordRef.current?.focus()}>
              <span className="auth-terminal-prompt">gateway %</span>
              <input
                ref={passwordRef}
                type="password"
                autoComplete={authSetupMode ? 'new-password' : 'current-password'}
                className="auth-terminal-cmd-input password-input"
                value={authPassword}
                onChange={(e) => setAuthPassword(e.target.value)}
                onKeyDown={handlePasswordKeyDown}
                disabled={authBusy}
                spellCheck={false}
              />
            </div>

            {/* Setup Mode: Confirm Password */}
            {authSetupMode && (
              <div className="auth-terminal-prompt-line" onClick={() => confirmRef.current?.focus()}>
                <span className="auth-terminal-prompt">gateway %</span>
                <input
                  ref={confirmRef}
                  type="password"
                  autoComplete="new-password"
                  className="auth-terminal-cmd-input password-input"
                  value={authPasswordConfirm}
                  onChange={(e) => setAuthPasswordConfirm(e.target.value)}
                  onKeyDown={handleConfirmKeyDown}
                  placeholder="再输一次以确认"
                  disabled={authBusy}
                  spellCheck={false}
                />
              </div>
            )}
          </div>

          {/* Hint Line */}
          <div className="auth-terminal-hint-line">
            {authSetupMode
              ? '首次远程访问，请在上方两行设置并确认管理员密码（至少 8 位），回车进入'
              : '按 Enter 提交登录（输入框内回车提交密码登录）'}
          </div>

          {/* Error Message */}
          {authError ? (
            <div className="auth-terminal-line error-line">
              [error] {authError}
            </div>
          ) : null}

          {/* Busy State */}
          {authBusy ? (
            <div className="auth-terminal-line busy-line">
              [authenticating...] 正在验证身份凭据，请稍候
            </div>
          ) : null}

          {/* Checkbox Section */}
          <div className="auth-terminal-trust-wrapper">
            <label className="auth-terminal-checkbox-row">
              <input
                type="checkbox"
                className="auth-terminal-checkbox-input"
                checked={trustDevice}
                onChange={(e) => handleTrustDeviceChange(e.target.checked)}
              />
              <span className={`auth-terminal-checkbox-box ${trustDevice ? 'checked' : ''}`}>
                {trustDevice && (
                  <svg viewBox="0 0 12 12" width="10" height="10" fill="none">
                    <path
                      d="M2.5 6L4.8 8.5L9.5 3.5"
                      stroke="#ffffff"
                      strokeWidth="1.8"
                      strokeLinecap="round"
                      strokeLinejoin="round"
                    />
                  </svg>
                )}
              </span>
              <span className="auth-terminal-checkbox-label">
                信任此安全设备（30 天内免登录）
              </span>
            </label>
            <div className="auth-terminal-checkbox-sub">
              仅在个人电脑上勾选；主动退出后将取消本设备信任。
            </div>
          </div>

          {/* Action Buttons Section */}
          <div className="auth-terminal-actions">
            <button
              type="button"
              className="auth-terminal-btn"
              disabled={authBusy}
              onClick={() => void onSubmit(authSetupMode ? 'setup' : 'login')}
            >
              <span>{authBusy ? '登录中…' : authSetupMode ? '设置密码并进入' : '密码登录'}</span>
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
