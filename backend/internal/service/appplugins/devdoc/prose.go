package devdoc

// 正文（"为什么 / 怎么用"）。表格由 doc.go 生成，不写在这里：
// 数字与词表只有一个来源，正文里出现具体上限就会漂。
//
// 约定：`xHead` 是标题与导语（表格之前），`xTail` 是表格之后的收尾。
// 不需要表格的段落只有一个常量。

const proseHeader = `# 应用插件开发手册

> 本文档由服务端代码生成，随部署版本变化。下载到本地的那一份，
> 与「接入应用 → 管理 → 插件」里看到的能力**逐字一致**。
>
> 适用对象：要给本平台写插件的开发者、以及用 AI 生成插件定义的运营。
> 读完之后你应该能独立写出一个可导入、可安装、可读写数据的插件。

## 目录

1. [这是什么](#1-这是什么)
2. [五分钟上手](#2-五分钟上手)
3. [manifest 全字段](#3-manifest-全字段)
4. [字段类型矩阵](#4-字段类型矩阵)
5. [列表：搜索、筛选、排序、分页](#5-列表搜索筛选排序分页)
6. [表单布局](#6-表单布局)
7. [动作与权限](#7-动作与权限)
8. [管理端接口](#8-管理端接口)
9. [对外只读面](#9-对外只读面)
10. [升级与破坏性变更](#10-升级与破坏性变更)
11. [错误码](#11-错误码)
12. [上限](#12-上限)
13. [安全边界](#13-安全边界)
14. [给 AI 的提示词模板](#14-给-ai-的提示词模板)
15. [完整示例](#15-完整示例)

## 1. 这是什么

应用插件是**纯声明式**的扩展：一份 JSON（manifest）描述"我要存什么数据、怎么展示、
允许谁做什么"，平台据此生成列表页、表单、CSV 导出与管理接口。

三条必须记住的边界：

- **不执行任何代码**。没有钩子、没有脚本、没有 webhook。插件能做的事，全部由
  manifest 里声明的类型、动作、白名单决定；manifest 之外的能力不存在。
- **不建表**。所有记录进统一存储，按 app_id + plugin_key + entity 隔离；
  字段是 JSON 里的键。因此加字段不需要 DDL，删字段也不会丢历史（只是不再展示）。
- **不越权**。插件数据只属于"安装了它的那个应用"，同一个插件在两个应用里的数据
  互不可见；对外只读还要求逐列白名单。

## 2. 五分钟上手

第 1 步：复制下面的示例（也可以在管理台点「插件 → 导入插件 → 填入示例」）。

`

const proseQuickStartTail = `
第 2 步：导入（新建时 expected_version 传 0）：

    curl -X PUT "http://127.0.0.1:8080/api/admin/app-plugins/ticket" \
      -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
      -d '{"manifest": { ...上面的 JSON... }, "expected_version": 0}'

先体检、不落库，把 dry_run=1 加到地址上即可；返回体里的 breaking 与 affected
会告诉你"这次改动会不会破坏已有数据"。

第 3 步：安装到某个应用（app_id 见「接入应用」列表）：

    curl -X POST "http://127.0.0.1:8080/api/admin/apps/2/plugins" \
      -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
      -d '{"key": "ticket", "config": {"slaHours": 8}}'

第 4 步：打开「接入应用 → 管理 → 插件」页签，点「打开」，就能新建记录了。

## 3. manifest 全字段

根级：

| 字段 | 必填 | 语义 |
|---|---|---|
| apiVersion | 是 | 固定 1。基座读到别的值直接拒绝 |
| key | 是 | 插件标识。小写字母开头，只含字母/数字/下划线/连字符，最长 32 位；**导入后不可改**（它进了安装行与数据行） |
| name | 是 | 显示名，1-32 字 |
| version | 是 | x.y.z，用于漂移提示与升级判据 |
| description | 否 | 一句话说明（列表页可见） |
| icon | 否 | 空、builtin:<key> 或 upload:<id>。留空比乱挑一个更清楚 |
| config | 否 | 安装级配置字段（同一套字段类型与校验） |
| entities | 是 | 数据对象列表，至少一个 |
| limits | 否 | maxRecords：本插件每个对象的记录数上限（只能比基座上限更严） |

数据对象（entities[]）：

| 字段 | 必填 | 语义 |
|---|---|---|
| key | 是 | 对象标识（进 URL 的 entity 参数） |
| name | 是 | 显示名（页签、弹窗标题、空态文案都用它） |
| unique | 否 | 业务唯一字段：写入时按它查重（trim + 忽略大小写），重复返回 409 |
| fields | 是 | 字段定义，至少一个；见下 |
| list | 是 | 列表视图：columns（列）、actions（能力）、sort、filters |
| form | 否 | 表单布局：分段与列数。不声明 = 单段单列 |
| public | 否 | 对外只读的**字段白名单**；空 = 这个对象不对外 |

字段（fields[] 与 config[] 共用）：

| 字段 | 必填 | 语义 |
|---|---|---|
| key | 是 | 字段标识，规则同插件 key；不能是 id / record_key / version / created_at / updated_at（会与记录外壳撞名） |
| label | 是 | 显示名，1-32 字 |
| type | 是 | 见下一节的类型矩阵 |
| required | 否 | 必填。缺键即 422；有 default 时缺键补默认值 |
| maxLen | 否 | 长度上限；不写按类型给默认值（见类型矩阵） |
| options | enum 必填 | 枚举取值，每个 1-32 字，不许重复 |
| default | 否 | 表单初值；必填字段缺键时会**入账**这个值，因此它自身必须合法 |
| help | 否 | 字段下方常驻说明（"填什么"），不是"为什么这么填" |
| placeholder | 否 | 输入框占位提示 |
| readOnly | 否 | 表单里禁改（不是安全边界） |
| min / max | 否 | 仅 int：写入侧强制范围 |

## 4. 字段类型矩阵

类型词表是**封闭的**：只有下表这七种。没有 money（JSON 数字是浮点，金额会掉精度）、
没有 file（上传件 24 小时过期，长期引用会静默变空）、没有富文本、没有子表。

`

const proseTypesTail = `
"可排序"决定它能不能出现在 list.sort；"可筛选"决定它能不能出现在 list.filters
（算子由类型决定，不需要额外声明）。

## 5. 列表：搜索、筛选、排序、分页

`

const proseListTail = `
排序方言 sort=<字段>:asc|desc 是全站统一的，插件列表与其它列表页吃同一个解析器。
不传 sort 时按 list.sort 的**首项**排；任何排序都会用 id DESC 兜底，
保证"排序值相同"的行在翻页时不重复、不漏。

筛选的写法由算子决定：

| 类型 | 算子 | 写法 | 例子 |
|---|---|---|---|
| enum / bool | 等值 | f_<key>=<值> | f_status=open、f_done=1 |
| int / date | 区间 | f_<key>=起..止（任一侧可空） | f_due=2026-01-01..2026-01-31、f_score=..60 |
| string | 包含 | f_<key>=<片段> | f_owner=张 |

三条硬规则：

- 白名单外的字段一律 **400**（不是静默忽略）。"解析完悄悄丢掉"会让界面显示"已筛选"
  而数据是全量，比报错危险得多。
- 筛选条件同时作用于**列表、计数、导出**。屏幕上筛过的，导出的就是同一批行。
- 值的绑定全部走参数，字段名只能来自 manifest —— 没有拼接注入的余地。

导出恒按创建顺序（id ASC）、keyset 翻页：按数据列排序要整表排序或吃满内存，
5 万行的上限就是为此设的。导出前请先用筛选把范围收到需要的那部分。

## 6. 表单布局

不声明 form 时，表单是单段单列、按字段声明顺序排列 —— 五个字段以内这样最省事。

字段多了就分段：

    "form": {
      "sections": [
        { "title": "基本信息", "columns": 2, "fields": ["ticketNo", "title"] },
        { "title": "处理", "columns": 1, "fields": ["status", "due", "detail"] }
      ]
    }

- columns: 2 只在**段内**生效；手机档（≤767px）自动降为 1 列。
- 开关（bool）字段在两列段里横跨整行 —— 半宽开关右边留白容易被当成加载失败。
- **漏写的字段不会消失**：没进任何分段的字段会被追加到末段（标题由服务端给，
  见第 12 节的"其他"段），所以"加了字段却没有输入框"在结构上不会发生。
  反过来也别指望靠"不写"来隐藏字段：要禁改就用 readOnly，要不展示就别声明。
- 新建记录时表单底部有「保存并继续」：保存成功但不关窗，表单回到默认值，适合连续录入。
- help 与错误提示共用一个位置：**出错时显示错误**（用户那一刻要看的是原因，
  不是再读一遍说明）。所以 help 只写"填什么"，规则解释写进本文档。

## 7. 动作与权限

列表视图里声明的 actions 就是**全部**可用能力：

`

const proseActionsTail = `
没声明的动作，接口直接拒绝（不是"隐藏按钮"）：导出没声明就拒绝导出，改没声明就拒绝改。

安装状态决定能不能写，与 actions 是两把锁：

| 安装状态 | 读 | 写 | 界面 |
|---|---|---|---|
| 1 启用 | 可以 | 可以 | 正常 |
| 2 停用 | 可以 | 拒绝 | 顶部横幅说明 |
| 3 已卸载 | 拒绝 | 拒绝 | 顶部横幅 + 可重新启用 |

卸载**不删数据**；要真删得先卸载，再走「清除数据」。这条顺序是刻意的：
"卸载"是一次误点就能撤销的操作，"删数据"不能。

## 8. 管理端接口

全部挂在 /api/admin 下，需要管理员凭证。返回体统一是 {code, message, data}。

`

const proseAPITail = `
导入 / 升级的请求体：

    PUT /api/admin/app-plugins/:key?dry_run=1
    { "manifest": { ... }, "expected_version": 3, "force": false }

- expected_version 是**当前目录项的版本号**（新建传 0）。并发下第二次会拿到 409，
  避免"两个人的旧稿互相覆盖"。版本号从目录列表取（q 精确命中那一行），
  不要拿 404 去探测"插件是否存在"。
- dry_run=1 只体检不落库，返回 applied: false 与问题清单。
- force: true 才允许破坏性变更（见第 10 节）。

记录读写：

    # 列表（筛选 + 排序 + 分页）
    curl -G "http://127.0.0.1:8080/api/admin/apps/2/plugins/ticket/records" \
      --data-urlencode "entity=ticket" --data-urlencode "f_status=open" \
      --data-urlencode "sort=due:asc" --data-urlencode "page=1" --data-urlencode "page_size=20" \
      -H "Authorization: Bearer $TOKEN"

    # 新建
    curl -X POST ".../records" -H "Authorization: Bearer $TOKEN" \
      -H "Content-Type: application/json" \
      -d '{"entity":"ticket","data":{"ticketNo":"T-1001","title":"打不开","status":"open"}}'

    # 整条替换（必须带当前 version）
    curl -X PUT ".../records/01K...ABC" -H "Authorization: Bearer $TOKEN" \
      -H "Content-Type: application/json" \
      -d '{"entity":"ticket","version":3,"data":{"ticketNo":"T-1001","title":"已修复","status":"done"}}'

    # 批量删除（单事务）
    curl -X POST ".../records/batch-delete" -H "Authorization: Bearer $TOKEN" \
      -H "Content-Type: application/json" \
      -d '{"entity":"ticket","record_keys":["01K...A","01K...B"]}'

写入语义：

- **只认声明过的字段**。多传一个键就是 422（path: data.xxx，消息"没有这个字段"）。
  这条比"忽略未知字段"严格，但它保证"我传了 A，库里就有 A"，不会有安静丢失。
- **空值等于不传**。空串/空数组不会入库；必填字段被清空会得到 422。
- **更新是整条替换**，不是 PATCH：没传的字段就是被清空。
- **乐观锁**：version 必须等于库里当前值，否则 409。重新取一次再改即可。
- 记录的对外编号是 record_key（ULID），在所有接口与审计里都用它，别用内部自增 id。

## 9. 对外只读面

给终端用户免登录读取时用：

    GET /api/apps/:app_slug/p/:plugin_key/:entity

两把锁必须同时打开才会返回数据，否则一律 404（不区分原因 —— 免得变成"探测有没有
这个插件"的面）：

1. 安装级：该应用的这个插件打开了「对外只读」；
2. 定义级：该对象的 public 里列了字段。

只下发白名单字段（外加记录编号）；public 为空的对象整体不对外。
白名单是逐列的，不是"整个对象公开"的开关：以后加字段不会悄悄多暴露一列。

## 10. 升级与破坏性变更

同一个 key 再次导入即为升级。四类变更被认为**会破坏已有数据**，必须 force: true：

1. 删掉一个数据对象（它的记录还在库里，但再也读不到）；
2. 删掉一个字段（同上，值还在，接口不再返回）；
3. 改字段类型（旧值可能不符合新类型的校验，读写都会别扭）；
4. 换业务唯一字段（unique）：旧记录的唯一值会在同一事务里按新字段重算，
   撞重复会整体失败。

不破坏但需要人跟进的事，会出现在 affected 里：

- 新增**必填**配置项：已安装的应用需要补填（补齐前界面会提示，已有写入不受影响）。

升级后旧数据仍按原样读：展示按**当前** manifest 投影，已删字段不会出现在列表与表单里，
因此"打开旧记录 → 保存"不会因为残留字段而失败。

## 11. 错误码

`

const proseErrorsTail = `
plugin_manifest_invalid（422）的响应体里带 data.problems[]，每条是 {path, message}，
path 形如 entities[0].fields[2].type —— 直接定位到出错的那一行，不用逐段猜。
一次会收全**所有**问题，改一轮就能过。

记录写入的错误同样带 problems[]，path 形如 data.ticketNo，
前端会把它贴在对应输入框下面。

## 12. 上限

`

const proseLimitsTail = `
manifest 只能声明**更严**的上限，不能更宽：limits.maxRecords 超过基座上限会被拒。

## 13. 安全边界

- **鉴权**：所有管理端接口要管理员凭证；对外只读要"安装级 + 定义级"双闸。
- **隔离**：查询恒带 app_id 与 plugin_key；一个应用看不到另一个应用的记录。
- **注入**：字段名只能来自 manifest（已校验），值全部参数绑定；LIKE 的 % 与 _
  已转义，搜 a_b 不会命中 axb。
- **输出**：CSV 导出对表头与单元格做转义（防公式注入）；记录按 JSON 下发，不做 HTML 拼接。
- **readOnly 不是权限**：它只影响表单渲染，脚本直接写仍能改。
  真正不让人写的手段是"不给 update 动作"或"停用安装"。
- **审计**：导入、安装、配置变更、记录增删改、批量删除都留痕；批量删除只记前 20 个编号
  并标注截断（审计的目标标识放不下上百个 key）。

看起来也能写、但基座**明确不支持**的几种做法（写进 manifest 会被拒，或者写进去也没人读）：

| 想做的事 | 为什么不行 | 改用 |
|---|---|---|
| 存金额 | JSON 数字是浮点，0.1+0.2 会掉精度 | 存整数（分），展示时除以 100 |
| 存附件/图片 | 上传件 24 小时过期，长期引用会静默变空 | 存链接（link）+ 自己的对象存储 |
| 子表/多行明细 | 记录是扁平 JSON，没有嵌套校验 | 拆成第二个对象，用单号关联 |
| 自动发通知 | 插件不执行代码、不发外部请求 | 用平台的公告/通知 |
| 自定义页面布局 | 布局只有 list.columns 与 form.sections 两种表达 | 用这两者组合 |
| 计算字段/公式 | 不执行代码 | 在写入前由调用方算好再提交 |
| webhook / script / sql 字段 | 这些键不在词表里，导入即 422 | 没有等价物：需要外部动作就走平台的开放接口 |

## 14. 给 AI 的提示词模板

把下面这段连同"你要存什么"一起交给模型，产出的 manifest 通常可以直接导入：

    请输出一份应用插件 manifest（JSON，apiVersion=1），必须满足：
    1. 只用这些字段类型：string / text / int / bool / enum / date / link；
    2. 每个对象至少一个字段；业务唯一值放进 unique（string，maxLen ≤ 64）；
    3. list.columns 最多 8 列，只放"列表里一眼要看"的；长文本不要进列表；
    4. list.actions 只声明真正需要的：create/update/delete/export/batch_delete；
    5. list.sort 最多 3 项（不要用 text/link）；list.filters 最多 4 项；
    6. 必填字段必须给 default，或明确说明由谁填；
    7. 需要分组的表单用 form.sections（最多 4 段，columns 只能 1 或 2），
       不要漏字段 —— 漏掉的会被追加到末段；
    8. 需要对外只读的字段才写进 public，逐列决定；
    9. 不要出现 money / file / 脚本 / webhook / 子表 —— 基座不支持。
    只输出 JSON，不要解释。

导入前先 dry_run=1 体检一次：problems[] 会直接告诉你哪一行不合法。

## 15. 完整示例

一份可以直接导入的定义（工单：配置 + 五种字段 + 列表 + 对外只读白名单）：

`
