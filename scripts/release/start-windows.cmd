@echo off
rem 包内启动器：把管理页指到同级的 web-dist，再运行网关。
rem 数据默认在 %AppData%\llm-protocol-gateway，可用 GATEWAY_DB / GATEWAY_ADDR 覆盖。
setlocal
set "GATEWAY_WEB_DIST=%~dp0web-dist"
"%~dp0gateway.exe" %*
