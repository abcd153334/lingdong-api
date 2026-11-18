# 灵动 API · 数据库备份与校验
#
# 为什么必须有：全站账务（余额/冻结/流水/用量）只有一份，落在 PostgreSQL 数据卷里。
# 卷丢了 = 用户余额与营收历史一起消失，且无法从任何上游重建。而"有备份"和
# "备份能恢复"是两件事 —— 本脚本每次备份后都会算一遍账务指纹并写进校验文件，
# 恢复演练时用它比对（恢复演练口径）。
#
# 用法：
#   powershell -File deploy/backup.ps1
#   powershell -File deploy/backup.ps1 -OutDir D:\backups -KeepDays 30
#
# 依赖：pg_dump / psql 在 PATH 中（PostgreSQL 客户端）。连接信息取 DATABASE_URL，
# 未设置时用本机开发库（127.0.0.1:25432/lingdong）——**生产必须显式传**，
# 脚本不会去猜生产地址。

param(
    [string]$OutDir = "",
    [int]$KeepDays = 14,
    [string]$DatabaseUrl = ""
)

$ErrorActionPreference = "Stop"

if (-not $OutDir) {
    $OutDir = Join-Path (Split-Path -Parent $PSScriptRoot) "backups"
}
if (-not (Test-Path $OutDir)) { New-Item -ItemType Directory -Path $OutDir -Force | Out-Null }

if (-not $DatabaseUrl) {
    $DatabaseUrl = $env:DATABASE_URL
}
if (-not $DatabaseUrl) {
    $DatabaseUrl = "postgres://lingdong:lingdong@127.0.0.1:25432/lingdong?sslmode=disable"
    Write-Host "未提供 DATABASE_URL，按本机开发库处理：$DatabaseUrl" -ForegroundColor Yellow
}

$stamp = Get-Date -Format "yyyyMMdd-HHmmss"
$dump = Join-Path $OutDir "lingdong-$stamp.dump"
$sumFile = Join-Path $OutDir "lingdong-$stamp.verify.txt"

Write-Host "开始备份 → $dump" -ForegroundColor Cyan
& pg_dump --format=custom --no-owner --no-acl --file="$dump" --dbname="$DatabaseUrl"
if ($LASTEXITCODE -ne 0) {
    Write-Host "备份失败（pg_dump exit $LASTEXITCODE）" -ForegroundColor Red
    exit 1
}

$sizeMb = [math]::Round((Get-Item $dump).Length / 1MB, 2)
Write-Host "备份完成：$sizeMb MiB"

# ---- 账务指纹 ------------------------------------------------------------
# 只查几个"恢复后必须一模一样"的数：任何一项对不上，说明这份备份不能用。
# COALESCE 是必须的：空库会返回 NULL，而 NULL 与 0 在字符串比较里不相等。
$sql = @'
SELECT
  (SELECT count(*) FROM users WHERE deleted_at IS NULL)                    AS users,
  (SELECT COALESCE(sum(balance),0) FROM users)                             AS balance_micro,
  (SELECT COALESCE(sum(frozen),0) FROM users)                              AS frozen_micro,
  (SELECT COALESCE(sum(used),0) FROM users)                                AS used_micro,
  (SELECT count(*) FROM transactions)                                      AS txns,
  (SELECT COALESCE(sum(amount),0) FROM transactions)                       AS txn_amount_micro,
  (SELECT count(*) FROM usages)                                            AS usages,
  (SELECT COALESCE(sum(charge),0) FROM usages)                             AS charge_micro,
  (SELECT count(*) FROM orders WHERE status='paid')                        AS paid_orders;
'@

Write-Host "采集账务指纹…"
$fingerprint = & psql --dbname="$DatabaseUrl" --no-psqlrc --tuples-only --no-align --command="$sql"
if ($LASTEXITCODE -ne 0) {
    Write-Host "采集指纹失败：备份文件已生成，但**未校验**，请人工确认" -ForegroundColor Red
    exit 1
}

$meta = @(
    "backup_file = $(Split-Path -Leaf $dump)",
    "created_at  = $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')",
    "size_bytes  = $((Get-Item $dump).Length)",
    "keep_days   = $KeepDays",
    "fingerprint = $fingerprint"
)
$meta | Set-Content -Path $sumFile -Encoding utf8

# 备份自身的 sha256：拷贝到异地后用它确认文件没坏（比大小可靠）。
$hash = (Get-FileHash -Algorithm SHA256 -Path $dump).Hash
Add-Content -Path $sumFile -Value "sha256     = $hash" -Encoding utf8

Write-Host "指纹已写入 $sumFile"
Write-Host $fingerprint

# ---- 清理过期备份 --------------------------------------------------------
$cutoff = (Get-Date).AddDays(-$KeepDays)
$old = Get-ChildItem -Path $OutDir -Filter "lingdong-*.dump" | Where-Object { $_.LastWriteTime -lt $cutoff }
foreach ($f in $old) {
    Write-Host "删除过期备份：$($f.Name)" -ForegroundColor DarkGray
    Remove-Item -Path $f.FullName -Force
    $v = Join-Path $OutDir ($f.BaseName + ".verify.txt")
    if (Test-Path $v) { Remove-Item -Path $v -Force }
}

Write-Host "完成（保留 $KeepDays 天）。请在异地再存一份：备份与数据库同盘时，盘坏 = 两份一起没。" -ForegroundColor Green
