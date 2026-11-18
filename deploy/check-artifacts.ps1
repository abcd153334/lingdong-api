# 硬性关卡：前端产物一致性校验（S20）
# 判定：容器 /usr/share/nginx/html/assets 与本地 web/dist/assets 的
#       文件名集合与逐文件 SHA256 必须完全一致，否则退出码 1（本次交付判定未完成）。
# 用法（仓库根执行）：
#   pwsh deploy/check-artifacts.ps1              # 全流程：本地构建 → 重建 web 容器 → 比对
#   pwsh deploy/check-artifacts.ps1 -SkipRebuild # 只比对（容器已含最新产物时）
param(
  [switch]$SkipRebuild
)
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$compose = Join-Path $root "deploy\docker-compose.yml"
$dist = Join-Path $root "web\dist\assets"

if (-not $SkipRebuild) {
  Write-Host "== 1/3 本地构建 web/dist =="
  Push-Location (Join-Path $root "web")
  pnpm build
  $code = $LASTEXITCODE
  Pop-Location
  if ($code -ne 0) { throw "pnpm build 失败" }

  Write-Host "== 2/3 重建 web 镜像与容器（部署最新产物）=="
  docker compose -f $compose build web
  if ($LASTEXITCODE -ne 0) { throw "docker compose build web 失败" }
  docker compose -f $compose up -d web
  if ($LASTEXITCODE -ne 0) { throw "docker compose up -d web 失败" }
}

if (-not (Test-Path $dist)) { throw "本地 $dist 不存在（先构建或去掉 -SkipRebuild）" }

Write-Host "== 2.5 产物不含已下线实现 =="
# 人机验证换过一次实现（图形码 -> PoW -> 图形码）。这里只钉两个"旧包才有"的标志：
#   turnstile —— 已完全移除；
#   captcha_payload —— PoW 的字段名，现在是 captcha_id + captcha_answer。
# 注意不要禁 "auth/captcha"：那是当前实现自己的路径。
$legacy = Get-ChildItem (Join-Path $dist "assets") -Filter *.js -File |
  Select-String -Pattern 'turnstile', 'captcha_payload' -SimpleMatch -List
if ($legacy) {
  throw "dist 里仍有已下线实现（turnstile / captcha_payload）：$($legacy.Path -join ', ')"
}
Write-Host "  PASS" -ForegroundColor Green

Write-Host "== 3/3 比对 assets 文件名与 SHA256 =="
# 本地：文件名 → 哈希（哈希转小写，对齐 busybox sha256sum 输出）
$localMap = @{}
Get-ChildItem $dist -File | ForEach-Object {
  $localMap[$_.Name] = (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
}
# 容器：busybox sha256sum 逐行 "hash  name"
$containerMap = @{}
$lines = docker exec lingdong-web sh -c "cd /usr/share/nginx/html/assets && sha256sum *"
foreach ($line in $lines) {
  if ($line -match "^([0-9a-f]{64})\s+(\S+)$") {
    $containerMap[$Matches[2]] = $Matches[1]
  } else { throw "无法解析容器哈希行: $line" }
}

$fail = $false
$missingInContainer = $localMap.Keys | Where-Object { -not $containerMap.ContainsKey($_) }
$extraInContainer = $containerMap.Keys | Where-Object { -not $localMap.ContainsKey($_) }
$mismatch = $localMap.Keys | Where-Object { $containerMap.ContainsKey($_) -and $localMap[$_] -ne $containerMap[$_] }

if ($missingInContainer) { $fail = $true; Write-Host "缺失于容器: $($missingInContainer -join ', ')" -ForegroundColor Red }
if ($extraInContainer) { $fail = $true; Write-Host "容器多出: $($extraInContainer -join ', ')" -ForegroundColor Red }
if ($mismatch) { $fail = $true; Write-Host "哈希不一致: $($mismatch -join ', ')" -ForegroundColor Red }

if ($fail) { throw "产物一致性校验失败（容器 assets != 本地 web/dist/assets）" }
Write-Host "产物一致性校验通过：$($localMap.Count) 个 assets 文件名与 SHA256 完全一致" -ForegroundColor Green
