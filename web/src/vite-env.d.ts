/// <reference types="vite/client" />
/// <reference types="vite-plugin-pwa/client" />

/** 构建时注入的应用版本（scripts/build-version.sh）；开发构建为空串。 */
declare const __APP_VERSION__: string | undefined;
