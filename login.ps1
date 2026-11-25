# login.ps1 — Windows 登录入口（在装了 TRAE 客户端的机器上运行）
#
# 用法：
#   .\login.ps1
#   .\login.ps1 -client-id en1oxy7wnw8j9n
#   $env:DEVICE_ID="1234567890123"; .\login.ps1
#
# 设备号默认自动从 %APPDATA%\Trae CN\User\globalStorage\storage.json 读取。
$ErrorActionPreference = "Stop"
Set-Location -LiteralPath $PSScriptRoot
go run ./cmd/login @args
exit $LASTEXITCODE
