package controller

// 插件数据的**唯一免凭证入口**：GET /api/apps/{slug}/p/{plugin}/{entity}。
//
// 为什么单独一条而不是塞进 /api/apps/:slug/profile：那条是"终端启动自举"用的，
// 变更频率与缓存档完全不同（profile 60s、这里 30s），合成一条只能取最严的那个。
//
// 五道闸与字段投影都在 service/appplugins/public.go，这里只做三件事：
// 认 slug、解析分页、按契约里的 max-age 出缓存头。

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/apicontract"
	"lingdong-api/internal/db"
	"lingdong-api/internal/service/appplugins"
)

// AppPluginPublic 处理 GET /api/apps/{slug}/p/{plugin}/{entity}。
func AppPluginPublic(c *gin.Context) {
	a, ok := publicAppOf(c)
	if !ok {
		return
	}
	// 非法分页值按默认处理而不是 400：这是只读端点，调用方多半是照文档拼的，
	// 为 page=abc 回一个错误体只会让对方在终端侧多写一遍容错。
	page, _ := strconv.Atoi(strings.TrimSpace(c.Query("page")))
	size, _ := strconv.Atoi(strings.TrimSpace(c.Query("page_size")))
	data, err := appplugins.PublicList(c.Request.Context(), db.DB, a.ID,
		c.Param("plugin"), c.Param("entity"), page, size)
	if err != nil {
		// 所有"不对外"的原因都收敛成 404 plugin_not_found / plugin_entity_not_found，
		// 区分它们等于给匿名方一份枚举面（见 public.go 的包注释）。
		respondPluginErr(c, err)
		return
	}
	respondCached(c, data, apicontract.MustPublicPolicy("apps.plugin_records").MaxAge)
}
