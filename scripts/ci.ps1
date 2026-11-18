# 灵动 API · 本地/CI 一键门禁
#
# 用途：把「提交前该跑的检查」固化成一条命令，避免"我本地跑过了"变成口头承诺。
# 任一阶段失败立即退出（exit 1），日志统一落 outputs/ci-<时间戳>.log。
#
# 用法：
#   powershell -File scripts/ci.ps1              # 全量
#   powershell -File scripts/ci.ps1 -SkipE2E     # 跳过 playwright
#   powershell -File scripts/ci.ps1 -BackendOnly  # 只跑后端
#
# 前置：后端集成用例需要 LD_TEST_DSN（默认指向本机 25432 的 ldtest_ci）。
# 本脚本会导出 LD_REQUIRE_TEST_DSN=1 —— 缺 DSN 时让集成用例**失败**而不是跳过，
# 否则"全绿"可能只是"一个集成用例都没跑"。

param(
    [switch]$BackendOnly,
    [switch]$SkipE2E
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
$stamp = Get-Date -Format "yyyyMMdd-HHmmss"
$logDir = Join-Path $repo "outputs"
if (-not (Test-Path $logDir)) { New-Item -ItemType Directory -Path $logDir | Out-Null }
$log = Join-Path $logDir "ci-$stamp.log"

function Section($name) {
    $line = "===== $name ====="
    Write-Host $line -ForegroundColor Cyan
    Add-Content -Path $log -Value $line -Encoding utf8
}

# 运行一条命令并同时写日志；失败立即退出。
function Run($name, $exe, $argList, $workDir) {
    Section $name
    Push-Location $workDir
    try {
        & $exe @argList 2>&1 | Tee-Object -FilePath $log -Append
        if ($LASTEXITCODE -ne 0) {
            Write-Host "FAILED: $name (exit $LASTEXITCODE)" -ForegroundColor Red
            Write-Host "日志：$log"
            exit 1
        }
    } finally {
        Pop-Location
    }
}

$go = "go"
$env:GOFLAGS = "-mod=mod"
$env:LD_REQUIRE_TEST_DSN = "1"
if (-not $env:LD_TEST_DSN) {
    $env:LD_TEST_DSN = "postgres://postgres:postgres@127.0.0.1:25432/ldtest_ci?sslmode=disable"
}

$backend = Join-Path $repo "backend"
$web = Join-Path $repo "web"

# ---- 后端 ----------------------------------------------------------------
Run "go build" $go @("./...") $backend
Run "go vet" $go @("./...") $backend
# -p 1：多个集成包并发会互相抢测试库里的全局表，随机红。
# -vet=off：build/vet 已单独跑过，这里再跑一遍 vet 只会让耗时翻倍。
Run "go test" $go @("test", "-vet=off", "-count=1", "-p", "1", "./internal/...") $backend

if ($BackendOnly) {
    Write-Host "后端门禁通过（已按要求跳过前端）。日志：$log" -ForegroundColor Green
    exit 0
}

# ---- 前端 ----------------------------------------------------------------
$npx = "npx"
Run "tsc" $npx @("tsc", "--noEmit") $web
Run "vitest" $npx @("vitest", "run") $web
Run "vite build" $npx @("vite", "build") $web

if (-not $SkipE2E) {
    # real-backend 需要真实后端 + LD_CAPTCHA_TEST_CODE，本地默认没有，故排除。
    Run "playwright" $npx @("playwright", "test", "--grep-invert", "real-backend") $web
}

Write-Host "全部门禁通过。日志：$log" -ForegroundColor Green
