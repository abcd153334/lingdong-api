// 灵动 API 后端入口（S0 工程骨架，无业务实现）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/controller"
	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/router"
	"lingdong-api/internal/service/agent"
	"lingdong-api/internal/service/authcfg"
	"lingdong-api/internal/service/authevent"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/calllog"
	channelsvc "lingdong-api/internal/service/channel"
	"lingdong-api/internal/service/configsync"
	"lingdong-api/internal/service/docdocs"
	"lingdong-api/internal/service/presets"
	"lingdong-api/internal/service/pricing"
	specsvc "lingdong-api/internal/service/spec"
	"lingdong-api/internal/service/stats"
	"lingdong-api/internal/service/storage"
	"lingdong-api/internal/service/verify"
	"lingdong-api/internal/worker"
)

// httpShutdownBudget 是收到 SIGTERM 后等待在途 HTTP 请求的上限。
//
// 30s 覆盖 chat / embedding 这类秒级请求；同步图像（应用侧最长等 780s）不可能靠
// 调大预算解决 —— 那会让每次发布都要等十几分钟。长请求靠发布流程解决：
// 先从 LB/Nginx 摘流、等在途请求落地，再发信号。
// 被切断的长请求按"客户端断开"处理：全额释放、不结算（见 deploy README 的断连口径）。
const httpShutdownBudget = 30 * time.Second

// httpReadTimeout 解析 LD_HTTP_READ_TIMEOUT（秒，0=不设）。
//
// 用环境变量而不是常量：不同部署的上传体积与链路差异很大（跨境慢链路传 20MB），
// 120s 是个能挡住慢速攻击、又不至于误杀正常上传的折中值，但运维必须能调。
func httpReadTimeout() time.Duration {
	raw := os.Getenv("LD_HTTP_READ_TIMEOUT")
	if raw == "" {
		return 120 * time.Second
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 120 * time.Second
	}
	return time.Duration(n) * time.Second
}

func main() {
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()
	if *showVersion {
		fmt.Printf("lingdong-api %s\n", controller.Version)
		return
	}

	env := conf.Load()
	// 敏感环境变量已进入 Env 结构，立刻从进程环境中移除：
	// 插件子进程与同 UID 的 /proc/<pid>/environ 都读不到宿主密钥（B0 的跨平台主措施）。
	conf.UnsetSensitiveEnv()
	logger.Init(env.LogLevel)
	defer logger.Sync()
	log := logger.L()

	// gin 的运行模式必须在 router.New() **之前**设定：debug 模式会在注册路由时
	// 把完整路由表打到 stderr（管理端路径一览），而生产日志是要长期留存/外发的。
	switch env.AppEnv {
	case "dev", "test":
		gin.SetMode(gin.DebugMode)
	default:
		gin.SetMode(gin.ReleaseMode)
	}

	// 密钥闸门必须排在**任何数据访问之前**：空/公开的 JWT_SECRET 等于把管理员
	// 登录态交给任何读过源码的人，此时连数据库都不该连。
	if warns, verr := conf.ValidateSecrets(env); verr != nil {
		log.Fatal("密钥配置不安全，拒绝启动", zap.Error(verr))
	} else {
		for _, w := range warns {
			log.Warn("密钥配置风险（仅开发环境允许）", zap.String("detail", w))
		}
	}

	// 节点模式（每台机器自己的属性，必须走环境变量）：非法值拒绝启动。
	// 拼错时若静默按 full 跑，运维会以为"已经关掉了前端"，实际门户还开着。
	if err := conf.ValidateNodeMode(env); err != nil {
		log.Fatal("节点模式配置非法，拒绝启动", zap.Error(err))
	}
	if env.IsAPIMode() {
		log.Info("节点模式：纯 API（不提供门户/管理台/文档站；未登记路径一律 404）",
			zap.Bool("cluster", env.ClusterMode))
	} else {
		log.Info("节点模式：完整（门户 + API）",
			zap.Bool("cluster", env.ClusterMode))
	}

	gdb, err := db.Init(env.DatabaseURL)
	if err != nil {
		log.Fatal("初始化数据库失败", zap.Error(err))
	}
	// 日界自检：门户「今日/本月」由 LD_TZ 决定，而依赖 CURRENT_DATE 的旧查询走
	// 数据库会话时区。两者偏移不一致时每天 00:00–08:00 的调用会归错日，且没有任何报错。
	stats.CheckSessionTimezone(context.Background())
	if err := db.WithMigrationLock(gdb, func() error {
		if err := db.AutoMigrate(gdb); err != nil {
			return fmt.Errorf("表结构迁移: %w", err)
		}
		if err := db.Seed(gdb); err != nil {
			return fmt.Errorf("种子数据: %w", err)
		}
		// 内置厂商预设（火山方舟等）：独立开关 LD_BUILTIN_PRESETS、独立幂等标记，
		// 默认**全部停用**，等运营填自己的密钥后再启用。必须在 AutoMigrate / Seed 之后。
		if err := presets.Seed(gdb); err != nil {
			return fmt.Errorf("内置厂商预设: %w", err)
		}
		// 支付相关数据迁移（幂等）：回填 orders.expire_at 与支付身份、关闭不可支付订单、
		// 给存量库补齐 pay_channels 的 protocol 与虎皮椒渠道。必须在 LoadSystemConfig 之前，
		// 否则补齐的渠道进不了内存镜像。
		if err := db.RunMigrations(gdb); err != nil {
			return fmt.Errorf("数据迁移: %w", err)
		}
		// 渠道私有配置清理（幂等）：删掉适配器已不认识的废弃键。
		// 必须紧跟在 RunMigrations 之后、开始服务之前 —— 放在业务启动后会有一段
		// "配置脏、请求全 503"的窗口；放在 AutoMigrate 之前则表可能还不存在。
		// 失败不阻断启动：清理失败是"某条配置仍需人工处理"，不是"服务不能提供"。
		optRep, optErr := channelsvc.MigrateChannelOptions(gdb, false)
		if optErr != nil {
			logger.L().Error("渠道私有配置清理失败（服务继续启动）", zap.Error(optErr))
		} else {
			channelsvc.LogChannelOptions(optRep)
		}
		// 启动体检：清理后仍有不合法的配置时逐条报错（不阻断启动）。
		// 没有这一步，脏配置要等到第一个真实请求才暴露，且表现是「整渠道 503」。
		if audit, aerr := channelsvc.AuditAdapterOptions(gdb); aerr != nil {
			logger.L().Warn("渠道私有配置体检失败（服务继续启动）", zap.Error(aerr))
		} else {
			channelsvc.LogOptionsAudit(audit)
		}
		return nil
	}); err != nil {
		log.Fatal("数据库准备失败（迁移/种子），拒绝启动", zap.Error(err))
	}
	// system_config 必须在文档站准备**之前**加载：发布闸门要用站点域名白名单
	// （`E_EXTERNAL_HOST` 允许的 host 来自这里），晚一步就会把合规的站内地址判成外链。
	if cfg, err := db.LoadSystemConfig(gdb); err != nil {
		// Error 而不是 Warn：内存镜像里没有 storage 时，写路径会直接 503 拒绝，
		// 绝不回落本地磁盘。运维需要立刻知道「现在传不了文件」，而不是从用户投诉里发现。
		log.Error("加载 system_config 失败：存储写入位置未确定，上传/产物转存将 503（不会回落本地磁盘），请检查数据库与配置行",
			zap.Error(err))
	} else {
		conf.System().Reload(cfg)
		log.Info("system_config 已加载", zap.Int("keys", len(cfg)))
	}
	// 文档站准备放到**监听之前同步执行**：它只做 SELECT/INSERT（无网络），
	// 换来「端口一通，文档就是完整的」——异步预热会让最初几分钟的 /docs 缺页，
	// 而这种缺页在监控上表现为「文档站时好时坏」，最难归因。
	//
	// 多副本：内部用 advisory lock，只有抢到锁的实例写库，其余实例直接跳过
	// （读路径不依赖这把锁，数据由写它的实例负责）。
	// 纯 API 节点没有文档站，跳过。
	if env.IsAPIMode() {
		log.Info("纯 API 节点：跳过文档站准备（文档站不对外提供）")
	} else {
		prepareDocs(gdb)
	}
	// L2 规格解析器接入 relay 注册表：渠道以 `adapter_id = spec:<slug>` 引用规格。
	// 必须在开始服务前装配（否则首批请求会在选路期把所有规格渠道判成"适配器未注册"）。
	specsvc.Install()

	// 计价体检：档位价填成元 / 填成小数时 spec_prices 解不出来，全链路不报错但账单会错。
	if bad, err := billing.UnparsableSpecModels(db.DB); err != nil {
		log.Warn("检查模型计价配置失败", zap.Error(err))
	} else if len(bad) > 0 {
		log.Error("以下模型的档位价无法解析（价格必须是整数微，不能填元或小数）：文档不会显示档位，下单也会命中不到价格",
			zap.Strings("models", bad))
	}

	// 生产环境若仍存在 .env 文件，提示移除：它会被同 UID 的进程直接读取。
	if env.AppEnv != "dev" {
		if _, statErr := os.Stat(".env"); statErr == nil {
			log.Warn("生产环境检测到 .env 文件，请移除（同 UID 的进程可能读取其中的密钥）")
		}
	}

	// Redis 不可用仅降级（限流与 leader 锁跳过），不阻断启动
	db.InitRedis(env.RedisAddr, env.RedisPassword, env.RedisDB)

	// 写入位置与指标：一条日志 + 两个抓取时指标，回答「现在到底写哪、能不能写」。
	storage.LogEffective()
	storage.RegisterMetrics()
	// 多节点共库时本地磁盘存储必然出错：对象只落在写入的那台机器上，
	// 其它节点读同一个 key 会 410。单机部署不受影响（默认 LD_CLUSTER_MODE=0）。
	//
	// 只在启动时判一次：运行期把 active 热切回 none 属于运维主动操作，
	// 由管理台的存储页与 LogEffective 负责回显，不在写路径上加闸
	// （那会让每一次上传都多一次配置解析，且把"配置错"变成"上传 503"）。
	if env.ClusterMode {
		backend, _, _, ok := storage.Effective()
		switch {
		case !ok:
			log.Fatal("集群模式已开启，但读不到存储配置（system_config.storage）：请检查数据库与配置行；单机部署请设 LD_CLUSTER_MODE=0")
		case backend == "none":
			log.Fatal("集群模式已开启，但写入位置仍是本地磁盘（storage.active=none）：多节点共库时对象只落在一台机器上，其它节点读取必然 410。" +
				"请先在管理台把存储切到已配置的 oss/cos/s3 并确认可写；单机部署请设 LD_CLUSTER_MODE=0")
		}
	}
	// 本地/测试环境配了公网 API 地址的预警（P1）：/f/ 直链 origin 会指向
	// 生产域名，上游回取素材必然失败——此前日志里没有任何线索，排障全靠猜。
	if w := siteurl.PublicBaseMisfitWarning(env.AppEnv); w != "" {
		log.Warn(w)
	}

	// 配置热更新订阅者（S10）：每次保存后记录变更键集；各消费方按需直接热读内存镜像。
	conf.System().Subscribe(func(changed []string) {
		log.Info("system_config 已热更新", zap.Int("keys", len(changed)), zap.Strings("changed", changed))
		// site 键变化时复查基址错配：本地开发在管理台临时填生产域名排障后忘改，
		// 是这条预警最常见的触发路径，光启动时查一次会漏掉。
		if w := siteurl.PublicBaseMisfitWarning(env.AppEnv); w != "" {
			for _, k := range changed {
				if k == "site" {
					log.Warn(w)
					break
				}
			}
		}
		// storage 变更后重打一次写入位置：这是"我刚改完，到底生效了没"的唯一回执。
		for _, k := range changed {
			if k == "storage" {
				storage.LogEffective()
				break
			}
		}
	})

	// 认证策略闸门：必须在开始服务之前。
	//
	// 带着矛盾配置启动的表现是「注册永远 400 / 所有人都登不上」，而日志里
	// 没有任何线索 —— 与其这样，不如直接起不来。检查项：认证配置键齐全、
	// 会话 TTL 自洽、注册要求联系方式验证时渠道**真的能发信**（传 verify.Ready，
	// 而不是只看 enabled 布尔：开关亮着但 SMTP 参数为空时发信必然失败）。
	//
	// 纯 API 节点没有注册与登录入口，这项校验只会带来误伤：SMTP 抖动一次就
	// 拒绝启动一台根本不发信的机器。跳过并留一行日志，避免"为什么没校验"要靠猜。
	if env.IsAPIMode() {
		log.Info("纯 API 节点：跳过注册/会话策略校验（本机没有注册与登录入口）")
	} else if err := authcfg.Validate(verify.Ready); err != nil {
		log.Fatal("认证策略配置不合法，拒绝启动", zap.Error(err))
	}

	// 强制管理员 2FA 的自锁预警。
	//
	// 刻意**不拒绝启动**（与最初方案相反）：库里一个已绑定的管理员都没有时，
	// 拒绝启动会让服务永远起不来 —— 而管理员重新绑定恰恰需要服务在跑
	// （绑定页走 /api/portal/auth/mfa/setup，不在 /admin/** 下）。
	// 所以这里的正确处置是「响一声」，让运维立刻看到，而不是把系统钉死。
	//
	// 纯 API 节点没有管理台，这条告警只会制造噪声（且触发一次多余的 DB 查询）。
	if !env.IsAPIMode() && authcfg.MFAPolicy() == authcfg.MFAPolicyRequiredForAdmin {
		var bound int64
		if err := gdb.Raw(`
			SELECT count(*) FROM users u
			JOIN user_mfa m ON m.uid = u.id AND m.enabled = true
			WHERE u.role = 0 AND u.status = 1 AND u.deleted_at IS NULL`).Scan(&bound).Error; err != nil {
			log.Warn("检查管理员二次验证绑定情况失败", zap.Error(err))
		} else if bound == 0 {
			log.Error("auth_mfa.policy=required_for_admin，但没有任何已绑定的在任管理员：管理台当前对所有人 403。" +
				"请让管理员登录后访问 /mfa/setup 完成绑定；若密钥已丢失，用 `lingdong-implant reset-mfa -username <名>` 重置")
		}
	}

	// 认证事件写入协程：异步批量落库，绝不阻断登录。
	authevent.Start()
	defer authevent.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Redis 健康周期重检（P2-18）：启动时 Ping 一次不够——启动时 Redis 未就绪会永久降级，
	// 运行中 Redis 挂掉则每个请求都去等一个死连接。30s 探测一次，成功即恢复。
	db.StartRedisHealthProbe(ctx, 30*time.Second)

	// 多实例配置一致（P1-8）：pub/sub 实时同步 + 30s 轮询兜底。
	// InvalidateLocalCache 是**不带广播**的版本，避免两个实例互相触发无限循环。
	configsync.SetPricingInvalidator(pricing.InvalidateLocalCache)
	configsync.Start(ctx)

	// 规格缓存巡检（多副本兜底）：pub/sub 丢消息时，注册表里的旧规格实例会一直留着。
	// 定期把本机规格缓存全清，让下一次请求重新解析（代价：每个在用规格每分钟一次读库）。
	specsvc.Watch(ctx, time.Minute)

	mgr := worker.NewManager(worker.Jobs())
	mgr.Start(ctx)

	// 只设读头部与空闲超时，**绝不设 ReadTimeout / WriteTimeout**：
	// 同步图像桥接要为一个请求等上游 780s（gpt-image 实测 540s），
	// 任何整体写超时都会把正在出图的请求半路切断 —— 那正是本次故障的另一半成因。
	//
	// ReadHeaderTimeout 必须有：compose 把 8080 直接映射到宿主机，
	// 绕过 nginx 时没有任何东西挡 Slowloris（慢速发头部占满 goroutine）。
	srv := &http.Server{
		Addr:              env.ListenAddr,
		Handler:           router.New(),
		ReadHeaderTimeout: 15 * time.Second,
		// 读超时（含请求体）——此前刻意留空，理由是"怕掐断 SSE"，
		// 但这个理由不成立：Go 的 ReadTimeout 只是**读**截止时间，
		// 不影响 handler 往外写的时长，SSE 的长连接写多久都不会被它打断。
		//
		// 真机压测（F7）确认了代价：声明 Content-Length: 4096 却只发 10 字节，
		// 连接被保持 25s 以上没有任何响应 —— 无需认证就能用慢 body 占满连接数。
		// 默认 120s：正常请求（含大文件上传）远用不到，慢速攻击最多占 2 分钟。
		// LD_HTTP_READ_TIMEOUT=0 可关掉（例如排查长轮询问题时）。
		ReadTimeout: httpReadTimeout(),
		IdleTimeout: 120 * time.Second,
		// 头部上限：Go 默认 1 MiB，等于「一个未认证请求就能让我们分配 1 MiB」。
		// 32 KiB 远超正常请求（Bearer + Cookie + UA 合计不到 4 KiB），
		// 又高于 nginx 默认的 large_client_header_buffers（8k×4），不会误杀。
		MaxHeaderBytes: 32 << 10,
	}
	go func() {
		log.Info("HTTP 服务启动", zap.String("addr", env.ListenAddr), zap.String("env", env.AppEnv))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("HTTP 服务异常退出", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Info("收到退出信号，开始优雅关闭")

	// 顺序不可反：**先停 HTTP 再停 Worker**。
	//
	// 反过来的话，Worker 已停（不再轮询/结算）而 HTTP 还在收新请求：
	// 新提交的任务只会被冻结、永远不会被推进，直到下一个实例接手
	// （任务轮询节拍 5s，但 leader 锁要等 TTL 60s 过期，最迟约 90s）。
	// 先停入口再排空后台，才是"不再产生新工作、把已有工作做完"。
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), httpShutdownBudget)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("HTTP 服务关闭失败", zap.Error(err))
	}
	// Worker 的退出等待有自己的上限（LD_TASK_DRAIN，默认 30s），不能被上面 30s 的
	// HTTP 超时 ctx 掐断，因此用独立的挂钟预算。
	mgr.Stop()
	// 预检失败记录是异步批量写的：HTTP 停了但队列里可能还有尾巴，
	// 这里排空一次再退出（最长 5s，失败只丢观测数据，不影响账务）。
	calllog.Flush()
	log.Info("已退出")
}

// prepareDocs 在监听之前把文档站准备好：能力回填 → 出厂种子（含 10 个模型族）→ 体检。
//
// 全部动作幂等：契约版本没变时不会改动任何已有行，第二次启动 0 新增 0 变更。
// 顺序不可换：EnsureCapabilities 必须早于播种（能力列是模型的事实来源），
// 而播种内部已经做完「逐篇体检、通过即发布」，所以体检只用于事后观测。
func prepareDocs(gdb *gorm.DB) {
	log := logger.L()

	// 只允许一个实例做这套重算：双副本同时冷启动时，两份同样的 INSERT 只会互相等锁。
	// 拿不到锁直接跳过 —— 这些动作幂等，另一个实例做完效果完全一样。
	release, ok, err := db.TryAdvisoryLock(gdb, db.DocsWarmLockID)
	if err != nil {
		log.Warn("文档准备：获取单实例锁失败，本机跳过", zap.Error(err))
		return
	}
	if !ok {
		log.Info("文档准备：另一实例正在执行，本机跳过")
		return
	}
	defer release()

	if n, err := docdocs.EnsureCapabilities(); err != nil {
		log.Warn("补齐模型能力失败（族页的能力描述会缺失）", zap.Error(err))
	} else if n > 0 {
		log.Info("已按事实补齐模型能力", zap.Int("rows", n))
	}
	if rep, err := docdocs.SeedDocs(); err != nil {
		log.Error("写入文档出厂种子失败（文档站会缺页）", zap.Error(err))
	} else {
		log.Info("文档出厂种子已就绪（只补缺页，已存在的内容不改）",
			zap.Int("new_sections", rep.Sections),
			zap.Int("new_nodes", rep.Nodes), zap.Int("new_endpoints", rep.Endpoints),
			zap.Int("fields", rep.Fields), zap.Int("skipped", rep.Skipped),
			zap.Int("damaged_drafts", len(rep.Damaged)))
	}
	// 全站体检：0 阻断也打一行 —— 否则「全绿」与「没跑」在日志上无法区分。
	probs := docdocs.CheckAll()
	blocking := 0
	for _, p := range probs {
		if p.Blocking {
			blocking++
		}
	}
	if blocking == 0 {
		log.Info("文档体检通过", zap.Int("problems", len(probs)))
	} else {
		log.Error("文档体检有阻断项（这些条目现在不可发布/不可见）",
			zap.Int("blocking", blocking), zap.Int("problems", len(probs)))
		for i, p := range probs {
			if !p.Blocking {
				continue
			}
			if i >= 10 {
				break
			}
			log.Error("文档体检", zap.String("code", p.Code),
				zap.String("field", p.Field), zap.String("msg", p.Message))
		}
	}

	// 卡在 applying 的提案：进程崩溃会让它永久不可操作，启动时落成失败终态。
	if n := agent.RecoverApplying(context.Background(), 5*time.Minute); n > 0 {
		log.Warn("有提案卡在应用中，已置为失败（可重新生成）", zap.Int64("count", n))
	}
}
