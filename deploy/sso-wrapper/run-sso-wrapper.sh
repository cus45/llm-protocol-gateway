#!/bin/bash
# launchd 用启动脚本：启动协议网关的 SSO 认证包装器（18094，对外端口）。
set -euo pipefail
cd /Users/thomas990p/openai2claude/llm-protocol-gateway/deploy/sso-wrapper

set -a
source .env
set +a

exec /opt/homebrew/bin/node sso_gateway_wrapper.js
