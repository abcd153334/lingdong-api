package middleware

import (
	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
)

// portalOpen 是「门户端是否开启」的读取入口（唯一实现在 conf.PortalEnabled，
// 会话闸与发券闸共用同一判据与同一默认值）。
func portalOpen() bool { return conf.PortalEnabled() }

// PortalOpen 是「门户站点入口」闸门，**只**挂在门户注册的两条路由上。
//
// 为什么必须是中间件而不是写进 handler：controller.RegisterContactCode 同时服务
// /api/user/register/contact-code —— 那是对外 API 面，只受 auth_register.mode 管辖，
// 写进 handler 会把两个面一起打死（而且错误码会变成 portal_disabled，看不出真因）。
//
// 注册的真假仍然只有 auth_register.mode 一个判据；这里关的是「门户这条入口」。
func PortalOpen() gin.HandlerFunc {
	return func(c *gin.Context) {
		if portalOpen() {
			c.Next()
			return
		}
		response.FailDef(c, errs.PortalDisabled)
		c.Abort()
	}
}
