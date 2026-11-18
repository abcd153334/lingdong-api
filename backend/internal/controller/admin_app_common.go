package controller

// 本文件是「应用级后台」各 handler 的公共部分：取应用、派生图标 URL、分页。
//
// 统一在这里做而不是各文件各写一份：`appOf` 的错误映射一旦分叉，
// 就会出现"品牌页 404、公告页 500"这种同一种失败不同表现的问题。

import (
	"errors"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	appsvc "lingdong-api/internal/service/app"
	"lingdong-api/internal/service/iconref"
)

// appOf 解析路径里的 :id 并加载应用；失败时已写出响应。
func appOf(c *gin.Context) (*model.App, bool) {
	id, ok := uintParam(c, "id")
	if !ok {
		// 路径参数不是数字属于**参数非法**，不是"应用不存在"：
		// 合成 404 会让前端把"我拼错了 URL"读成"应用被删了"。
		response.FailDef(c, errs.InvalidParam)
		return nil, false
	}
	a, err := appsvc.Get(c.Request.Context(), db.DB, id)
	if err != nil {
		if errors.Is(err, appsvc.ErrNotFound) {
			response.FailDef(c, errs.AppNotFound)
			return nil, false
		}
		response.FailDef(c, errs.Internal)
		return nil, false
	}
	return a, true
}

// logoURLOf 把图标令牌派生成可直接渲染的绝对地址。
//
// 存令牌而不是 URL：换存储后端/换域名时令牌不变，URL 由 iconref 按当前站点
// 基址现算（与模型/渠道图标同一套）。
func logoURLOf(c *gin.Context, a *model.App) string {
	return iconref.URL(siteurl.Base(c), a.LogoIcon)
}

// failAppErr 把 app 服务的领域错误映射成对外错误码，未识别的兜底 500。
func failAppErr(c *gin.Context, err error) {
	response.FailDef(c, appsvc.DefOr(err, errs.Internal))
}

// maxRunes 按**字符**计长（与 service/app 的同名函数同一口径）。
//
// 用 len() 会让"64 个汉字"被当成 192 字节而误判超长，与 PG varchar(n)
// 按字符计数的语义也不一致。
func maxRunes(s string, n int) bool { return utf8.RuneCountInString(s) <= n }

// insertedID 取 map 插入后 RETURNING 回填的自增 id。
//
// 不用裸断言：驱动在个别路径下会把整数扫成 int64，裸断言直接 panic。
func insertedID(row map[string]any) uint64 {
	switch v := row["id"].(type) {
	case uint64:
		return v
	case int64:
		return uint64(v)
	case int:
		return uint64(v)
	default:
		return 0
	}
}

// rfc3339 统一管理台的时间输出格式（UTC）。
//
// 无时区串（"2006-01-02 15:04:05"）会被浏览器按**本地时区**解析，
// 服务器与浏览器不同区时整体偏移；RFC3339 带 Z，解析结果唯一。
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// rfc3339OrEmpty 是 rfc3339 的指针版，**容忍 nil**。
//
// 列表页对每一行都调用它：`status=2 但 published_at 为空` 这种脏数据
// （导入/人工修库）直接用 *time.Time 调 Format 会 panic 掉整个响应。
func rfc3339OrEmpty(t *time.Time) string {
	if t == nil {
		return ""
	}
	return rfc3339(*t)
}
