package controller

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/sorting"
	"lingdong-api/internal/pkg/uploadcfg"
	"lingdong-api/internal/service/upload"
)

// fileSortColumns 是文件列表的排序白名单：**对外字段名 → SQL 列**。
//
// `size` 对外是"字节数"，SQL 列是 `size_bytes`：按对外名拼 SQL 会报列不存在（500），
// 接入方看不出是自己传错了字段。键集合与 apicontract `files.list` 的 Sortable 一致。
var fileSortColumns = map[string]string{
	"created_at": "created_at",
	"size":       "size_bytes",
}

// PortalFileList 处理 GET /api/portal/files：当前用户全量文件（跨全部 KEY，含 KEY 名称，T21 分页）。
func PortalFileList(c *gin.Context) {
	uid := c.GetUint64("user_id")
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	keyID, ok := optionalUintQuery(c, "key_id")
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	sortOrd, ok := sorting.Query(c, fileSortColumns)
	if !ok {
		return
	}
	// 托管密钥只能看到本 KEY 传的文件：查询参数 key_id 是给门户用的筛选器，
	// 对托管密钥必须**覆盖**而不是"作为默认值"。
	if kid, _, hosted := keyScopeOf(c); hosted {
		keyID = kid
	}
	items, total, err := upload.List(c.Request.Context(), upload.ListFilter{
		UserID: uid, KeyID: keyID, Offset: p.Offset(), Limit: p.Limit(),
		OrderBy: sortOrd.ClauseOr("created_at DESC, id DESC"),
	}, siteBase(c))
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, pagination.New(items, total, p))
}

// PortalFileDelete 处理 DELETE /api/portal/files/{id}：本人名下跨 KEY 可删，成功 204。
func PortalFileDelete(c *gin.Context) {
	scope := upload.Scope{UserID: c.GetUint64("user_id")}
	if kid, _, hosted := keyScopeOf(c); hosted {
		scope.KeyID = &kid
	}
	if err := upload.Delete(c.Request.Context(), strings.TrimSpace(c.Param("id")), scope); err != nil {
		response.FailDef(c, uploadFailDefOf(err))
		return
	}
	c.Status(204)
}

// PortalFileUpload 处理 POST /api/portal/files：门户直传（会话身份，无需 API KEY）。
//
// 与 POST /v1/files 的分工：**同一套 service，不同的凭证面**。
// 门户没有 KEY 上下文，日限额的计数主体因此落到账号维度（upload.QuotaSubject）——
// 这正是「门户上传不许填 API KEY」的产品决定：让用户为了传一张图先去造一把 KEY，
// 等于把密钥管理成本转嫁给最不需要它的那类用户。
//
// 四重限额（总量/日限/容量/并发）、类型与魔数校验、24h 有效期全部复用 upload.Create，
// 不在这里另写一份 —— 两条上传路径的规则一旦分叉，就会出现「门户能传、API 传不了」
// 这种无法解释的现象。
func PortalFileUpload(c *gin.Context) {
	uid := c.GetUint64("user_id")
	if uid == 0 {
		response.FailDef(c, errs.Unauthorized)
		return
	}
	// 与 /v1/files 同一个门（uploadcfg 推导），门户与 API 不再有两套体积口径。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, uploadcfg.RequestCapCached())
	fh, err := c.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			response.FailDef(c, errs.FileTooLarge)
			return
		}
		response.FailDef(c, errs.FileMissing)
		return
	}
	f, err := fh.Open()
	if err != nil {
		response.FailDef(c, errs.FileMissing)
		return
	}
	defer f.Close()

	in := upload.CreateInput{UserID: uid, Filename: fh.Filename, Size: fh.Size, Reader: f}
	var keyName string
	if kid, _, hosted := keyScopeOf(c); hosted {
		// 托管密钥仍然按 KEY 收窄：一把托管 KEY 的持有者不该看到（或写入）同账号其它 KEY 的文件。
		in.KeyID = kid
		if v, ok := c.Get("api_key_record"); ok {
			if k, _ := v.(*model.APIKey); k != nil {
				keyName = k.Name
			}
		}
	}
	rec, err := upload.Create(c.Request.Context(), in)
	if err != nil {
		if errors.Is(err, upload.ErrConcurrency) {
			c.Header("Retry-After", "1")
		}
		if errors.Is(err, upload.ErrDailyLimit) {
			c.Header("Retry-After", "60")
		}
		response.FailDef(c, uploadFailDefOf(err))
		return
	}
	// 门户成功一律 200 + code=0（与全部控制台端点同一形状）：
	// /v1/files 回 201 是 OpenAI Files 协议规定，门户不属于网关域，
	// 在这里破例会让「成功也可能是非 200」变成前端必须到处判断的第二条约定。
	response.OK(c, upload.View(rec, keyName, siteBase(c)))
}

// PortalFileConfig 处理 GET /api/portal/files/config：门户所需的有效限额与账号用量（只读）。
func PortalFileConfig(c *gin.Context) {
	s := uploadcfg.Current()
	stats, err := upload.StatsOf(c.Request.Context(), c.GetUint64("user_id"))
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, gin.H{
		"enabled":             s.Enabled,
		"daily_limit_per_key": s.DailyLimitPerKey, "retention_hours": s.RetentionHours,
		"user_total_mb": s.UserTotalMB, "concurrency": s.Concurrency,
		"today_count": stats.TodayCount, "total_bytes": stats.TotalBytes,
		// types 把「后缀 → 类别 → 单文件上限」一次给全：前端据此渲染 accept、
		// 取每个文件的体积上限，不再自己维护映射表（那必然与服务端漂移）。
		"types":  s.TypeList(),
		"limits": s.Limits,
		// quota_scope 说明「今日上传次数」是按谁计的。
		//
		// 门户没有 KEY，计数主体是账号；而限额数值仍是同一个 daily_limit_per_key。
		// 不下发这一项，前端只能写死「本 KEY」—— 门户里根本没有 KEY，
		// 那句文案会让用户以为自己在用某把不存在的 KEY。
		"quota_scope": "account",
	})
}
