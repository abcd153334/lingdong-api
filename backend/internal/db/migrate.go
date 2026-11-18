package db

import (
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// AutoMigrate 迁移全部业务表（step-00 §6.2 + S10 announcement_reads + S11 usages_archive
// + C1 call_io + 文档站 model_docs + L2 adapter_specs）；失败即阻断启动。
// 迁移完成后创建 AutoMigrate 无法表达的部分索引（见 indexes.go）。
func AutoMigrate(gdb *gorm.DB) error {
	// 必须先跑：接口文档主键从 endpoint_kind 改成 doc_id，而 GORM 会按新 struct
	// 直接加主键约束 —— 存量行 doc_id 为空时整个启动会卡在建表阶段。
	if _, err := MigrateDocEndpointIdentity(gdb, false); err != nil {
		return err
	}
	// 邀请返利表要从「一行两受益人」换成「一行一受益人」。
	// 必须在 AutoMigrate 之前：同名表已存在时 AutoMigrate 只加列，加完就是
	// 两套金额语义共存（旧列删不掉），等于把双真相源写进库。
	affTab, err := MigrateAffiliateRewardsV2(gdb)
	if err != nil {
		return err
	}
	LogAffiliateTable(affTab)
	if err := gdb.AutoMigrate(models()...); err != nil {
		return fmt.Errorf("db: AutoMigrate 失败: %w", err)
	}
	// M35 必须夹在 AutoMigrate 与 EnsureIndexes 之间：
	// uq_users_phone_norm 是表达式唯一索引（ltrim(phone,'+')），存量里同时存在
	// `+86…` 与 `86…` 时它会撞 23505，而它是 required —— 归一必须先生效。
	if rep, err := MigrateAuth2(gdb, false); err != nil {
		return err
	} else {
		LogAuthMigration2(rep)
	}
	if err := EnsureIndexes(gdb); err != nil {
		return err
	}
	return nil
}

// RunMigrations 是启动期的结构性补充与一次性数据动作（幂等）。
//
// 数据动作有三条，都必须先于业务启动跑完：
//   - M28：把计费整改前落库的**模型价目**（旧 quota_type / 9 条倍率列 /
//     带 `_quota` 后缀的 spec_prices 键）折算成绝对金额（微）；
//   - M29：把**套餐售价**从旧列 `price_cny_cent` 回填到 `price_cent`；
//   - M30：把**存量账务**（余额 / 流水 / 用量 / 订单到账 / 冻结额 …）从「额度」搬到「微」。
//
// 为什么都必须先跑：
//   - M28 没迁 → 模型在新结构里等于「未配置价格」，计费 fail-closed 直接 400；
//   - M29 没迁 → `price_cent = 0` 的套餐在 `order.Create` 判 400 `invalid_param`，
//     "所有套餐都买不了"，而错误码指向参数非法；
//   - M30 没迁 → 存量用户余额/消费/佣金**全部归零**，历史营收统计查不出来。
//
// 三条失败都必须阻断启动，而不是像索引那样只告警（带病启动的表现是
// "钱不对但接口 200"，比启动失败难查得多）。
//
// 除此之外不做任何回填 —— 出厂数据由 Seed 负责。
//
// 调用时机：Seed 之后、LoadSystemConfig 之前。
func RunMigrations(gdb *gorm.DB) error {
	ensureUsageLookupIndexes(gdb)
	// 资金列的非负约束：幂等、可回滚、绝不阻断启动（详见函数注释）。
	// 排在最前面：它只改表约束，不依赖任何数据搬迁。
	EnsureLedgerCheckConstraints(gdb)
	// M36：回收扫描索引（CONCURRENTLY，失败只告警）+ legacy 锚点回填。
	// 回填必须与「回收改用 usages.task_id」的代码同版本发布，否则老行会被当成孤儿。
	ensureReclaimIndexes(gdb)
	if _, err := MigrateReclaimAnchor(gdb, false); err != nil {
		return err
	}
	rep, err := MigrateBillingUnits(gdb, false)
	if err != nil {
		return err
	}
	LogBillingMigration(rep)
	prep, err := MigratePackagesPrice(gdb, false)
	if err != nil {
		return err
	}
	LogPackagesPrice(prep)
	lrep, err := MigrateLedgerUnits(gdb, false)
	if err != nil {
		return err
	}
	LogLedgerMigration(lrep)
	// M31：助手运行参数的列改名与旧默认值升级。
	// 必须排在 AutoMigrate 之后（新列由它建），且在业务启动之前（设置要读新列）。
	arep, err := MigrateAgentContext(gdb, false)
	if err != nil {
		return err
	}
	LogAgentContext(arep)
	// M32：适配器管理（owner_id 归属 + 删死列 docs_id）。
	aarep, err := MigrateAdapterAdmin(gdb, false)
	if err != nil {
		return err
	}
	LogAdapterAdmin(aarep)
	// M33：删 users.token_version 与两个被取代的认证配置键。
	authrep, err := MigrateAuth(gdb, false)
	if err != nil {
		return err
	}
	LogAuthMigration(authrep)
	// M34：删提案上的探测快照列。凭据的唯一来源是 agent_dry_runs（跨轮有效），
	// 留着这一列会让"同轮快照"这个半残来源继续存在。
	proberep, err := MigrateAgentProbe(gdb, false)
	if err != nil {
		return err
	}
	LogAgentProbe(proberep)
	// M37：清掉 storage 配置里的死键（retain_local_days / direct_redirect*）。
	// 必须在 LoadSystemConfig 之前 —— 内存镜像里不该再出现这些键。
	srep, err := MigrateStorageKeys(gdb, false)
	if err != nil {
		return err
	}
	LogStorageMigration(srep)
	// M38：uploads 从「全局 max_size_mb」迁到「按类型上限 + 后缀表 + 压缩」。
	// 失败不阻断启动：Current() 对旧形状是宽松解析（缺字段补出厂值），
	// 上传仍可用，只是"配的类型上限"要等运营在配置页保存一次才完全生效。
	if urep, uerr := MigrateUploadsConfig(gdb, false); uerr != nil {
		logger.L().Error("db: uploads 配置迁移失败（服务继续启动）", zap.Error(uerr))
	} else {
		LogUploadsMigration(urep)
	}
	// M39：给 uploads/files/icons 补 storage_bucket 并回填当前生效的桶。
	// **失败阻断启动**（与 M38 不同）：位置列缺失时清理会按空桶路由，
	// 那种「删除全部落空」比启动失败更难排查。
	brep, berr := MigrateStorageBucket(gdb, false)
	if berr != nil {
		return fmt.Errorf("M39 storage_bucket 回填: %w", berr)
	}
	LogStorageBucketMigration(brep)
	// M40：site.api_base_url（单串）→ api_base_urls（数组），并清掉 logo_text。
	// **失败阻断启动**：新代码只认数组，迁移没跑时门户地址会静默回落请求 Host
	// （反代下是 127.0.0.1），用户看到的每个链接都是错的却不报错。
	siterep, siteerr := MigrateSiteConfig(gdb, false)
	if siteerr != nil {
		return fmt.Errorf("M40 site 配置迁移: %w", siteerr)
	}
	LogSiteMigration(siterep)
	// M42：邀请返利配置归一（删旧键 / 补缺失 / 越界回退出厂值）。
	// **必须**在这里做：Seed 用的是 OnConflict{DoNothing}，已存在的 aff 行不会被更新，
	// 新键读出来全是 0 —— 表现是「开关开着、一分不发」，不报错也不打日志。
	// 按**结果**归一而不是「有没有某个新键」：新版管理台保存过一次的行会同时有旧键与新键，
	// 旧判据命中便永远跳过，旧键再也清不掉（这正是「保存成功、门户全 0」的现场）。
	// 必须在 LoadSystemConfig 之前，否则内存镜像里还是旧结构。
	affrep, afferr := MigrateAffiliateConfig(gdb)
	if afferr != nil {
		return fmt.Errorf("M42 aff 配置归一: %w", afferr)
	}
	LogAffiliateConfig(affrep)
	// M43：图像异步端点并入 images/generations（contracts 里已经没有旧键）。
	// **必须跑在业务启动之前**：旧键留在 models.endpoints / adapter_specs 里
	// 不会报错，只会静默失效（模型少一个端点、绑定体检不认识、出站选路不命中）。
	asyncrep, asyncerr := MigrateAsyncPath(gdb, false)
	if asyncerr != nil {
		return fmt.Errorf("M43 异步路径合并: %w", asyncerr)
	}
	LogAsyncPath(asyncrep)
	// M46：删除文档站 v6 的五张表（组 / 归属 / 组文案 / 说明页 / 接口文案）。
	//
	// 排在最后：前面几条迁移（M43 的端点合并）还会读这些表。
	// M44/M45 已随 v6 一起删除 —— 它们的作用是「清空旧表让启动期重建」，
	// 而这里的表已经不存在了，留着只会在空库上 TRUNCATE 一张不存在的表并阻断启动。
	//
	// **失败阻断启动**：五张表删一半、留下孤儿表，会比启动失败更难查
	//（下次 AutoMigrate 不再建它们，代码里也没人读，于是没人发现残留）。
	dvApplied, dverr := MigrateDocsV7DropLegacy(gdb)
	if dverr != nil {
		return fmt.Errorf("M46 清理旧文档表: %w", dverr)
	}
	LogDocsV7DropLegacy(dvApplied)

	// M47：删除 `doc_node_models`（族改静态白名单后，成员不再有写入方）。
	// 排在 AutoMigrate 之后：AutoMigrate 已不再包含该模型，删表不会与建表来回拉锯。
	nmApplied, nmerr := MigrateDocsV8DropNodeModels(gdb)
	if nmerr != nil {
		return fmt.Errorf("M47 清理成员表: %w", nmerr)
	}
	LogDocsV8DropNodeModels(nmApplied)

	// M48：字段说明三列合一（desc / note / groups → desc）。
	//
	// **失败阻断启动**：这条迁移会把 note 与 groups 两列删掉。如果只合并了一半就
	// 断了（比如中途 panic），那两列里的文字以后再也没有入口能读到 —— 属于静默丢内容。
	// 宁可起不来，也不要带着「说明少了一段」的库继续跑。
	ftChanged, fterr := MigrateDocsFieldText(gdb)
	if fterr != nil {
		return fmt.Errorf("M48 合并字段说明: %w", fterr)
	}
	LogDocsFieldText(ftChanged)

	// M49：文档 schema 变形后的一次性重置（板块入库 / 示例代码入库 / 示例表删除 /
	// 代表模型删除）。当前是全新开发阶段、没有用户数据，因此选择"清空重建"而不是
	// "逐列回填"：回填只会把旧脏行一起带进新结构，之后还要再清一次。
	//
	// **失败阻断启动**：只清了一半的库会让文档站静默少内容（没有报错、只是空白）。
	rsApplied, rserr := MigrateDocsReset(gdb)
	if rserr != nil {
		return fmt.Errorf("M49 重置文档内容: %w", rserr)
	}
	LogDocsReset(rsApplied)

	// M50：接口文档改成「每族一份完整文档」后的一次性重置。
	// 旧行是「共享模板 + 按族删字段」的产物：三族同文、预设/示例按 Kind 共享、
	// 任务读面每族一份。这些是行与行之间的关系，回填补不了 —— 只能清空重播。
	//
	// **失败阻断启动**：只清一半会让文档站静默少内容或留下重复页。
	fsApplied, fserr := MigrateDocsFamilySplit(gdb)
	if fserr != nil {
		return fmt.Errorf("M50 重建文档内容: %w", fserr)
	}
	LogDocsFamilySplit(fsApplied)

	// M51：修正「调试形态」那一列。种子里的 run_hint 曾以"右栏没有路径参数输入框"
	// 为由把这些接口判成不可调试，而面板早就有该输入框 —— 15 篇因此连"发送"都不出现。
	// 只 UPDATE、不删内容，运营在后台改过的行（run_mode != 'none'）不受影响。
	rmApplied, rmerr := MigrateDocsRunMode(gdb)
	if rmerr != nil {
		return fmt.Errorf("M51 修正接口调试形态: %w", rmerr)
	}
	LogDocsRunMode(rmApplied)

	// M52：任务对外号（task_ + ULID）。新行由 model.Task.BeforeCreate 生成，
	// 历史行在这里回填并置非空 —— 顺序见 migrations_tasks_public_id.go 的注释。
	pubApplied, puberr := MigrateTasksPublicID(gdb)
	if puberr != nil {
		return fmt.Errorf("M52 回填任务对外号: %w", puberr)
	}
	LogTasksPublicID(pubApplied)

	// M53：任务面形状变更后重播接口文档（只删 origin='seed' 的接口页）。
	// 必须在播种之前：播种的规则是「已存在就跳过」，不先删就永远不会更新。
	tsApplied, tserr := MigrateDocsTaskShape(gdb)
	if tserr != nil {
		return fmt.Errorf("M53 重播文档任务面: %w", tserr)
	}
	LogDocsTaskShape(tsApplied)

	// M55：幂等键补端点作用域。必须在 EnsureIndexes（建 uq_tasks_idem_v2）之前：
	// 新索引参与唯一性判定，历史行的 NULL 端点会让「同键同端点」也认不出来。
	idemApplied, idemerr := MigrateTasksIdemEndpoint(gdb)
	if idemerr != nil {
		return fmt.Errorf("M55 回填幂等端点: %w", idemerr)
	}
	LogTasksIdemEndpoint(idemApplied)

	// M56：视频提交页按新字段契约重播（只删视频提交页）。
	// 同样必须在播种之前 —— 播种「已存在就跳过」，不先删就永远停在旧字段表。
	vidApplied, viderr := MigrateDocsVideoShape(gdb)
	if viderr != nil {
		return fmt.Errorf("M56 重播视频文档: %w", viderr)
	}
	LogDocsVideoShape(vidApplied)

	// M57：视频提交页按**素材字段收敛**重播，并删掉已下架的「视频生成」通用节点。
	// 位置约束同 M56：必须在播种之前，否则「已存在就跳过」会让旧字段表永远留着。
	matApplied, materr := MigrateDocsVideoMaterial(gdb)
	if materr != nil {
		return fmt.Errorf("M57 重播视频素材字段: %w", materr)
	}
	LogDocsVideoMaterial(matApplied)

	// M58：视频提交页按「示例 model 用真实 id + 素材硬边界」重播。
	// 同样必须在播种之前：播种「已存在就跳过」，不先删就永远停在旧文案。
	exApplied, exerr := MigrateDocsVideoExample(gdb)
	if exerr != nil {
		return fmt.Errorf("M58 重播视频示例: %w", exerr)
	}
	LogDocsVideoExample(exApplied)

	// M59：Muse 视频提交页按**裁剪后的兼容写法段**重播（原来照搬全表，
	// 在「没有 resolution、只收图片」的族页上写了 `size → resolution` 等假承诺）。
	// 位置约束同 M56–M58：必须在播种之前，否则「已存在就跳过」让旧文案永远留着。
	muApplied, muerr := MigrateDocsMuseCompat(gdb)
	if muerr != nil {
		return fmt.Errorf("M59 重播 Muse 兼容段: %w", muerr)
	}
	LogDocsMuseCompat(muApplied)

	// M60：图片三族接口页按新契约重播（Seedream 删 async、size 默认值按模型、笔记去配置话术）。
	// 位置约束同 M56–M59：必须在播种之前，否则「已存在就跳过」让旧文案永远留着。
	imgApplied, imgerr := MigrateDocsImagesReplay(gdb)
	if imgerr != nil {
		return fmt.Errorf("M60 重播图片文档: %w", imgerr)
	}
	LogDocsImagesReplay(imgApplied)

	// M62：同一批页面再重播一次（gpt-image 的尺寸换算表改为由事实渲染 + 补 1:3/3:1）。
	img2Applied, img2Err := MigrateDocsImagesReplay2(gdb)
	if img2Err != nil {
		return fmt.Errorf("M62 重播图片文档: %w", img2Err)
	}
	LogDocsImagesReplay2(img2Applied)

	// M63：视频四族页重播（只为清掉说明里的「渠道 / 上游」等内部视角用词）。
	vidApplied, vidErr := MigrateDocsVideosReplay(gdb)
	if vidErr != nil {
		return fmt.Errorf("M63 重播视频文档: %w", vidErr)
	}
	LogDocsVideosReplay(vidApplied)

	// M61：usages.upstream_spec 从 varchar(16) 加宽到 32（与 billing 常量一致）。
	// 顺序无关但必须在修复上游档位原文落库之前完成 —— 否则结算会因列宽报错回滚。
	usApplied, userr := MigrateUsagesUpstreamSpec(gdb)
	if userr != nil {
		return fmt.Errorf("M61 加宽 usages.upstream_spec: %w", userr)
	}
	LogUsagesUpstreamSpec(usApplied)

	// M64：同步幂等键（列 + 部分唯一索引，DDL 幂等）。
	if err := MigrateUsagesIdemKey(gdb); err != nil {
		return err
	}
	return nil
}

func models() []any {
	return []any{
		&model.User{},
		// 认证域：会话是登录态的唯一真相源；auth_events 是认证事件流（与 admin_audit 分开）；
		// 两张 MFA 表在批次 A 就建好（索引依赖表存在），业务逻辑随后接。
		&model.UserSession{},
		&model.AuthEvent{},
		&model.UserMFA{},
		&model.UserMFABackupCode{},
		&model.APIKey{},
		// App 是「接入应用」：对外 API 的归属锚点（api_keys.app_id / orders.app_id）。
		// 无 DeletedAt —— 下线只停用（status=2），删除会留下孤儿引用（见 model/app.go 注释）。
		&model.App{},
		// 应用级后台的数据：公告与版本发布。均为硬删（版本用 status=3 撤回），
		// 只有归属列 app_id，不需要额外的关联表。
		&model.AppNotice{},
		&model.AppRelease{},
		// 应用插件（声明式）：目录 → 安装 → 记录，父表在前。
		// 三张表都**没有 DeletedAt**：目录与安装在卸载后要留着（否则清除数据的入口消失），
		// 记录是硬删。所以它们的唯一索引都是普通唯一索引（不带 deleted_at 条件）。
		&model.AppPlugin{},
		&model.AppPluginInstall{},
		&model.AppPluginRecord{},
		&model.Model{},
		&model.ModelBinding{},
		&model.Channel{},
		&model.Vendor{},
		&model.SystemConfig{},
		&model.Usage{},
		&model.UsageArchive{},
		&model.UsageDaily{},
		&model.Task{},
		&model.Order{},
		&model.Package{},
		&model.RedeemCode{},
		&model.Transaction{},
		&model.PaymentDedup{},
		// AffiliateReward 是被邀请人每笔订单的返利凭据，也是返利额度的唯一来源
		// （可转出/冷却中/累计都由它派生）。退款成功时把未到期的行作废。
		&model.AffiliateReward{},
		&model.PayOrphanNotify{},
		// PaymentEvent 是 Stripe 回调事件的幂等去重（event id 级别）。
		&model.PaymentEvent{},
		&model.File{},
		&model.AdminAudit{},
		&model.Upload{},
		// Icon 是运营上传的图标（models/vendors/channels 的 icon 列按 `upload:<id>` 引用）。
		// 与 uploads 分开：uploads 有 24h 过期，图标是长期配置数据（见 model/icon.go）。
		&model.Icon{},
		&model.AnnouncementRead{},
		// DataMigration 记录一次性数据动作（如 M11 清理占位支付渠道）是否已执行，
		// 防止 RunMigrations 每次启动重跑时把运营手工恢复的数据再次删掉。
		&model.DataMigration{},
		// CallIO 是调用记录的原始报文与媒体链接（C1 详情层）。
		// 与 usages 一对零或一，不随归档搬迁（usage_id 稳定，按 id 反查）。
		&model.CallIO{},
		// 注意：model_docs（旧的单模型文案表）已由 m27 删除，不再进 AutoMigrate ——
		// 留着它会在每次启动时把表重新建出来，然后 m27 再删，来回拉锯。
		// 文档内容 v7：板块 / 节点 / 接口 / 字段 / 枚举 / 响应变体 / 预设 / 错误码。
		// 这一组是**文档内容的唯一真相**（v6 的五张表已由 M46 删除）。
		//
		// 注意：不要再把它们加回来 —— 加回 AutoMigrate 会让每次启动重建表、
		// M46 再删一次，来回拉锯（model_docs 与 m27 踩过同一个坑）。
		// 顺序：板块必须在节点之前（节点的 section 指向它）。
		&model.DocSection{},
		&model.DocNode{},
		&model.DocEndpoint{},
		&model.DocEndpointField{},
		&model.DocFieldEnum{},
		&model.DocResponseVariant{},
		&model.DocPreset{},
		&model.DocEndpointError{},
		// AdapterSpec 是 L2 数据驱动适配器的唯一真相源（渠道以 `spec:<slug>` 引用）。
		// 取代已删除的 adapters 表：那份表存的是"插件元数据"，而这里存的是"API 形状"。
		&model.AdapterSpec{},
		// AI 编排（/admin/agent）：文档 → 会话 → 消息 → 提案 → 运行记账 → 设置。
		// 提案是写生产表的唯一凭证，apply 才真正落库（见 model/agent.go 文件头）。
		&model.AgentDoc{},
		&model.AgentSession{},
		&model.AgentMessage{},
		&model.AgentProposal{},
		&model.AgentRun{},
		&model.AgentSetting{}, &model.AgentAsk{},
		// 连通性探测的持久凭据（跨轮复用）：早前只在 run 内存里，跨轮即丢。
		&model.AgentDryRun{},
		// 一键更新：指令/进度（jobs）与宿主机更新程序的单行心跳。
		// 状态必须在这里而不是 Redis —— 更新会替换 backend 自己，新容器起来后
		// 要用同一行回答"上次更新到哪一步了"。
		&model.ReleaseUpdateJob{},
		&model.ReleaseAgentState{},
	}
}
