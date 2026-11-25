#!/usr/bin/env bash
# login.sh — TRAE 登录（2026-11 豆包账号体系融合后的新流程）
#
# 登录走豆包 SSO + PKCE，凭证里会自动保存设备密钥对，
# 后续刷新 token 需要用它做设备签名（v3 ExchangeToken）。
#
# 设备号（必须真实，否则签到返回 code=9074）：
#   - 默认自动从客户端 storage.json 的 iCubeAuthInfo://icube-dc:<数字> 读取
#   - 也可以手动指定：DEVICE_ID=1234567890123 ./login.sh
#
# 其他参数：./login.sh -h
set -euo pipefail

cd "$(dirname "$0")"

# DEVICE_ID 环境变量会被 cmd/login 自动读取
exec go run ./cmd/login "$@"
