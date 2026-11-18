# 全量质量门禁（严格串行，任一失败即阻断）
#
# 依据：开发方案修订 r2（.trae/documents/dev/plan-revision-2026-10-20-r2.md）P1-7
# 设计约束：
#   1) vitest 与 playwright 严禁并发（同机资源竞争会产生超时假失败）；
#   2) 真实联调用例依赖 docker compose 栈（lingdong-backend:8080）；
#   3) 产物一致性校验比对「本地 dist」与「容器 assets」的 SHA256；
#   4) 页面层 UI 契约门禁（S8）为纯静态断言，插在 vitest 之后、playwright 之前，失败即阻断。
#
# 用法：
#   powershell -File scripts/verify.ps1                # 全量
#   powershell -File scripts/verify.ps1 -SkipPlaywright # 仅本地调试（报告中标记为 SKIPPED）
#   powershell -File scripts/verify.ps1 -SkipPlaywright -SkipArtifacts
#                                                       # 本机无 docker 时（playwright 与产物比对都跳过）
#
# 退出码：0 = 全部门禁通过；1 = 存在失败步骤（报告已归档）

[CmdletBinding()]
param(
  [switch]$SkipPlaywright,
  # 本机无 docker 时跳过「本地 dist vs 容器 assets」比对；正式门禁不得跳过。
  [switch]$SkipArtifacts
)

$ErrorActionPreference = 'Continue'
$Root = Split-Path -Parent $PSScriptRoot
$BackendDir = Join-Path $Root 'backend'
$WebDir = Join-Path $Root 'web'
$ReportDir = Join-Path $Root '.trae/documents/dev/test-reports'

if (-not (Test-Path $ReportDir)) { New-Item -ItemType Directory -Path $ReportDir -Force | Out-Null }

$Stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$ReportPath = Join-Path $ReportDir "verify-$Stamp.md"

$Commit = (& git -C $Root rev-parse --short HEAD 2>$null)
if (-not $Commit) { $Commit = 'unknown' }

# 测试库 DSN（集成用例）。**绝不指向业务库 lingdong**。
#
# 这里曾默认指向 ldtest，而 CI 与实际使用的是 ldtest_ci —— 两者不是同一个库时，
# "门禁全绿"跑的是另一份数据。
if (-not $env:LD_TEST_DSN) {
  $env:LD_TEST_DSN = 'postgres://postgres:postgres@127.0.0.1:25432/ldtest_ci?sslmode=disable'
}
# 关键：没有 DSN 时 testutil 会 t.Skip，于是集成用例全被跳过而报告仍然"通过"。
# 打开这个开关，缺配置就变红 —— 绿必须是"真跑过"。
$env:LD_REQUIRE_TEST_DSN = '1'

$Results = New-Object System.Collections.ArrayList

function Invoke-Gate {
  param(
    [string]$Name,
    [string]$WorkDir,
    [string[]]$Command
  )
  Write-Host ""
  Write-Host "==> $Name" -ForegroundColor Cyan
  $sw = [System.Diagnostics.Stopwatch]::StartNew()
  Push-Location $WorkDir
  try {
    # 注意：pnpm 在本机解析为 pnpm.ps1（PowerShell 脚本）。对 PowerShell 脚本而言，
    # `& $exe $arr` 会把整个数组当作**单个参数**传进去（实测 pnpm 报 `Command "exec playwright test" not found`），
    # 必须用 @rest 显式 splat 才能逐个参数传入。
    $rest = @()
    if ($Command.Count -gt 1) { $rest = @($Command[1..($Command.Count - 1)]) }
    $output = & $Command[0] @rest 2>&1 | Out-String
    $code = $LASTEXITCODE
    if ($null -eq $code) { $code = 0 }
  } catch {
    $output = ($_ | Out-String)
    $code = 1
  } finally {
    Pop-Location
  }
  $sw.Stop()
  $status = if ($code -eq 0) { 'PASS' } else { 'FAIL' }
  if ($code -eq 0) { Write-Host "    PASS ($([int]$sw.Elapsed.TotalSeconds)s)" -ForegroundColor Green }
  else { Write-Host "    FAIL ($([int]$sw.Elapsed.TotalSeconds)s)" -ForegroundColor Red }
  [void]$Results.Add([pscustomobject]@{
    Name    = $Name
    Status  = $status
    Seconds = [int]$sw.Elapsed.TotalSeconds
    Output  = $output
  })
  return ($code -eq 0)
}

$allPass = $true

# 0) 部署面静态门禁（无网络、无数据库、<10s）。
#
# 为什么放在最前面：这四条查的是"仓库是否只含系统代码 / 文档与配置是否自洽"，
# 失败时给出的修复建议最直接；放在后面会让一次 10 分钟的测试跑完才被告知
# "仓库里混进了 86 个文档"。仓库卫生排第一，因为它是不可撤销的公开风险。
#
# 注意：原先这里还有一条 `deploy/scripts/check-docs.sh`（文档断链校验）。
# 文档已按「公开仓只放系统代码」的铁律移出仓库，该脚本随之删除，改由
# `scripts/check-repo-hygiene.sh` 守住「哪些文件能进仓库」。
#
# 依赖 bash（Git Bash 即可）；本机没有 bash 时整体跳过并记录 SKIPPED，不计为通过。
if (Get-Command bash -ErrorAction SilentlyContinue) {
  $allPass = (Invoke-Gate -Name 'repo: hygiene'       -WorkDir $Root -Command @('bash', 'scripts/check-repo-hygiene.sh'))      -and $allPass
  $allPass = (Invoke-Gate -Name 'deploy: env coverage' -WorkDir $Root -Command @('bash', 'deploy/scripts/check-env-coverage.sh')) -and $allPass
  $allPass = (Invoke-Gate -Name 'deploy: version'      -WorkDir $Root -Command @('bash', 'deploy/scripts/check-version.sh'))      -and $allPass
  $allPass = (Invoke-Gate -Name 'deploy: compose'      -WorkDir $Root -Command @('bash', 'deploy/scripts/check-compose.sh'))      -and $allPass
} else {
  Write-Host ""
  Write-Host '==> deploy 静态门禁（未找到 bash，跳过）' -ForegroundColor Yellow
  [void]$Results.Add([pscustomobject]@{ Name = 'deploy static gates'; Status = 'SKIPPED'; Seconds = 0; Output = '本机没有 bash；正式门禁不得跳过' })
}

# 1) 后端编译 / 静态检查 / 单元与集成测试（串行）
$allPass = (Invoke-Gate -Name 'go build ./...'   -WorkDir $BackendDir -Command @('go', 'build', './...'))      -and $allPass
$allPass = (Invoke-Gate -Name 'go vet ./...'     -WorkDir $BackendDir -Command @('go', 'vet', './...'))        -and $allPass
# 跨端契约：前端 adapter_options 常量必须与后端结构逐字一致。
# 这条比「go test」更早失败、文案也更直接（2026-11-04 的 fixed_tier 漂移就是漏了这道）。
$allPass = (Invoke-Gate -Name 'adapter options schema' -WorkDir $BackendDir -Command @('go', 'run', './cmd/dump-adapter-options', '-check')) -and $allPass
# -p 1 必须保留：多包并发会争用同一个测试库（LD_TEST_DSN），导致 auth / affiliate 偶发失败。
$allPass = (Invoke-Gate -Name 'go test -p 1 ./...' -WorkDir $BackendDir -Command @('go', 'test', '-p', '1', './...')) -and $allPass
# 1.5) 出站安全静态断言：支付沙箱禁止引入会「自己出网」的证书下载器。
#
# 逻辑放在独立脚本里（不用内联 -Command）：多行脚本作为 -Command 参数会被 PowerShell
# 二次分词，引号会被剥掉（实测 `-match "x"` 变成 `-match x` 直接语法错）。
# 断言口径与理由见 scripts/check-payment-downloader.ps1 的文件头注释。
$allPass = (Invoke-Gate -Name 'payment 包禁止 core/downloader' -WorkDir $Root -Command @('powershell', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', (Join-Path $Root 'scripts/check-payment-downloader.ps1'))) -and $allPass

# 2) 前端类型检查 + 组件测试（vitest）
$allPass = (Invoke-Gate -Name 'pnpm typecheck'   -WorkDir $WebDir -Command @('pnpm', 'typecheck'))            -and $allPass
$allPass = (Invoke-Gate -Name 'pnpm test'        -WorkDir $WebDir -Command @('pnpm', 'test'))                 -and $allPass

# 2.5) 页面层 UI 契约（S8）：静态断言 §5.3 六条禁止项 + §7 度量，失败即阻断（不依赖 dev server）
$allPass = (Invoke-Gate -Name 'scripts/check-ui-contracts.ps1' -WorkDir $Root -Command @('powershell', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', (Join-Path $Root 'scripts/check-ui-contracts.ps1'))) -and $allPass

# 3) 端到端（playwright）——必须在 vitest 结束后单独串行执行
if ($SkipPlaywright) {
  Write-Host ""
  Write-Host '==> playwright（已通过 -SkipPlaywright 跳过）' -ForegroundColor Yellow
  [void]$Results.Add([pscustomobject]@{ Name = 'pnpm exec playwright test'; Status = 'SKIPPED'; Seconds = 0; Output = '本机调试跳过；正式门禁不得跳过' })
} else {
  $allPass = (Invoke-Gate -Name 'pnpm exec playwright test' -WorkDir $WebDir -Command @('pnpm', 'exec', 'playwright', 'test')) -and $allPass
}

# 4) 产物一致性：本地 dist 与容器 assets 的 SHA256 比对
#
# 这步必须 docker（要起 web 容器取容器内 assets）。本机未装 docker 时它只能恒红，
# 与 -SkipPlaywright 同性质：本地调试可显式跳过，正式门禁（有 compose 栈的机器）不得跳过。
if ($SkipArtifacts) {
  Write-Host ""
  Write-Host '==> deploy/check-artifacts.ps1（已通过 -SkipArtifacts 跳过）' -ForegroundColor Yellow
  [void]$Results.Add([pscustomobject]@{ Name = 'deploy/check-artifacts.ps1'; Status = 'SKIPPED'; Seconds = 0; Output = '本机无 docker，跳过；正式门禁不得跳过' })
} else {
  $allPass = (Invoke-Gate -Name 'deploy/check-artifacts.ps1' -WorkDir $Root -Command @('powershell', '-NoProfile', '-File', (Join-Path $Root 'deploy/check-artifacts.ps1'))) -and $allPass
}

# ---- 归档报告 ----
$lines = New-Object System.Collections.ArrayList
[void]$lines.Add("# 质量门禁报告 $Stamp")
[void]$lines.Add("")
[void]$lines.Add("- 提交：$Commit")
[void]$lines.Add("- 总结果：$(if ($allPass) { 'PASS' } else { 'FAIL' })")
[void]$lines.Add("- 执行方式：串行（vitest 与 playwright 不并发）")
[void]$lines.Add("")
[void]$lines.Add('| 步骤 | 结果 | 耗时(s) |')
[void]$lines.Add('|---|---|---|')
foreach ($r in $Results) { [void]$lines.Add("| $($r.Name) | $($r.Status) | $($r.Seconds) |") }
[void]$lines.Add("")
[void]$lines.Add('## 各步骤输出（末尾 40 行）')
foreach ($r in $Results) {
  [void]$lines.Add("")
  [void]$lines.Add("### $($r.Name) — $($r.Status)")
  [void]$lines.Add('```')
  $tail = ($r.Output -split "`r?`n")
  if ($tail.Count -gt 40) { $tail = $tail[($tail.Count - 40)..($tail.Count - 1)] }
  [void]$lines.Add(($tail -join "`n"))
  [void]$lines.Add('```')
}
[System.IO.File]::WriteAllText($ReportPath, ($lines -join "`n"), (New-Object System.Text.UTF8Encoding($true)))

Write-Host ""
Write-Host "报告已归档：$ReportPath" -ForegroundColor Cyan
if ($allPass) {
  Write-Host '门禁通过。' -ForegroundColor Green
  exit 0
}
Write-Host '门禁失败（已阻断）。' -ForegroundColor Red
exit 1
