package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/response"
)

// apiSurfacePrefixes 是「纯 API 节点」保留的路径前缀（唯一清单）。
//
// 每一项都必须有明确理由，否则就是在给纯 API 节点开一个不需要的口子：
//   - /v1/            网关本体；/v1/media/{token} 是给 <img src> 用的能力 URL
//   - /v1beta/        Gemini 协议面（google-genai / LangChain-ChatGoogle 直连）
//   - /dashboard/     OpenAI 老客户端按老习惯直连的 billing 口
//   - /api/user/      应用自助面（托管密钥、额度查询、应用用户登录），Bearer 鉴权
//   - /api/apps/      应用的公开面（品牌 / 公告 / 版本三条，各自独立缓存策略）。
//     终端在用户登录之前就要展示品牌与"检查更新"，纯 API 节点也必须提供；
//     返回体是白名单，不含任何经营数据
//   - /api/pay/       支付回调（回调固定主站，但渠道重试/DNS 变更可能落到任意节点）
//   - /api/icons/     /v1/models 返回的 icon_url 指向这里，而 <img> 带不了凭证
//   - /f/             历史上传直链（本地后端产物；云存储下通常 302 到桶）
//   - /api/diag/hold  长等待探针；本身要求口令，未配置时返回 404
var apiSurfacePrefixes = []string{
	"/v1/",
	"/v1beta/",
	// /suno/ 是 new-api 的 SUNO 协议面（顶层路径）。纯 API 节点必须提供它，
	// 否则按 new-api 写的客户端会拿到整块 404，而平台其实有这个能力。
	"/suno/",
	"/dashboard/",
	"/api/user/",
	"/api/apps/",
	"/api/pay/",
	"/api/icons/",
	"/f/",
	"/api/diag/hold",
}

// apiSurfaceExact 是必须**精确匹配**的路径（避免 /api/healthxyz 这类前缀逃逸）。
var apiSurfaceExact = map[string]bool{
	"/api/health": true,
	"/metrics":    true,
}

// APISurfaceAllows 判断某路径是否属于「纯 API 节点」的对外面。
//
// 这是**允许表（fail-closed）**：没登记的一律拒绝。新增端点时必须显式决定
// 要不要在 API 节点暴露 —— router 的覆盖测试会强制这个决定，不决策就红灯。
func APISurfaceAllows(path string) bool {
	p := strings.TrimSpace(path)
	if p == "" {
		return false
	}
	if apiSurfaceExact[p] {
		return true
	}
	for _, prefix := range apiSurfacePrefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// APISurfaceOnly 把对外面裁剪成「纯 API」：未登记的路径直接 404。
//
// 为什么放在中间件而不是按模式条件注册路由：
//   - 路由表保持不变，apicontract / 文档同步 / 路由快照测试零改动；
//   - fail-closed：新增路径默认不暴露，而条件注册默认暴露（漏一个 if 就是一个洞）。
//
// 必须挂在 CORS 之前：被拒绝的路径不该回任何跨域头，否则浏览器会把 404
// 读成"跨域失败"，把"这台机器不提供前端"误判成 CORS 配置问题。
func APISurfaceOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		if APISurfaceAllows(c.Request.URL.Path) {
			c.Next()
			return
		}
		// 404 而不是 403：403 等于确认"这里有个端点，只是不给你"。
		response.NotFound(c)
		c.Abort()
	}
}
