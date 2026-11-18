package controller

import (
	"encoding/base64"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/captcha"
)

// Captcha 处理 GET /api/portal/auth/captcha?scene=login。
//
// 一次响应同时给出**图片 id 与图片本身**（dataURL）：
//
//	{ "id": "…32 位 hex…", "image": "data:image/png;base64,…" }
//
// 为什么不像传统做法那样 `GET /captcha` 直出 PNG + 把 id 写 Cookie：
//   - `<img src>` 读不到响应头，id 只能落到 Cookie（单槽）；
//   - 多标签页、同页两个表单、乃至 React 的任意一次重渲染都会让 id 与图错位，
//     用户"照着图填"却永远错——这是上一版真实发生过的事故。
//
// 现在 id 与图在同一次响应里，天然配对：不需要 Cookie，也不存在竞态。
//
// scene 只用于合法性校验（策略由服务端在提交时按场景裁决）。
func Captcha(c *gin.Context) {
	switch c.Query("scene") {
	case captcha.SceneRegister, captcha.SceneLogin, captcha.ScenePasswordReset:
	default:
		response.FailDef(c, errs.InvalidParam)
		return
	}

	id, png, err := captcha.New(c.Request.Context())
	if err != nil {
		respondErr(c, err)
		return
	}
	// 挑战必须每次现取：缓存住会让同一张图被多个人拿到同一份答案。
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	response.OK(c, gin.H{
		"id":    id,
		"image": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
	})
}
