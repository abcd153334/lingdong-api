# 页面层 UI 契约门禁（S8）
#
# 依据：.trae/documents/ui-consistency-refactor-plan-v3.md
#   §5.3 页面层禁止项（6 条，可自动化断言）
#   §7   验收度量 M1-M5 / M7 / M10 / M15（组件契约与原型类移植）
#
# 设计约束：
#   1) 只做静态断言（grep 级），不启动 dev server、不依赖后端；可随时单跑；
#   2) 失败即阻断（退出码 1），并由 scripts/verify.ps1 串行链调用；
#   3) 每次运行归档 .trae/documents/dev/test-reports/ui-contracts-<stamp>.md。
#
# 用法：
#   powershell -File scripts/check-ui-contracts.ps1
#
# 退出码：0 = 全部契约达标；1 = 存在违规（报告已归档）

[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

$Root = Split-Path -Parent $PSScriptRoot
$WebDir = Join-Path $Root 'web'
$SrcDir = Join-Path $WebDir 'src'
$PagesDir = Join-Path $SrcDir 'pages'
$StyleDir = Join-Path $SrcDir 'styles'
$ReportDir = Join-Path $Root '.trae/documents/dev/test-reports'

if (-not (Test-Path $PagesDir)) { Write-Host "pages dir not found: $PagesDir" -ForegroundColor Red; exit 1 }
if (-not (Test-Path $ReportDir)) { New-Item -ItemType Directory -Path $ReportDir -Force | Out-Null }

$Stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$ReportPath = Join-Path $ReportDir "ui-contracts-$Stamp.md"

$PageFiles = @(Get-ChildItem -LiteralPath $PagesDir -Recurse -File -Filter *.tsx | Sort-Object FullName)
$ComponentsCss = Get-Content -Raw -LiteralPath (Join-Path $StyleDir 'components.css') -Encoding UTF8
$TokensCss = Get-Content -Raw -LiteralPath (Join-Path $StyleDir 'tokens.css') -Encoding UTF8

$script:LineCache = @{}
function Get-SourceLines {
  param([System.IO.FileInfo]$File)
  $key = $File.FullName
  if (-not $script:LineCache.ContainsKey($key)) {
    $script:LineCache[$key] = @(Get-Content -LiteralPath $File.FullName -Encoding UTF8)
  }
  # 逗号前缀：PowerShell 的 return 会把单元素数组展开成标量，
  # 只有一行的文件会让调用方的 $lines[$i] 变成 Char（然后 .Trim() 直接抛异常）。
  return ,$script:LineCache[$key]
}

# 扫描页面层：返回命中行（file:line: 片段），最多保留 8 条样例行
# ContextMode: exclude = 排除「窗口内出现 ContextPattern」的行；require = 只保留「窗口内出现 ContextPattern」的行
function Find-Violations {
  param(
    [string]$Pattern,
    [int]$LookBack = 0,
    [int]$LookAhead = 0,
    [string]$ContextPattern = $null,
    [ValidateSet('exclude', 'require')]
    [string]$ContextMode = 'exclude',
    [string[]]$IgnorePattern = @()
  )
  $hits = New-Object System.Collections.ArrayList
  foreach ($f in $PageFiles) {
    $lines = Get-SourceLines $f
    for ($i = 0; $i -lt $lines.Count; $i++) {
      $line = $lines[$i]
      if ($line -notmatch $Pattern) { continue }
      $skip = $false
      foreach ($ig in $IgnorePattern) { if ($line -match $ig) { $skip = $true; break } }
      if ($skip) { continue }
      if ($ContextPattern) {
        $from = [Math]::Max(0, $i - $LookBack)
        $to = [Math]::Min($lines.Count - 1, $i + $LookAhead)
        $found = $false
        for ($j = $from; $j -le $to; $j++) { if ($lines[$j] -match $ContextPattern) { $found = $true; break } }
        if ($ContextMode -eq 'exclude' -and $found) { continue }
        if ($ContextMode -eq 'require' -and -not $found) { continue }
      }
      [void]$hits.Add("$($f.Name):$($i + 1): $($line.Trim())")
    }
  }
  return @($hits)
}

# 文件级扫描：整文件命中 MustPattern 且**不含** LackPattern 记为违规。
# 现有 Find-Violations 是逐行的，表达不了「同一文件里出现了 A 却没出现 B」。
function Find-FileViolations {
  param(
    [string]$MustPattern,
    [string]$LackPattern,
    [string[]]$IgnoreNames = @()
  )
  $hits = New-Object System.Collections.ArrayList
  foreach ($f in $PageFiles) {
    if ($IgnoreNames -contains $f.Name) { continue }
    $raw = Get-Content -Raw -LiteralPath $f.FullName -Encoding UTF8
    if ($raw -notmatch $MustPattern) { continue }
    if ($raw -match $LackPattern) { continue }
    [void]$hits.Add($f.Name)
  }
  return @($hits)
}

$Results = New-Object System.Collections.ArrayList
function Add-Check {
  param(
    [string]$Id,
    [string]$Name,
    [int]$Actual,
    [int]$Expect,
    [ValidateSet('atMost', 'atLeast')]
    [string]$Mode = 'atMost',
    [string[]]$Samples = @(),
    [string]$Note = ''
  )
  $ok = if ($Mode -eq 'atMost') { $Actual -le $Expect } else { $Actual -ge $Expect }
  $status = if ($ok) { 'PASS' } else { 'FAIL' }
  [void]$Results.Add([pscustomobject]@{
      Id      = $Id
      Name    = $Name
      Actual  = $Actual
      Expect  = $Expect
      Mode    = $Mode
      Status  = $status
      Samples = @($Samples | Select-Object -First 8)
      Note    = $Note
    })
}

# 通用工具类：仅匹配 Tailwind 风格 token，避免误伤语义类（flex-between / items-start / mt16）
$TW = '(grid-cols-|col-span-|row-span-|\bgap-[0-9]|\bm[btlrx]?-[0-9]|\bp[btlrx]?-[0-9]|\bw-\[|\bh-\[|inline-flex|flex-col|flex-wrap|items-center|items-end|justify-between|justify-center|justify-end|max-md:|max-lg:|max-sm:|text-\[var|bg-\[var|border-\[var|rounded-\[|\bflex(?=[\s"]))'
$LAYOUT = '(margin|marginTop|marginBottom|marginLeft|marginRight|padding|paddingTop|flex|flexGrow|display|gap|alignItems|justifyContent|textAlign|minWidth|maxWidth|height|position|float)'

# ---------- M1 表单字段统一走 Field ----------
$m1 = Find-Violations -Pattern 'className="form-item"'
Add-Check -Id 'M1' -Name '页面层 className="form-item" 命中（应为 0，统一走 Field）' -Actual $m1.Count -Expect 0 -Samples $m1

# ---------- M2 三态 / 胶囊 / 筛选条不得自造 ----------
$m2a = Find-Violations -Pattern 'className="empty"'
$m2b = Find-Violations -Pattern 'className="pbar'
# 胶囊组：.chips 内出现 kbtn（R17：.chips 仅允许作标签容器）
$m2c = Find-Violations -Pattern 'className="chips"' -LookAhead 8 -ContextPattern 'kbtn' -ContextMode require
$m2 = @($m2a) + @($m2b) + @($m2c)
Add-Check -Id 'M2' -Name '页面层 className="empty"/"pbar*"/胶囊组 .chips 命中（应为 0）' -Actual $m2.Count -Expect 0 -Samples $m2 `
  -Note "empty=$($m2a.Count) pbar=$($m2b.Count) chips+kbtn=$($m2c.Count)"

# ---------- M3 内联布局样式（白名单：色值 / 进度宽度） ----------
$m3 = Find-Violations -Pattern 'style=\{\{' -IgnorePattern @()
$m3 = @($m3 | Where-Object { ($_ -split ':', 3)[-1] -match $LAYOUT })
Add-Check -Id 'M3' -Name '页面层内联布局样式（仅允许色值/进度宽度）' -Actual $m3.Count -Expect 0 -Samples $m3

# ---------- M4 Tailwind 布局/响应式/色值工具类 ----------
$m4 = Find-Violations -Pattern ('className="[^"]*' + $TW)
Add-Check -Id 'M4' -Name '页面层 Tailwind 布局/响应式/色值工具类' -Actual $m4.Count -Expect 0 -Samples $m4

# ---------- M5 页面层裸 apiClient / 裸 hook ----------
$m5 = Find-Violations -Pattern '(apiClient\.|useQuery\(|useMutation\()'
Add-Check -Id 'M5' -Name '页面层裸 apiClient / useQuery / useMutation' -Actual $m5.Count -Expect 0 -Samples $m5

# ---------- M7 手写卡片骨架 / 裸表单控件 ----------
$m7a = Find-Violations -Pattern '<h3[ >]'
$m7b = Find-Violations -Pattern '<(input|select|textarea)([\s>/])' -LookBack 9 -ContextPattern '<Field|<TagInput|<Select'
$m7 = @($m7a) + @($m7b)
Add-Check -Id 'M7' -Name '手写 <h3> 卡片骨架 / 裸 <input|select|textarea>（应在 Field 内）' -Actual $m7.Count -Expect 0 -Samples $m7 `
  -Note "h3=$($m7a.Count) bare-input=$($m7b.Count)"

# ---------- M10 列定义零双写（统一 lib/table.ts） ----------
$m10 = Find-Violations -Pattern '(orderColumns|usageColumns)\s*[:=]'
Add-Check -Id 'M10' -Name '页面层重复定义订单/调用列（应只在 lib/table.ts）' -Actual $m10.Count -Expect 0 -Samples $m10

# ---------- M15 原型页级语义类：已移植 + 被引用 ----------
# 2026-11-11：'adapter-grid' / 'adapter-card' 随「适配器页」合并进模型服务而删除
# （左栏是级联列表，不再有卡片网格）。名单里删掉，样式表里的死规则也一并删掉 ——
# 留着"门禁要求存在但页面不用"的类，等于把门禁变成摆设。
$Transplant = @(
  'up-zone', 'qbar', 'uc-hero', 'code-row',
  'au-row', 'chan-row', 'models-grid', 'link-row', 'defaults-grid', 'doc',
  'eff-date', 'flex-between', 'wrap', 'right'
)
$missing = New-Object System.Collections.ArrayList
foreach ($cls in $Transplant) {
  $cssRx = '\.' + [regex]::Escape($cls) + '(?![-\w])'
  $tsxRx = '\b' + [regex]::Escape($cls) + '(?![-\w])'
  if ($ComponentsCss -notmatch $cssRx) { [void]$missing.Add("components.css 缺 .$cls") ; continue }
  $ref = Get-ChildItem -LiteralPath $SrcDir -Recurse -File -Include *.tsx |
    Where-Object { (Get-Content -Raw -LiteralPath $_.FullName -Encoding UTF8) -match $tsxRx } |
    Select-Object -First 1
  if (-not $ref) { [void]$missing.Add("无 tsx 引用 .$cls") }
}
$m15 = @($missing)
Add-Check -Id 'M15' -Name "原型页级类移植+引用（$($Transplant.Count) 项应全通过）" `
  -Actual ($Transplant.Count - $m15.Count) -Expect $Transplant.Count -Mode atLeast -Samples $m15 `
  -Note "缺失 $($m15.Count) 项"

# ---------- M16 页面层禁止手写 tab 容器 ----------
# 正则锚定「引号后紧跟完整类名」：命中 className="tabs" / "pill-tabs" / {cn("tabs"，
# 不命中 className="api-tabs" / "api-langtabs"（引号后是 api-）与未来的 "tabs-wrap"，
# 因此 Docs 的文档站排版语言无需白名单，也不会误伤。
$m16 = Find-Violations -Pattern 'className=("|\{cn\(\s*")(pill-)?tabs'
Add-Check -Id 'M16' -Name '页面层手写 tab 容器（应为 0，统一走 <Tabs> / <PageTabs>）' -Actual $m16.Count -Expect 0 -Samples $m16 `
  -Note '锚定完整类名：api-tabs / api-langtabs / tabs-wrap 不在命中范围'

# ---------- M17 L1 页签必须接 usePageTabs（视图开关白名单） ----------
# 白名单只有 4 个，且都很短、可审计：
#   Dashboard.tsx / Home.tsx —— 区间切换是同一份数据的口径（L2），不该进 URL；
#   Showcase.tsx             —— 组件展示台，不是业务页；
#   AppManageDialog.tsx     —— 应用管理**弹窗内**的视图开关（L2）。为什么也不进 URL：
#                              弹窗关闭后页签状态没有意义，而给它一条深链等于让同一份
#                              数据页有两个宿主（弹窗 / 全屏），滚动与返回路径各一份。
#                              注意：这是**按文件**豁免，该文件以后不得再放 L1 页签。
$m17 = Find-FileViolations -MustPattern '<Tabs\b' -LackPattern 'usePageTabs' `
  -IgnoreNames @('Dashboard.tsx', 'Home.tsx', 'Showcase.tsx', 'AppManageDialog.tsx')
Add-Check -Id 'M17' -Name 'L1 页签未接 usePageTabs（白名单 Dashboard/Home/Showcase/AppManageDialog）' -Actual $m17.Count -Expect 0 -Samples $m17 `
  -Note '新增免 URL 的页签前先确认它属于 L2 视图开关，再进白名单'

# ---------- M18 同页 idPrefix 不得重复 ----------
# 守 layout.spec 的「同页 DOM id 唯一」；前提是 idPrefix 传字面量（不要用变量/useId）。
$m18 = New-Object System.Collections.ArrayList
foreach ($f in $PageFiles) {
  $raw = Get-Content -Raw -LiteralPath $f.FullName -Encoding UTF8
  $prefixes = @([regex]::Matches($raw, 'idPrefix="([^"]+)"') | ForEach-Object { $_.Groups[1].Value })
  if (-not $prefixes.Count) { continue }
  foreach ($d in @($prefixes | Group-Object | Where-Object { $_.Count -gt 1 })) {
    [void]$m18.Add("$($f.Name): idPrefix「$($d.Name)」×$($d.Count)")
  }
}
Add-Check -Id 'M18' -Name '同页 idPrefix 重复（应为 0）' -Actual $m18.Count -Expect 0 -Samples $m18

# ---------- M19-M21 文案治理（§4.12） ----------
#
# 基线值 = 冻结「当前实测」，只允许降不允许升。
# 为什么不用绝对值 0：还没清完的页面会一直红灯，门禁失去信号价值；
# 也不用 §4.12.3 的「单页 ≤8」：Config.tsx 一个文件装 6 个页签，按文件计数
# 会永久 violation，真实口径是「每个页签 ≤8」。
# 为什么不用硬编码 8：见上。基线表就是这条规则的可审计形态。
#
# 文案规则必须能扫 components/（支付渠道参数表单在那里），但**不能**把整个
# 目录塞进 Find-Violations：M7 的裸 <input|select|textarea> 会在 Input.tsx /
# Textarea.tsx / Select.tsx 上立刻失败。所以单独一份文件集，只喂文案规则。
$CopyFiles = @(
  # Showcase 是组件展示台（故意把每种提示都演一遍），与 M17 的白名单同一条理由
  Get-ChildItem -LiteralPath $PagesDir -Recurse -File -Filter *.tsx |
    Where-Object { $_.Name -ne 'Showcase.tsx' }
  Get-ChildItem -LiteralPath (Join-Path $SrcDir 'components') -Recurse -File -Filter *.tsx |
    Where-Object { $_.Name -notin @('Input.tsx', 'Textarea.tsx', 'Select.tsx', 'Showcase.tsx') }
) | Sort-Object FullName

function Get-RelKey([System.IO.FileInfo]$File) {
  return ($File.FullName.Substring($SrcDir.Length + 1) -replace '\\', '/')
}

# 问号提示：重复渲染的卡片单元按组件预算计（PayChannelCard 每张卡片 2 处）
$TipBaseline = @{
  # 26 = 25 + 「直链 302」「直链有效期」两处问号 - 「本地副本保留」一处问号（字段已删）。
  # 这两条都属于「不解释必误解」：打开 302 等于把预签名地址交给浏览器（有效期内谁都能下），
  # 有效期填 0 是「用默认 300s」而不是「立刻过期」—— 都不写清就会配出一个自己不想要的行为。
  # 2026-11-03 由 26 → 27：+1「可用联系方式渠道」。该字段是**策略白名单**，
  # 只有同时配好发送服务（SMTP / 短信商）的渠道才会展示给终端用户；不解释就会被
  # 当成「勾了却看不到」的 bug —— 同样是「不解释必误解」，非文案膨胀。
  'pages/admin/Config.tsx'                       = 27
  'pages/portal/Keys.tsx'                        = 6
  'components/admin/PayChannelCreateDialog.tsx'  = 4
  # 2026-11-09 移除 `pages/portal/Settings.tsx` = 3：用户中心改为账户设置弹窗的落地页
  # （只剩 PageHead + 弹窗），三个提示原本属于已下线的「通知偏好」本地开关。
  'components/admin/PayChannelCard.tsx'          = 2
  'pages/admin/Packages.tsx'                     = 1
  'components/admin/PayChannelParams.tsx'        = 1
  # 以下为 2026-10-28 收尾时按「实测值冻结」补登（此前漏登 → 门禁红灯但无人看）。
  # 冻结的是**已存在**的提示，规则不变：只允许降，不允许升。
  # 2026-11-09 由 3 → 0：三条提示随表单拆到 AppCreateDialog（新建）与弹窗各页签，
  # 页面本身只剩列表与筛选。基线必须跟着降到 0 —— 留着 3 等于允许页面层再堆三条。
  'pages/admin/Apps.tsx'                         = 0
  'pages/admin/Orders.tsx'                       = 1
  'components/admin/agent/AgentSettingsDialog.tsx' = 5
  # 2026-11-09 接入应用改为卡片 + 弹窗五页签，提示按文件重新登记。
  # 每一条都是「不解释必误解」，不是文案膨胀：
  #   AppCreateDialog 3：slug 创建后不可改（改了全部接入方失效）、说明不对接入方展示、
  #     关闭自助签发后新用户无法自助建密钥（已签发的仍可用）。
  #   BrandTab 3：图标存令牌（换存储/域名不影响）、一句话标语会出现在启动页、
  #     支持链接会渲染成「获取帮助」入口。
  #   AppEditDialog 3：标识创建后不可改、说明不对接入方展示、关闭自助签发的影响。
  #     （基本信息编辑从新建弹窗里独立出来，三个字段的坑与 AppCreateDialog 同源。）
  #   NoticeEditorDrawer 5：停用只是不再下发（记录保留）、开始/结束时间留空的含义、
  #     内容为纯文本（平台不解析 Markdown/HTML，终端渲染环境不可控）、
  #     **上下架只在列表里切换（编辑表单不写该列，改这里不生效）**。
  #     第 5 条与「停用」那条互斥（新建/编辑两个分支各渲染一条），屏幕上的 tip 数没变。
  #   ReleaseEditorDrawer 3：已发布版本的版本号不可改、下载地址必须绝对地址、
  #     强制更新会阻断旧版本全部用户（开之前必须知道代价）。
  #   OverviewTab 1：维护模式不影响签发/支付回调/已生成产物。
  #     （原登记在 RuntimeTab；2026-11-20「在线」页签并入概览，提示随表单一起搬走，总数不变。）
  'pages/admin/apps/AppCreateDialog.tsx'          = 3
  'pages/admin/apps/AppEditDialog.tsx'            = 3
  'pages/admin/apps/tabs/BrandTab.tsx'            = 3
  'pages/admin/apps/tabs/NoticeEditorDrawer.tsx'  = 5
  'pages/admin/apps/tabs/ReleaseEditorDrawer.tsx' = 3
  'pages/admin/apps/tabs/OverviewTab.tsx'         = 1
  # 2026-11-11 模型服务合页：原来的 Channels.tsx(15) / Models.tsx(3) 两个页面被
  # 拆成「渠道表单」「模型表单」两个组件，条数按拆后的实测值重新登记（总量 19 未增）：
  #   ChannelForm 15：与旧渠道页逐条一致（多密钥调度、分组口径、权重 0 的含义、
  #     私有配置三套形状各自的坑、声明式配置四个字段）。搬文件不改文案。
  #   ModelDialog 3：上下文窗口/输出上限两项的「留空=跟随全局」、计费方式切换会清空另一套价目。
  #   BindDialog 1：上游名留空 = 与本模型同名。
  'pages/admin/model-service/ChannelForm.tsx'     = 15
  'pages/admin/model-service/ModelDialog.tsx'     = 3
  'pages/admin/model-service/BindDialog.tsx'      = 1
  # 2026-11-14 文档编辑页整体重写（两栏 + 编辑/预览）：旧的 DocWorkbench 与
  # docsv7/* 全部删除，基线随之换成新文件。全页只保留 6 条问号提示，都是
  # 「不解释必误解」：绑定端点是发布闸门、响应按状态码分别设封套、示例只写
  # {{base_url}}、「生成」会重写四段代码、说明页正文是唯一内容、板块改名立即生效。
  # 隐藏 / 顺序 / 颜色这类一眼可推断的一律不写。
  'components/admin/docs/EndpointForm.tsx' = 3
  'components/admin/docs/NodeForm.tsx'     = 1
  'components/admin/docs/SectionForm.tsx'  = 1
}
# 常驻小字：只允许用在「表单错误 / 区块级规则」上，不允许新增
$HintBaseline = @{
  'pages/admin/Config.tsx'     = 8
  'pages/portal/Models.tsx'    = 5
  'pages/Register.tsx'         = 3
  'pages/portal/Usage.tsx'     = 3
  'pages/portal/Files.tsx'     = 2
  'pages/admin/Packages.tsx'   = 1
  'pages/Login.tsx'            = 1
  # 2026-10-28 按实测值补登（同 M19 说明）：两步验证的说明与错误提示必须留在明面，
  # 折叠成问号会让"为什么登录要验证码"在开启前无处可查。
  'components/portal/MfaCard.tsx' = 7
  'pages/MfaSetup.tsx'            = 4
  # 2026-11-11 模型服务合页（同 M19）：常驻小字随表单搬到组件里，条数按实测登记。
  # 只允许降：新增一条小字要在这里改大数字，评审时看得见。
  'pages/admin/model-service/ChannelForm.tsx' = 1
  'pages/admin/model-service/ModelDialog.tsx' = 2
  'pages/admin/model-service/ModelPanel.tsx'  = 1
}

$m19 = New-Object System.Collections.ArrayList
$m21 = New-Object System.Collections.ArrayList
$m20 = New-Object System.Collections.ArrayList
# 只匹配**文案属性**，不匹配数据键（`accessorKey: "quota"` 这类不能算违规）
$JargonRx = '(label|tip|hint|placeholder|title|sub|desc)="[^"]*(quota|auto_ban|allowed_domains|adapter_id|upstream_model|checksum|\bRPM\b|\bQPS\b)'
foreach ($f in $CopyFiles) {
  $key = Get-RelKey $f
  $raw = Get-Content -Raw -LiteralPath $f.FullName -Encoding UTF8
  # 排除 data-tip（Tip 组件自身的数据属性，不是调用点）
  $tips = ([regex]::Matches($raw, '(?<![-\w])tip=(?:"|\{)')).Count
  $hints = ([regex]::Matches($raw, 'className="hint(?:"|\s)')).Count
  $tipBase = if ($TipBaseline.ContainsKey($key)) { $TipBaseline[$key] } else { 0 }
  $hintBase = if ($HintBaseline.ContainsKey($key)) { $HintBaseline[$key] } else { 0 }
  if ($tips -gt $tipBase) { [void]$m19.Add("${key}: tip $tips > 基线 $tipBase") }
  if ($hints -gt $hintBase) { [void]$m21.Add("${key}: hint $hints > 基线 $hintBase") }
  foreach ($line in @(Select-String -LiteralPath $f.FullName -Pattern $JargonRx -AllMatches -Encoding UTF8)) {
    [void]$m20.Add("$($f.Name):$($line.LineNumber): $($line.Line.Trim())")
  }
}
Add-Check -Id 'M19' -Name '问号提示数超过基线（只允许降，不允许升）' -Actual $m19.Count -Expect 0 -Samples $m19 `
  -Note '§4.12：默认无提示。新增提示前先问「不解释会不会误解」，会就去写问号，不会就别写'
Add-Check -Id 'M20' -Name '可见文案出现技术术语（§4.12.2 强制中文）' -Actual $m20.Count -Expect 0 -Samples $m20 `
  -Note 'quota→额度 / auto_ban→自动禁用 / allowed_domains→域名白名单；只扫文案属性，不扫数据键'
Add-Check -Id 'M21' -Name '常驻小字（div.hint）超过基线（只允许降）' -Actual $m21.Count -Expect 0 -Samples $m21 `
  -Note '出错提示必须留在明面（Field 的 hintWarn）；纯规则说明一律进问号'

# ---------- X1 控件口径令牌 ----------
$x1 = 0
if ($TokensCss -notmatch '--ctl-h\s*:') { [void]$Results.Add([pscustomobject]@{ Id = 'X1'; Name = 'tokens.css 缺 --ctl-h'; Actual = 0; Expect = 1; Status = 'FAIL'; Samples = @(); Note = '' }) }
else {
  $use = ([regex]::Matches($ComponentsCss, 'var\(--ctl-h(-sm)?\)')).Count
  $x1 = if ($use -ge 3) { 1 } else { 0 }
  Add-Check -Id 'X1' -Name '控件高度令牌 --ctl-h 定义并落地（≥3 处消费）' -Actual $x1 -Expect 1 -Mode atLeast -Note "消费 $use 处"
}

# ---------- X2 栅格自包含 ----------
$x2 = 0
foreach ($g in @('grid-2', 'grid-3', 'grid-4')) {
  if ($ComponentsCss -match ('(?s)\.' + $g + '\s*\{[^}]*display:\s*grid')) { $x2++ }
}
Add-Check -Id 'X2' -Name '.grid-2/3/4 自包含 display:grid（3 项）' -Actual $x2 -Expect 3 -Mode atLeast

# ---------- X3 重复组件已删除 ----------
$x3 = if (Test-Path (Join-Path $SrcDir 'components/common/Toolbar.tsx')) { 0 } else { 1 }
Add-Check -Id 'X3' -Name '重复组件 Toolbar.tsx 已删除' -Actual $x3 -Expect 1 -Mode atLeast

# ---------- X4 唯一组件矩阵存在 ----------
$required = @(
  'components/common/Field.tsx',
  'components/common/Card.tsx',
  'components/common/EmptyState.tsx',
  'components/common/SegmentedChips.tsx',
  'components/common/Dialog.tsx',
  'components/common/Drawer.tsx',
  'components/common/useOverlayBehavior.ts',
  'components/common/Tabs.tsx',
  'components/common/PageTabs.tsx',
  'lib/pageTabs.ts',
  'lib/searchParams.ts',
  'lib/table.ts'
)
$lack = @($required | Where-Object { -not (Test-Path (Join-Path $SrcDir $_)) })
# lib/table 允许 .ts / .tsx 两种扩展名（当前为 table.tsx）
if ($lack -contains 'lib/table.ts' -and (Test-Path (Join-Path $SrcDir 'lib/table.tsx'))) {
  $lack = @($lack | Where-Object { $_ -ne 'lib/table.ts' })
}
$x4 = $required.Count - $lack.Count
Add-Check -Id 'X4' -Name '唯一组件矩阵齐备（Field/Card/EmptyState/SegmentedChips/弹层/页签/table）' -Actual $x4 -Expect $required.Count -Mode atLeast -Samples $lack

# ---------- X5 图表渐变 id 逐实例唯一 ----------
$charts = Join-Path $SrcDir 'components/common/Charts.tsx'
$x5 = if ((Test-Path $charts) -and ((Get-Content -Raw -LiteralPath $charts -Encoding UTF8) -match 'useId\(')) { 1 } else { 0 }
Add-Check -Id 'X5' -Name 'TrendChart 渐变 id 使用 useId()（消除重复 id）' -Actual $x5 -Expect 1 -Mode atLeast

# ---------- X6 门户可见页面/组件不得出现供应链字样 ----------
#
# 用户端不能出现「渠道 / 上游 / 适配器」——这些词一个就能说明平台背后还有一层。
# 扫描范围是**用户能看到的全部 tsx**，不是只有 pages/portal/**：
#   - 公开文档页（pages/docs/DocsPage.tsx）与它的渲染件（components/docs/**、DocPage.tsx）
#     是「照着文档接入」的开发者主要入口，漏掉它等于门禁只管了一半；
#   - components/common 整体不扫：里面有管理台分支（CallDetailDrawer 的 !portal 段落
#     合法含「渠道」），扫全目录会逼着人写豁免，最后门禁形同虚设；
#   - 注释不算文案（本项目注释里大量出现这些词是为了解释**为什么不能出现**）。
$PortalScopes = @(
  (Join-Path $PagesDir 'portal'),
  # 公开文档站：页面在 pages/docs（B4 起的实际位置），渲染件在 components/docs。
  # 漏掉前者等于「新组件放进 pages/docs 就绕过这条门禁」—— 而文档站正是
  # 最该守这条的地方（开发者第一次看到的就是它）。
  (Join-Path $PagesDir 'docs'),
  (Join-Path $SrcDir 'components/docs')
)
$PortalFiles = New-Object System.Collections.ArrayList
foreach ($d in $PortalScopes) {
  if (-not (Test-Path $d)) { continue }
  foreach ($f in @(Get-ChildItem -LiteralPath $d -Recurse -File -Filter *.tsx | Sort-Object FullName)) {
    [void]$PortalFiles.Add($f)
  }
}
# 单文件（不在上面两个目录里）：公开文档页 / 展示页 / 共用文档渲染器
foreach ($rel in @('pages/Showcase.tsx', 'components/common/DocPage.tsx')) {
  $p = Join-Path $SrcDir $rel
  if (Test-Path $p) { [void]$PortalFiles.Add((Get-Item -LiteralPath $p)) }
}

$x6 = New-Object System.Collections.ArrayList
# 两组：① 供应链字样；② 已知聚合支付渠道商名（门户只能出现「微信支付/支付宝」这类方式名）
$rxSupply = '(可用渠道|换渠道|上游|适配器|渠道数|虎皮椒|迅虎|易支付|xunhupay)'
foreach ($f in $PortalFiles) {
  $lines = Get-SourceLines $f
  for ($i = 0; $i -lt $lines.Count; $i++) {
    $t = $lines[$i].Trim()
    if ($t.StartsWith('//') -or $t.StartsWith('*') -or $t.StartsWith('/*')) { continue }
    # 去掉单行 JSX/块注释后再判：{/* ... */} 与 /* ... */ 都不算可见文案
    $stripped = [regex]::Replace($lines[$i], '\{?/\*.*?\*/\}?', '')
    if ($stripped -match $rxSupply) { [void]$x6.Add("$($f.Name):$($i + 1): $t") }
  }
}
Add-Check -Id 'X6' -Name '门户页面出现供应链/收单机构字样（渠道/上游/适配器/虎皮椒/易支付…）' -Actual $x6.Count -Expect 0 -Samples $x6 `
  -Note '管理台不受此限：运营本来就要看渠道与收单机构'

# ---------- X7 引用的 CSS 变量必须已定义 ----------
#
# var(--typo) 不会有任何报错：没有 fallback 时整条声明在计算值阶段被丢弃
# （边框整条消失、底色透明、图表线条退回黑色）。改类名/令牌名之后，这是
# 最难靠肉眼发现的一类回归，所以用静态对账守住。
#
# 只认「行首 --x:」形式的定义：那是 :root 与 [data-theme] 里的令牌声明；
# 写在样式属性里当普通值用的 --x 不构成定义。
$DefinedVars = New-Object System.Collections.Hashtable
foreach ($css in @(Get-ChildItem -LiteralPath $SrcDir -Recurse -File -Filter *.css)) {
  $raw = Get-Content -Raw -LiteralPath $css.FullName -Encoding UTF8
  foreach ($m in [regex]::Matches($raw, '(?m)^\s*(--[A-Za-z0-9_-]+)\s*:')) {
    $DefinedVars[$m.Groups[1].Value] = $true
  }
}
$x7 = New-Object System.Collections.ArrayList
foreach ($f in @(Get-ChildItem -LiteralPath $SrcDir -Recurse -File -Include *.css, *.tsx, *.ts)) {
  $lines = Get-SourceLines $f
  for ($i = 0; $i -lt $lines.Count; $i++) {
    $t = $lines[$i].Trim()
    if ($t.StartsWith('//') -or $t.StartsWith('*') -or $t.StartsWith('/*')) { continue }
    foreach ($m in [regex]::Matches($lines[$i], 'var\(\s*(--[A-Za-z0-9_-]+)')) {
      $name = $m.Groups[1].Value
      if (-not $DefinedVars.ContainsKey($name)) { [void]$x7.Add("$($f.Name):$($i + 1): $name") }
    }
  }
}
Add-Check -Id 'X7' -Name '未定义的 CSS 变量引用（var(--x) 必须有定义）' -Actual $x7.Count -Expect 0 -Samples $x7 `
  -Note '没有 fallback 的 var() 失效时整条声明被静默丢弃；有 fallback 也会让暗色主题不跟随'

# ---------- X8 对话区滚动容器白名单 ----------
#
# 一页只允许"对话流 + 侧栏列表 + 输入框 + 弹窗内列表"这几处滚动；多一个
# overflow-y:auto 就可能出现嵌套滚动：滚到底把整页带走、或外侧抢滚动导致
# 对话区永远贴不到底。新增滚动容器必须先在这里登记（=显式决定，而不是顺手写）。
$AgentCssPath = Join-Path $SrcDir 'styles/agent.css'
$x8 = New-Object System.Collections.ArrayList
if (Test-Path -LiteralPath $AgentCssPath) {
  $AllowedScrollers = @(
    '.agent-rail__list',
    '.agent-thread',
    '.agent-tools__list',
    '.agent-composer__input',
    '.agent-picklist',
    '.agent-docs__list'
  )
  $agentCss = Get-Content -Raw -LiteralPath $AgentCssPath -Encoding UTF8
  foreach ($m in [regex]::Matches($agentCss, '(?s)([^{}]+)\{([^{}]*)\}')) {
    $body = $m.Groups[2].Value
    if ($body -notmatch 'overflow-y\s*:\s*auto') { continue }
    $selLines = @($m.Groups[1].Value -split "`r?`n" | Where-Object { $_.Trim() -ne '' })
    $sel = if ($selLines.Count -gt 0) { $selLines[$selLines.Count - 1].Trim() } else { '' }
    $ok = $false
    foreach ($a in $AllowedScrollers) {
      if ($sel -eq $a -or $sel.StartsWith("$a ") -or $sel.StartsWith("$a,") -or $sel.StartsWith("${a}:")) { $ok = $true }
    }
    if (-not $ok) { [void]$x8.Add("$sel -> overflow-y:auto") }
  }
}
Add-Check -Id 'X8' -Name 'agent.css 出现未登记的滚动容器（overflow-y:auto 白名单外）' -Actual $x8.Count -Expect 0 -Samples $x8 `
  -Note '每多一个滚动容器就多一条嵌套滚动/抢滚动的路径；新增必须先登记'

# ---------- X9 折叠块必须有 aria-expanded ----------
#
# 思考块与工具组是"内容可折叠"的唯一入口；缺 aria-expanded 时读屏用户
# 完全感知不到这里还能展开，视觉上也拿不到状态（样式靠 is-open 类）。
$procTsx = Join-Path $SrcDir 'components/admin/agent/AgentProcess.tsx'
$x9 = New-Object System.Collections.ArrayList
if (Test-Path -LiteralPath $procTsx) {
  $proc = Get-Content -Raw -LiteralPath $procTsx -Encoding UTF8
  if ($proc -notmatch 'agent-think__head[\s\S]{0,200}?aria-expanded') { [void]$x9.Add('agent-think__head 缺 aria-expanded') }
  if ($proc -notmatch 'agent-tools__head[\s\S]{0,200}?aria-expanded') { [void]$x9.Add('agent-tools__head 缺 aria-expanded') }
}
Add-Check -Id 'X9' -Name '折叠块缺 aria-expanded（思考块 / 工具组）' -Actual $x9.Count -Expect 0 -Samples $x9 `
  -Note '展开状态必须对读屏可见；条件渲染 + aria-expanded 是本项目既定的折叠契约'

# ---------- X10 文档站类名必须已定义 ----------
#
# className 里写错一个字母不会有任何报错：样式静默消失，页面只是"看着有点怪"。
# 文档站的 dc-* 是 B4 新建的一整套（侧栏三层 / 端点页 / 相对链接），
# 而 X7 只校验 CSS **变量**，管不到类名。
#
# 只扫 dc-* 命名空间：它是文档站专属前缀；runp-* / api-* 与全站共用，
# 混进来会逼着人写豁免（豁免一多，门禁就形同虚设）。
$x10 = New-Object System.Collections.ArrayList
$cssPath = Join-Path $SrcDir 'styles/components.css'
if (Test-Path -LiteralPath $cssPath) {
  $css = Get-Content -Raw -LiteralPath $cssPath -Encoding UTF8
  $defined = New-Object 'System.Collections.Generic.HashSet[string]'
  foreach ($m in [regex]::Matches($css, '\.(dc-[a-z0-9-]+)')) { [void]$defined.Add($m.Groups[1].Value) }

  # 收集的是**绝对路径字符串**，不是 FileInfo：`Get-Content -LiteralPath` 收到
  # FileInfo 时在 PS 5.1 上会按「当前目录 + 文件名」解析，直接 PathNotFound。
  $dcFiles = New-Object System.Collections.ArrayList
  foreach ($d in @((Join-Path $SrcDir 'components/docs'), (Join-Path $PagesDir 'docs'))) {
    if (-not (Test-Path $d)) { continue }
    foreach ($f in @(Get-ChildItem -LiteralPath $d -Recurse -File -Filter *.tsx)) {
      [void]$dcFiles.Add($f.FullName)
    }
  }
  foreach ($path in $dcFiles) {
    $raw = [System.IO.File]::ReadAllText($path)
    $name = [System.IO.Path]::GetFileName($path)
    # 只看 className 的取值（字符串字面量或模板字面量）：普通文本里出现 dc- 不算。
    foreach ($m in [regex]::Matches($raw, 'className=(?:"([^"]*)"|\{`([^`]*)`\})')) {
      $body = if ($m.Groups[1].Success) { $m.Groups[1].Value } else { $m.Groups[2].Value }
      foreach ($c in [regex]::Matches($body, 'dc-[a-z0-9-]+')) {
        if (-not $defined.Contains($c.Value)) {
          [void]$x10.Add("${name}: className 里的 .$($c.Value) 在 components.css 里没有定义")
        }
      }
    }
  }
}
Add-Check -Id 'X10' -Name '文档站 className 里的 dc-* 必须已在 components.css 定义' -Actual $x10.Count -Expect 0 -Samples $x10 `
  -Note '写错类名不会报错，只会静默丢样式'

# ---------- X11 滚动条样式单点定义 ----------
#
# 滚动条一旦散落成「每个容器自己调一下」，必然出现两类回归：
#   ① 新页面忘了写 → 冒出一根 17px 的系统滚动条，且暗色下是浅色轨道；
#   ② 把 `scrollbar-width/color` 与 `::-webkit-scrollbar` 写在同一个元素上 ——
#      Chromium 121+ 见到前者就整体忽略后者，于是「有的地方生效、有的地方不」，
#      而且取决于浏览器版本，本地看不出来。
# 所以全站只允许 src/styles/app.css 一个文件出现滚动条声明（令牌在 tokens.css）。
$x11 = New-Object System.Collections.ArrayList
$sbAllowed = Join-Path $StyleDir 'app.css'
$sbRx = '::-webkit-scrollbar|scrollbar-width|scrollbar-color|scrollbar-gutter'

# 注释里提到 `::-webkit-scrollbar` 是正当的（解释"为什么不用它"时必然出现），
# 不能算违规 —— 不剥注释的话这条门禁第一周就会被当成噪音关掉。逐行维护块注释
# 状态、剥掉行注释，只对真正的代码行做匹配（行号不偏移）。
function Get-CodeLines([System.IO.FileInfo]$File) {
  $lines = Get-SourceLines $File
  $out = New-Object System.Collections.ArrayList
  $inBlock = $false
  foreach ($line in $lines) {
    $s = $line
    $kept = ''
    $i = 0
    while ($i -lt $s.Length) {
      if ($inBlock) {
        $end = $s.IndexOf('*/', $i)
        if ($end -lt 0) { $i = $s.Length; break }
        $inBlock = $false
        $i = $end + 2
      }
      else {
        $b = $s.IndexOf('/*', $i)
        $l = $s.IndexOf('//', $i)
        if ($b -ge 0 -and ($l -lt 0 -or $b -lt $l)) {
          $kept += $s.Substring($i, $b - $i)
          $inBlock = $true
          $i = $b + 2
        }
        elseif ($l -ge 0) {
          $kept += $s.Substring($i, $l - $i)
          break
        }
        else {
          $kept += $s.Substring($i)
          break
        }
      }
    }
    [void]$out.Add($kept)
  }
  return ,$out
}

foreach ($f in @(Get-ChildItem -LiteralPath $SrcDir -Recurse -File -Include *.css, *.tsx, *.ts)) {
  if ($f.FullName -eq $sbAllowed) { continue }
  $code = Get-CodeLines $f
  for ($i = 0; $i -lt $code.Count; $i++) {
    if ($code[$i] -match $sbRx) { [void]$x11.Add("$($f.Name):$($i + 1): $($code[$i].Trim())") }
  }
}
Add-Check -Id 'X11' -Name '滚动条声明出现在 app.css 之外（唯一实现处）' -Actual $x11.Count -Expect 0 -Samples $x11 `
  -Note '样式只在 src/styles/app.css 的「滚动条」区块，令牌在 tokens.css（--sb-*）'

# ---------- X12 页面滚动锁单点实现 ----------
#
# 锁滚动曾散落成两份「存下 body.style.overflow、退出时写回」的快照
# （useOverlayBehavior + MediaPreview）：从抽屉里再开图片预览、或非栈序关闭时，
# 上层释放就把「底下还开着」的页面解锁了 —— 背景能滚、宽度也回去。
# 现在唯一实现是 lib/scrollLock.ts（引用计数 + 按布局实测补偿），
# 任何组件要用锁只能调 lockScroll()，不许自己碰 body 样式。
$x12 = New-Object System.Collections.ArrayList
$lockAllowed = Join-Path $SrcDir 'lib\scrollLock.ts'
$lockRx = 'body\.style\.(overflow|paddingRight)\s*='
foreach ($f in @(Get-ChildItem -LiteralPath $SrcDir -Recurse -File -Include *.ts, *.tsx)) {
  if ($f.FullName -eq $lockAllowed) { continue }
  # 单测要直接读写这两个属性来观察锁的效果，它不是第二套实现
  if ($f.FullName -match '\\test\\') { continue }
  $code = Get-CodeLines $f
  for ($i = 0; $i -lt $code.Count; $i++) {
    if ($code[$i] -match $lockRx) { [void]$x12.Add("$($f.Name):$($i + 1): $($code[$i].Trim())") }
  }
}
Add-Check -Id 'X12' -Name '写 body 滚动样式出现在 lib/scrollLock.ts 之外（锁滚动唯一实现处）' -Actual $x12.Count -Expect 0 -Samples $x12 `
  -Note '组件要用滚动锁只能调 lockScroll()（引用计数 + 幂等释放）；直接存/写 body.style.overflow 会在非栈序关闭时把下层页面解锁'

# ---------- X13 禁止全局预留滚动条位置 ----------
#
# `html{scrollbar-gutter:stable}` 能消掉「内容跨过一屏」时的 10px 横跳，代价是
# **每一条路由**在没有滚动条时也恒定少 10px：`clientWidth` 报 1280，而 body /
# #root / 顶栏的右边界是 1270 —— 右侧出现一条没有拇指的空白带，顶栏看起来被切掉。
# 防跳变的正确位置是 lib/scrollLock.ts 的实测补偿（锁前锁后各量一次布局），
# 不是全局预留；所以 stable / both-edges 一律不允许出现在任何 CSS 里。
$x13 = New-Object System.Collections.ArrayList
foreach ($f in @(Get-ChildItem -LiteralPath $SrcDir -Recurse -File -Include *.css)) {
  $code = Get-CodeLines $f
  for ($i = 0; $i -lt $code.Count; $i++) {
    if ($code[$i] -match 'scrollbar-gutter\s*:\s*(stable|both-edges)') {
      [void]$x13.Add("$($f.Name):$($i + 1): $($code[$i].Trim())")
    }
  }
}
Add-Check -Id 'X13' -Name '全局预留滚动条位置（scrollbar-gutter: stable）' -Actual $x13.Count -Expect 0 -Samples $x13 `
  -Note '会让无滚动条的页面也恒定少一个滚动条宽（顶栏右侧空白）；防跳变请用 lib/scrollLock.ts 的实测补偿'

# ---------- X14 控件宽度不许写在 input 类上（admin-docs.css） ----------
#
# 事故复盘：`components.css` 的 `input[type="text"], …, select, textarea { width:100% }`
# 特异性是 (0,1,1)；`admin-docs.css` 里 `.adp-kv__k { width:170px }` 是 (0,1,0) ——
# 写多少都输。表现是「可选值」的说明框被压成 28px、错误码表把后两列推出屏幕。
# 修法是把列宽交给父级 grid 轨道（控件保持 100%），**不是**换个选择器再写 width。
# 这条门禁只锚定本页的控件类后缀，范围刻意收窄：不误伤 `.adp-rail{width:300px}`
# 这类容器声明，也不会把未来的通用 CSS 检查伪装成"已经做了"。
$x14 = New-Object System.Collections.ArrayList
$admCss = Join-Path $StyleDir 'admin-docs.css'
if (Test-Path $admCss) {
  $ctlSuffix = '__(k|v|key|status|code|desc|label)$|__row$'
  $code = Get-CodeLines (Get-Item $admCss)
  $sel = ''
  for ($i = 0; $i -lt $code.Count; $i++) {
    $line = $code[$i]
    if ($line -match '^\s*([.#][^{]*)\{') { $sel = $Matches[1].Trim() }
    if ($line -match '(?<![-a-z])width\s*:') {
      foreach ($part in ($sel -split ',')) {
        $p = $part.Trim()
        if ($p -match '^\.adp-' -and $p -match $ctlSuffix) {
          [void]$x14.Add("admin-docs.css:$($i + 1): $p { $($line.Trim()) }")
        }
      }
    }
    if ($line -match '\}') { $sel = '' }
  }
}
Add-Check -Id 'X14' -Name 'admin-docs.css 给控件类写了 width（列宽必须来自父级 grid 轨道）' -Actual $x14.Count -Expect 0 -Samples $x14 `
  -Note '全局 input[type]{width:100%} 是 (0,1,1)，单类名 (0,1,0) 写 width 一定输；用 grid-template-columns 定列宽'

# ---------- 汇总输出 ----------
$failed = @($Results | Where-Object { $_.Status -eq 'FAIL' })
$allPass = ($failed.Count -eq 0)

Write-Host ''
Write-Host '== UI contract gate ==' -ForegroundColor Cyan
foreach ($r in $Results) {
  $color = if ($r.Status -eq 'PASS') { 'Green' } else { 'Red' }
  Write-Host ("  [{0}] {1,-4} {2} (actual={3} expect={4})" -f $r.Status, $r.Id, $r.Name, $r.Actual, $r.Expect) -ForegroundColor $color
  foreach ($s in $r.Samples) { Write-Host "        - $s" -ForegroundColor DarkYellow }
}

$lines = New-Object System.Collections.ArrayList
[void]$lines.Add("# UI 契约门禁报告 $Stamp")
[void]$lines.Add('')
[void]$lines.Add("- 总结果：$(if ($allPass) { 'PASS' } else { 'FAIL' })")
[void]$lines.Add('- 依据：`ui-consistency-refactor-plan-v3.md` §5.3 / §7')
[void]$lines.Add("- 扫描范围：`web/src/pages/**/*.tsx`（$($PageFiles.Count) 文件）")
[void]$lines.Add('')
[void]$lines.Add('| 项 | 度量 | 实测 | 期望 | 结果 |')
[void]$lines.Add('|---|---|---|---|---|')
foreach ($r in $Results) {
  [void]$lines.Add("| $($r.Id) | $($r.Name) | $($r.Actual) | $($r.Expect) | $($r.Status) |")
}
foreach ($r in $Results) {
  if ($r.Samples.Count -gt 0 -or $r.Note) {
    [void]$lines.Add('')
    [void]$lines.Add("### $($r.Id) 明细")
    if ($r.Note) { [void]$lines.Add("") ; [void]$lines.Add("> $($r.Note)") }
    foreach ($s in $r.Samples) { [void]$lines.Add("- ``$s``") }
  }
}
[System.IO.File]::WriteAllText($ReportPath, ($lines -join "`n"), (New-Object System.Text.UTF8Encoding($true)))

Write-Host ''
Write-Host "Report: $ReportPath" -ForegroundColor Cyan
if ($allPass) { Write-Host 'UI contracts PASS.' -ForegroundColor Green; exit 0 }
Write-Host "UI contracts FAIL ($($failed.Count) item(s))." -ForegroundColor Red
exit 1
