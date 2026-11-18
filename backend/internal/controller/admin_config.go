package controller

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/netguard"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/service/notify"
	"lingdong-api/internal/service/payment"
	"lingdong-api/internal/service/storage"
	"lingdong-api/internal/service/systemconfig"
)

// AdminConfigGet 处理 GET /api/admin/config：返回全量配置（敏感字段以 Mask 脱敏回显）。
//
// 同时下发四样「派生数据」（**必须放在 config 外层**）：
//   - pay_channel_schemas：各协议的参数声明（键/标签/分组/必填/密文），
//     前端据此渲染卡片表单，不再遍历 params 里存了哪些键；
//   - pay_channel_templates：新增渠道时的空白参数模板（按协议），仅为兼容旧前端保留，
//     新前端以 pay_channel_schemas 为准（下个版本可删）；
//   - notify_base：回调地址前缀，配合渠道 ID 得到 `{notify_base}{id}/notify`；
//   - pay_method_options：可选支付方式（后端单点，前端不维护映射表）。
//
// 放外层的原因：前端把 `config` 当 27 键全量包原样提交，塞进 config 会多写一个
// `Keys` 白名单外的键，保存直接报错。
func AdminConfigGet(c *gin.Context) {
	// 回调地址前缀 = 第一条 API 地址 + /api/pay/（渠道卡片再拼 `{id}/notify`）。
	// 未配置时留空，前端据此提示「去站点设置里填 API 调用地址」。
	notifyBase := ""
	if base := siteurl.ConfiguredBase(); base != "" {
		notifyBase = base + "/api/pay/"
	}
	// 参数模板由 schema 派生：硬编码协议列表会变成第三个漂移点
	// （Go schema / 前端 FALLBACK / 这里），新增协议时必漏一处。
	templates := make(map[string]payment.ChannelParams, len(payment.Schemas()))
	for _, sch := range payment.Schemas() {
		p := payment.ParseProtocol(sch.Protocol)
		if !p.Valid() {
			continue
		}
		templates[sch.Protocol] = payment.DefaultParams(p)
	}

	response.OK(c, gin.H{
		"config":                systemconfig.Read(),
		"mask":                  systemconfig.Mask,
		"pay_channel_schemas":   payment.Schemas(),
		"pay_channel_templates": templates,
		"notify_base":           notifyBase,
		"orphan_groups":         systemconfig.OrphanGroups(c.Request.Context(), db.DB),
		// 云存储渠道 schema：与 pay_channel_schemas 同理，前端不维护第二份字段列表。
		"storage_schemes":   systemconfig.StorageSchemes(),
		"storage_effective": storageEffective(),
		"pay_method_options": []gin.H{
			{"value": string(payment.MethodWechat), "label": payment.MethodWechat.Label()},
			{"value": string(payment.MethodAlipay), "label": payment.MethodAlipay.Label()},
			{"value": string(payment.MethodCard), "label": payment.MethodCard.Label()},
		},
	})
}

// storageEffective 回显「现在到底写哪」：写路径的唯一判据是 storage.active。
//
// 放在响应**外层**而不是 config 里：config 会被前端原样 PUT 回来，
// 多一个字段就会撞配置白名单（400 未知键）。
//
// 未配置时 writable=false，管理台据此显示红条 —— 这是「配置丢了但服务还在跑」
// 的唯一可见信号（此时上传已经在 503）。
func storageEffective() gin.H {
	backend, bucket, endpoint, ok := storage.Effective()
	label := ""
	if ok {
		label = systemconfig.SchemeLabel(backend)
	}
	return gin.H{
		"active": backend, "label": label, "bucket": bucket,
		"endpoint": endpoint, "writable": ok,
	}
}

// AdminConfigUpdate 处理 PUT /api/admin/config：全量保存（键 → 值 的扁平映射），保存即热生效。
func AdminConfigUpdate(c *gin.Context) {
	var body map[string]json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil || len(body) == 0 {
		response.FailDef(c, errs.InvalidParam.WithMessage("请求体须为「配置键 → 值」的 JSON 对象"))
		return
	}
	if err := systemconfig.Update(c.Request.Context(), db.DB, c.GetUint64("user_id"), body); err != nil {
		configFail(c, err)
		return
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	response.OKMsg(c, "配置已保存并即时生效", gin.H{"keys": keys})
}

// AdminStorageTest 处理 POST /api/admin/storage/test：对存储通道做一次读写探针。
//
// 两种目标：
//   - 不带请求体（或 active 为空）：测**当前已生效**的配置；
//   - 带 `{active, channels}`：测**草稿**配置，不落库、不热更新。
//
// 草稿探针是必需的：保存即热生效，凭证填错会让在跑的产物转存全部失败（逐单退款 +
// 上游成本白付），而配置页只显示「保存成功」。先验后存把试错成本降到零。
//
// 失败时**不回显底层错误原文**：S3/OSS 的 SDK 错误常带 bucket、endpoint、
// access key id 片段与签名原文，而这些会显示在前端、进截图、进浏览器日志。
// 细节只写服务端日志，前端拿到的是一条可操作的短句。
func AdminStorageTest(c *gin.Context) {
	var body struct {
		Active   string                            `json:"active"`
		Channels map[string]map[string]interface{} `json:"channels"`
	}
	// 空请求体是合法输入，绑定失败不当作错误。
	_ = c.ShouldBindJSON(&body)

	backend := strings.TrimSpace(body.Active)
	var ch map[string]interface{}
	if backend == "" {
		cur, err := storage.Active()
		if err != nil {
			logger.L().Warn("存储探针：当前没有激活的存储后端", zap.Error(err))
			response.OK(c, gin.H{"ok": false, "backend": "none", "message": "尚未启用或未正确配置存储后端（详见服务端日志）"})
			return
		}
		backend = cur.Name()
		// 测当前生效配置必须回读通道参数：给 nil 去构建会一律报「缺少必填」，
		// 而那个后端其实一直工作正常。
		ch = storage.ActiveChannel(backend)
	} else {
		ch = body.Channels[backend]
	}

	name, err := storage.ProbeChannel(c.Request.Context(), backend, ch)
	if err != nil {
		logger.L().Warn("存储探针失败", zap.String("backend", name), zap.Error(err))
		response.OK(c, gin.H{"ok": false, "backend": name, "message": probeMessage(err)})
		return
	}
	response.OK(c, gin.H{"ok": true, "backend": name, "message": "通道连通（读写探针通过）"})
}

// probeMessage 把探针失败归成一句可操作的中文（细节留在服务端日志）。
func probeMessage(err error) string {
	switch {
	case errors.Is(err, netguard.ErrBlocked):
		return "endpoint 指向内网或环回地址，已被安全策略拒绝"
	case errors.Is(err, storage.ErrBackendGone):
		return "配置不完整或参数非法：请检查 endpoint / bucket / 密钥（详见服务端日志）"
	case strings.Contains(err.Error(), "删除失败"):
		return "对象可写但不可删，请检查密钥策略是否含删除权限"
	default:
		return "写入失败：请检查 endpoint / bucket / 密钥权限（详见服务端日志）"
	}
}

// AdminNotifyTest 处理 POST /api/admin/notify/test：按当前通道配置发送一封测试通知。
func AdminNotifyTest(c *gin.Context) {
	var body struct {
		Channel string `json:"channel"`
		Target  string `json:"target"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	target := strings.TrimSpace(body.Target)
	switch body.Channel {
	case "email":
		if !strings.Contains(target, "@") {
			response.FailDef(c, errs.InvalidParam.WithMessage("测试邮箱格式不正确"))
			return
		}
		if err := notify.SendTestEmail(c.Request.Context(), target); err != nil {
			configFail(c, err)
			return
		}
	case "sms":
		if target == "" {
			response.FailDef(c, errs.InvalidParam.WithMessage("测试手机号不能为空"))
			return
		}
		if err := notify.SendTestSMS(c.Request.Context(), target); err != nil {
			configFail(c, err)
			return
		}
	default:
		response.FailDef(c, errs.InvalidParam.WithMessage("通知通道仅支持 email / sms"))
		return
	}
	response.OKMsg(c, "测试通知已发送", gin.H{"channel": body.Channel, "target": target})
}

// configFail 优先还原服务层校验错误定义（含细节 message），未知错误统一 500。
func configFail(c *gin.Context, err error) {
	if def, ok := errs.AsDef(err); ok {
		response.FailDef(c, def)
		return
	}
	response.FailDef(c, errs.Internal)
}
