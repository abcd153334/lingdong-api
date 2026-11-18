# 出站安全静态断言：支付沙箱禁止引入会「自己出网」的证书下载器。
#
# 依据：微信官方 SDK 的 core/downloader 会绕过 netguard 的 IP 校验直连上游下载平台证书，
# 因此 payment 包内必须零引用（只能用 WithWechatPayPublicKeyAuthCipher / WithWechatPayCertificate）。
#
# **断言口径必须落在本仓库源码上，不能用 `go list -deps`（传递依赖）**：
# wechatpay-go 的 core 包自己就 import 了 core/downloader（client.go / settings.go 等 6 个文件），
# 只要用官方 SDK 的 core.NewClient，downloader 必然出现在传递依赖里 —— 此时 -deps 断言恒假，
# 与「本仓库会不会去下载证书」毫无关系，门禁变成永久红灯（任何正当实现都过不了）。
#
# 真正要守的两条：
#   1) payment 不直接 import .../core/downloader；
#   2) 不调用会自动下载证书的 WithWechatPayAutoAuthCipher（注释里提到它不算违规）。
#
# 用法：powershell -NoProfile -ExecutionPolicy Bypass -File scripts/check-payment-downloader.ps1
# 退出码：0 = 达标；1 = 存在违规

$ErrorActionPreference = 'Stop'

$Root = Split-Path -Parent $PSScriptRoot
$BackendDir = Join-Path $Root 'backend'

Push-Location $BackendDir
try {
  $direct = go list -f '{{.Imports}}' ./internal/service/payment
  if ($direct -match 'wechatpay-go/core/downloader') {
    Write-Output 'payment 直接 import 了 core/downloader（会绕过 netguard 直连下载平台证书）'
    exit 1
  }

  $hit = Get-ChildItem ./internal/service/payment -Filter *.go |
    Where-Object { $_.Name -notlike '*_test.go' } |
    Select-String -Pattern 'WithWechatPayAutoAuthCipher' |
    Where-Object { $_.Line -notmatch '^\s*//' }
  if ($hit) {
    Write-Output 'payment 调用了自动下载器 WithWechatPayAutoAuthCipher（应改用手动证书/公钥模式）'
    $hit | ForEach-Object { Write-Output ('  ' + $_.Path + ':' + $_.LineNumber) }
    exit 1
  }

  Write-Output 'payment downloader guard PASS'
  exit 0
} finally {
  Pop-Location
}
