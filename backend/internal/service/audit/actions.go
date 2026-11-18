package audit

import (
	"errors"
	"fmt"
	"strings"
)

// 本文件是 admin_audit.action 的**唯一权威枚举**（v2.0 §4.9.8）。
// 写审计的调用方必须使用本文件的常量；未登记的字面量一律拒绝入库。

// ErrUnknownAction 未登记的审计动作。
var ErrUnknownAction = errors.New("audit: 未登记的审计动作")

const (
	ActionUserUpdate      = "user.update"
	ActionUserQuotaAdjust = "user.quota.adjust"
	ActionUserStatus      = "user.status"
	ActionUserRole        = "user.role"
	// ActionUserPasswordReset 记录管理员重置他人密码。
	// detail 只记 target_uid，**绝不记密码**（admin_audit 对所有只读管理员可见）。
	ActionUserPasswordReset = "user.password_reset"

	ActionChannelCreate      = "channel.create"
	ActionChannelUpdate      = "channel.update"
	ActionChannelDelete      = "channel.delete"
	ActionChannelTest        = "channel.test"
	ActionChannelBatchStatus = "channel.batch_status"

	ActionModelCreate         = "model.create"
	ActionModelUpdate         = "model.update"
	ActionModelDelete         = "model.delete"
	ActionModelBatchStatus    = "model.batch_status"
	ActionModelRatioUpdate    = "model.ratio.update"
	ActionModelBindingsUpdate = "model.bindings.update"

	// 接入应用（对外 API 的归属主体）。status 单独一个动作：
	// 「停用应用」会让所有新接入被拒，属于运营开关，与改名分开审计更易追责。
	ActionAppCreate = "app.create"
	ActionAppUpdate = "app.update"
	ActionAppStatus = "app.status"
	// 应用级后台：品牌、公告、版本、维护。
	//
	// 为什么拆这么细：应用后台的动作会直接影响**终端用户看到什么**，
	// 与"改个后台名字"不是一个追责等级。尤其是 app.maintenance ——
	// 一次误开就是全应用 503，必须能单独筛出来。
	ActionAppBrandUpdate   = "app.brand.update"
	ActionAppNoticeCreate  = "app.notice.create"
	ActionAppNoticeUpdate  = "app.notice.update"
	ActionAppNoticeDelete  = "app.notice.delete"
	ActionAppReleaseCreate = "app.release.create"
	ActionAppReleaseUpdate = "app.release.update"
	ActionAppReleaseStatus = "app.release.status"
	ActionAppReleaseDelete = "app.release.delete"
	ActionAppMaintenance   = "app.maintenance"

	// 应用插件（声明式）。
	//
	// 按对象分名：目录导入 / 安装 / 配置 / 启停 / 卸载 / 清除数据 / 记录增删改各一个。
	// 「清除数据」必须单独一个动作：它是**不可逆**的物理删除，与"卸载"（保留数据）
	// 不是同一个追责等级；混在 uninstall 里会让事后分不清"只是摘掉"还是"已经删了"。
	ActionAppPluginImport       = "app.plugin.import"
	ActionAppPluginDelete       = "app.plugin.delete"
	ActionAppPluginInstall      = "app.plugin.install"
	ActionAppPluginConfig       = "app.plugin.config"
	ActionAppPluginStatus       = "app.plugin.status"
	ActionAppPluginUninstall    = "app.plugin.uninstall"
	ActionAppPluginPurge        = "app.plugin.purge"
	ActionAppPluginRecordCreate = "app.plugin.record.create"
	ActionAppPluginRecordUpdate = "app.plugin.record.update"
	ActionAppPluginRecordDelete = "app.plugin.record.delete"
	// ActionAppPluginPublic 是"把插件数据开成免凭证可读"这个动作。
	// 单独记一笔而不是混进 status：它是唯一会改变**匿名可见范围**的操作，
	// 事后追查"这条数据什么时候出去的"只能靠它。
	ActionAppPluginPublic = "app.plugin.public"

	ActionVendorCreate = "vendor.create"
	ActionVendorUpdate = "vendor.update"
	ActionVendorDelete = "vendor.delete"

	// ActionModelVendorProvenance 记录「系统把模型归属纠偏到原始厂商」这类数据迁移动作
	// （AdminID=0）。与 vendor.update 分开：后者是运营手动改分组，前者是代码改口径，
	// 混在一起会让「谁改的这个模型分组」查不清。
	ActionModelVendorProvenance = "model.vendor.provenance"

	ActionOrderRefund        = "order.refund"
	ActionOrderRefundConfirm = "order.refund_confirm"
	ActionOrderManualCredit  = "order.manual_credit"
	ActionOrderClose         = "order.close"

	// ActionAffiliateBackfill 记录「补发已入账订单漏发的返利」。
	// 它不改余额（只补凭据），但会让一批钱变成可转出，必须留痕。
	ActionAffiliateBackfill = "affiliate.backfill"

	ActionRedeemGenerate = "redeem.generate"
	ActionRedeemDelete   = "redeem.delete"

	// 充值套餐（后台配置）。update 的 detail 必带 before/after 的价格与赠送额度：
	// 改价是资金敏感操作，只记「谁改了哪个套餐」无法事后追责。
	ActionPackageCreate = "package.create"
	ActionPackageUpdate = "package.update"
	ActionPackageStatus = "package.status"
	ActionPackageDelete = "package.delete"

	ActionConfigUpdate  = "config.update"
	ActionConfigPay     = "config.pay"
	ActionConfigStorage = "config.storage"
	ActionConfigFiles   = "config.files"
	ActionConfigNotify  = "config.notify"
	ActionConfigAuth    = "config.auth"

	// L2 规格（adapter_specs）的增删改。
	//
	// 为什么单独登记：规格是「数据驱动的适配器代码」，改一份会立刻影响所有引用它的渠道，
	// 敏感度与 channel.* 同级。四个动作不合并：
	// create/update 改的是行为，status 只改"是否参与选路"，delete 是不可逆的（渠道随即失联）。
	ActionAdapterSpecCreate = "adapter_spec.create"
	ActionAdapterSpecUpdate = "adapter_spec.update"
	ActionAdapterSpecStatus = "adapter_spec.status"
	ActionAdapterSpecDelete = "adapter_spec.delete"

	// AI 编排：提案应用与回滚。
	//
	// 与其它动作分开登记的理由：这两个动作会**成批**改渠道/模型/规格，
	// 事后查「谁在什么时候批量改了什么」时必须能一眼筛出来；
	// 混在 channel.update 里就需要逐条翻 detail。
	ActionAgentApply          = "agent.apply"
	ActionAgentRollback       = "agent.rollback"
	ActionAgentProposalReject = "agent.proposal.reject"
	// 文档副本与运行参数：前者是喂给 AI 的素材（决定它看到什么事实），
	// 后者会改变"能跑几步、花多少钱"。两者都必须是可追溯的动作。
	ActionAgentDocCreate = "agent.doc.create"
	ActionAgentDocDelete = "agent.doc.delete"
	ActionAgentSettings  = "agent.settings"

	ActionKeyReveal = "key.reveal"

	// ActionKeyAdminStatus 是管理台对 KEY 的兜底启停（AdminKeyStatus）。
	// status=2 的托管密钥唯一来源就是管理员动作，本动作必须可审计到人。
	ActionKeyAdminStatus = "key.admin_status"

	// ActionIconUpload 是运营上传图标。
	//
	// 为什么要登记：图标会直接出现在**门户首屏与对外文档站**（对外可见），
	// 而它只有"上传"一个动作、没有对应的实体行可查（icons 表没有编辑面）。
	// 不登记就意味着「门户上那张奇怪的图是谁传的」永远查不出来。
	ActionIconUpload = "icon.upload"

	// r2 增补登记：任务域。
	ActionTaskCancel = "task.cancel"
	ActionTaskRetry  = "task.retry"
	// ActionTaskPricingResnapshot 记录「重试按当前价目重新快照」。
	//
	// 必须与 task.retry 分开：重试本身不改钱，重快照会改钱（预扣额变了）。
	// 混在同一个 action 里，事后查「哪次操作让用户多付了」就得逐条翻 detail。
	ActionTaskPricingResnapshot = "task.pricing_resnapshot"
	// ActionTaskPurge 记录「物理删除终态任务与产物」（B7）。
	//
	// 与 task.cancel 必须分开：取消只是改状态、钱全额退回、数据全在；
	// 物理删除会让任务行与产物一起消失，事后只能靠这条审计还原「删了什么」。
	ActionTaskPurge = "task.purge"
)

// 一键更新（部署动作）。
//
// 这是**唯一会替换线上二进制/镜像**的管理动作，所以请求与结果分开记：
//   - request 必须在提交那一刻就落库（哪怕进程随后被替换，也要能回答"谁批的"）；
//   - result 由读路径在观察到终态时补写（agent 不写审计表，避免出现第二份写入路径）。
const (
	ActionReleaseUpdateRequest = "release.update.request"
	ActionReleaseUpdateResult  = "release.update.result"
)

// API 文档站动作（对外文档的文案与结构管理）。
//
// 为什么逐动作登记而不是一个笼统的 docs.update：文档是对外承诺，
// 事后要能回答「谁把这段说明发布出去的」「谁把这个模型从这篇文档里拆走的」。
// 混成一个动作时这些问题的答案都在 detail 里，而 detail 不参与检索。
const (
	ActionDocsEndpointSave      = "docs.endpoint.save"
	ActionDocsEndpointPublish   = "docs.endpoint.publish"
	ActionDocsEndpointUnpublish = "docs.endpoint.unpublish"
	// v7 工作台的节点级动作（接口级动作沿用上面 endpoint.*）。
	// 节点会整体出现/消失（含它下面所有接口），所以「谁把这一族下线了」
	// 必须能单独查出来，不能混在 endpoint.publish 里。
	ActionDocsNodeCreate     = "docs.node.create"
	ActionDocsNodeSave       = "docs.node.save"
	ActionDocsNodeDelete     = "docs.node.delete"
	ActionDocsNodePublish    = "docs.node.publish"
	ActionDocsNodeUnpublish  = "docs.node.unpublish"
	ActionDocsEndpointCreate = "docs.endpoint.create"
	ActionDocsEndpointDelete = "docs.endpoint.delete"
	ActionDocsEndpointFields = "docs.endpoint.fields"
	ActionDocsSectionSave    = "docs.section.save"
)

// 门户自助动作（v2.0 §4.9.8 注：门户操作不写 admin_audit，仅登记枚举备查）。
const (
	ActionKeyCreate    = "key.create"
	ActionKeyUpdate    = "key.update"
	ActionKeyStatus    = "key.status"
	ActionKeyReset     = "key.reset"
	ActionKeyDelete    = "key.delete"
	ActionKeyPlaintext = "key.plaintext"
)

// AllowedActions 是允许写入 admin_audit 的动作全集（唯一权威表）。
var AllowedActions = []string{
	ActionUserUpdate, ActionUserQuotaAdjust, ActionUserStatus, ActionUserRole,
	ActionUserPasswordReset,

	ActionChannelCreate, ActionChannelUpdate, ActionChannelDelete, ActionChannelTest, ActionChannelBatchStatus,

	ActionModelCreate, ActionModelUpdate, ActionModelDelete, ActionModelBatchStatus,
	ActionModelRatioUpdate, ActionModelBindingsUpdate,

	ActionAppCreate, ActionAppUpdate, ActionAppStatus,
	ActionAppBrandUpdate,
	ActionAppNoticeCreate, ActionAppNoticeUpdate, ActionAppNoticeDelete,
	ActionAppReleaseCreate, ActionAppReleaseUpdate, ActionAppReleaseStatus, ActionAppReleaseDelete,
	ActionAppMaintenance,
	ActionAppPluginImport, ActionAppPluginDelete,
	ActionAppPluginInstall, ActionAppPluginConfig, ActionAppPluginStatus,
	ActionAppPluginUninstall, ActionAppPluginPurge,
	ActionAppPluginRecordCreate, ActionAppPluginRecordUpdate, ActionAppPluginRecordDelete,
	ActionAppPluginPublic,

	ActionVendorCreate, ActionVendorUpdate, ActionVendorDelete, ActionModelVendorProvenance,

	ActionOrderRefund, ActionOrderRefundConfirm, ActionOrderManualCredit, ActionOrderClose,
	ActionAffiliateBackfill,

	ActionRedeemGenerate, ActionRedeemDelete,

	ActionPackageCreate, ActionPackageUpdate, ActionPackageStatus, ActionPackageDelete,

	ActionConfigUpdate, ActionConfigPay, ActionConfigStorage, ActionConfigFiles, ActionConfigNotify,
	ActionReleaseUpdateRequest, ActionReleaseUpdateResult,
	// 认证策略（会话 TTL / 失败阈值 / 验证码 / 注册准入）单独一个子动作：
	// 它决定「谁能注册、爆破要多久、会话活多久」，与支付/存储配置不是一类风险。
	ActionConfigAuth,

	ActionAdapterSpecCreate, ActionAdapterSpecUpdate, ActionAdapterSpecStatus, ActionAdapterSpecDelete,

	ActionAgentApply, ActionAgentRollback, ActionAgentProposalReject,
	ActionAgentDocCreate, ActionAgentDocDelete, ActionAgentSettings,

	ActionKeyReveal,

	ActionKeyAdminStatus,

	ActionIconUpload,

	ActionTaskCancel, ActionTaskRetry, ActionTaskPricingResnapshot,
	ActionTaskPurge,

	// 文档站（v7）：接口页三个 + 节点/板块五个 + 字段与新建删除四个。
	// **每一个都必须在这里**：漏登不是"少记一条审计"，而是 Normalize 返回
	// ErrUnknownAction，调用点把它吞成一条 ERROR 日志 —— 后台看板里
	// 一条文档审计都没有，页面却已经被改了。
	ActionDocsEndpointSave, ActionDocsEndpointPublish, ActionDocsEndpointUnpublish,
	ActionDocsNodeCreate, ActionDocsNodeSave, ActionDocsNodeDelete,
	ActionDocsNodePublish, ActionDocsNodeUnpublish,
	ActionDocsEndpointCreate, ActionDocsEndpointDelete, ActionDocsEndpointFields,
	ActionDocsSectionSave,
}

// PortalActions 是门户自助动作（不写 admin_audit）。
var PortalActions = []string{
	ActionKeyCreate, ActionKeyUpdate, ActionKeyStatus, ActionKeyReset, ActionKeyDelete, ActionKeyPlaintext,
}

// Valid 校验 action 是否为合法的管理审计动作（唯一校验函数）。
func Valid(action string) bool {
	for _, a := range AllowedActions {
		if a == action {
			return true
		}
	}
	return false
}

// Normalize 归一动作字面量：权威动作原样返回（去首尾空白），其余一律拒绝（无别名兼容层）。
func Normalize(action string) (string, error) {
	a := strings.TrimSpace(action)
	if !Valid(a) {
		return "", fmt.Errorf("%w: %q", ErrUnknownAction, action)
	}
	return a, nil
}
