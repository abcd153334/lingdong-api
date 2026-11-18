package router_test

// 认证事件审计（/api/admin/auth-events）验收用例（方案 §2.13.3 + §5 条目 51）。
//
// 守三件事：
//  1. 筛选条件与列表/导出**共用同一份 WHERE**（列表能筛出来的，导出必须也能）；
//  2. 元数据（事件选项 + 保留天数）不依赖库里已有数据 —— 一次攻击没发生后
//     就没法按它筛，恰恰是最需要排查的时候；
//  3. 新端点确实挂在管理组下（普通用户 403、未登录 401），
//     否则「审计页」本身会变成一条未授权可读的登录流水。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/authevent"
)

// adminGET 发一个带管理台会话的 GET（GET 不需要 CSRF，但仍带 Origin 走同源校验）。
func adminGET(t *testing.T, r *gin.Engine, path string, sess *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://example.com"+path, nil)
	req.Header.Set("Origin", "http://example.com")
	if sess != nil {
		req.AddCookie(sess)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// seedAuthEvents 直接落库。
//
// 不走 authevent.Record：那是一条异步队列（Start 未调用时直接丢），
// 用它播种会让用例依赖 200ms 的 flush 节拍，偶发失败且难查。
func seedAuthEvents(t *testing.T, gdb *gorm.DB, rows ...model.AuthEvent) {
	t.Helper()
	for i := range rows {
		if rows[i].CreatedAt.IsZero() {
			rows[i].CreatedAt = time.Now()
		}
		if err := gdb.Create(&rows[i]).Error; err != nil {
			t.Fatalf("播种认证事件失败: %v", err)
		}
	}
}

// authEventPage 是分页响应体（只解用例关心的字段）。
type authEventPage struct {
	Items []struct {
		ID      uint64 `json:"id"`
		UID     uint64 `json:"uid"`
		Account string `json:"account"`
		Event   string `json:"event"`
		Label   string `json:"label"`
		Result  string `json:"result"`
		IP      string `json:"ip"`
	} `json:"items"`
	Total int64 `json:"total"`
}

func decodeEventPage(t *testing.T, rec *httptest.ResponseRecorder) authEventPage {
	t.Helper()
	var out authEventPage
	if err := json.Unmarshal(decodeEnv(t, rec).Data, &out); err != nil {
		t.Fatalf("解析认证事件分页失败: %v（%s）", err, rec.Body.String())
	}
	return out
}

// seedAdminWithEvents 建一个已提权的管理员并播四条事件，返回会话与 UID。
func seedAdminWithEvents(t *testing.T, gdb *gorm.DB, r *gin.Engine, name string) (*http.Cookie, uint64) {
	t.Helper()
	registerUser(t, r, name)
	var u model.User
	if err := gdb.Where("username = ?", name).First(&u).Error; err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	if err := gdb.Model(&model.User{}).Where("id = ?", u.ID).Update("role", 0).Error; err != nil {
		t.Fatalf("提权失败: %v", err)
	}
	sess, _, _ := loginAs(t, r, name, "UA/admin", nil)

	base := time.Now().Add(-10 * time.Minute)
	seedAuthEvents(t, gdb,
		model.AuthEvent{UID: u.ID, Account: name, Event: authevent.EventLogin, Result: authevent.ResultSuccess, IP: "1.1.1.1", CreatedAt: base},
		model.AuthEvent{UID: 0, Account: "ghost", Event: authevent.EventLoginFailed, Result: authevent.ResultFail, Reason: "invalid_credential", IP: "9.9.9.9", CreatedAt: base.Add(time.Minute)},
		model.AuthEvent{UID: u.ID, Account: name, Event: authevent.EventMFAEnable, Result: authevent.ResultSuccess, IP: "1.1.1.1", CreatedAt: base.Add(2 * time.Minute)},
		model.AuthEvent{UID: u.ID, Account: name, Event: authevent.EventBackupCodeUsed, Result: authevent.ResultSuccess, IP: "2.2.2.2", CreatedAt: base.Add(3 * time.Minute)},
	)
	return sess, u.ID
}

func TestAdminAuthEventsListAndFilters(t *testing.T) {
	gdb, r := authTestEnv(t)
	seedAuthConfig(t, "optional")
	sess, uid := seedAdminWithEvents(t, gdb, r, "olive")

	// 全量：按 id 倒序（= 时间倒序），中文 label 由后端补，前端不再维护第二份映射。
	all := adminGET(t, r, "/api/admin/auth-events", sess)
	if all.Code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d %s", all.Code, all.Body.String())
	}
	page := decodeEventPage(t, all)
	if page.Total != 4 || len(page.Items) != 4 {
		t.Fatalf("应 4 条事件，实际 total=%d len=%d", page.Total, len(page.Items))
	}
	if page.Items[0].Event != authevent.EventBackupCodeUsed {
		t.Fatalf("应按 id 倒序（最新在前），实际首条 %s", page.Items[0].Event)
	}
	if page.Items[0].Label != "使用备份码" {
		t.Fatalf("label 必须由后端补中文名，实际 %q", page.Items[0].Label)
	}

	// 分页：page_size 生效且 total 不随之变化。
	trimmed := decodeEventPage(t, adminGET(t, r, "/api/admin/auth-events?page_size=2", sess))
	if trimmed.Total != 4 || len(trimmed.Items) != 2 {
		t.Fatalf("page_size=2 应返回 2 条但 total=4，实际 len=%d total=%d", len(trimmed.Items), trimmed.Total)
	}

	cases := []struct {
		name  string
		query string
		want  int64
	}{
		{"按事件", "?event=auth.login_failed", 1},
		{"按结果", "?result=fail", 1},
		{"按用户", "?uid=" + strconv.FormatUint(uid, 10), 3},
		{"按 IP", "?ip=9.9.9.9", 1},
		{"组合无交集", "?event=auth.login&result=fail", 0},
	}
	for _, tc := range cases {
		rec := adminGET(t, r, "/api/admin/auth-events"+tc.query, sess)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: 应 200，实际 %d %s", tc.name, rec.Code, rec.Body.String())
		}
		if got := decodeEventPage(t, rec).Total; got != tc.want {
			t.Fatalf("%s: 期望 %d 条，实际 %d", tc.name, tc.want, got)
		}
	}

	// 非法结果值必须 400：静默当成「不筛」会让排查者以为「这段时间没有失败」。
	bad := adminGET(t, r, "/api/admin/auth-events?result=bogus", sess)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("非法 result 应 400，实际 %d %s", bad.Code, bad.Body.String())
	}
	if c := decodeEnv(t, bad).ErrorCode; c != "invalid_param" {
		t.Fatalf("非法 result 的错误码应是 invalid_param，实际 %s", c)
	}
	badUID := adminGET(t, r, "/api/admin/auth-events?uid=abc", sess)
	if badUID.Code != http.StatusBadRequest {
		t.Fatalf("非法 uid 应 400，实际 %d", badUID.Code)
	}
}

func TestAdminAuthEventsExportRespectsFilter(t *testing.T) {
	gdb, r := authTestEnv(t)
	seedAuthConfig(t, "optional")
	sess, _ := seedAdminWithEvents(t, gdb, r, "quinn")

	rec := adminGET(t, r, "/api/admin/auth-events/export?event=auth.login_failed", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("导出应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("导出 Content-Type 应为 text/csv，实际 %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "auth-events-") {
		t.Fatalf("导出应带附件文件名，实际 %q", cd)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "\ufeff") {
		t.Fatal("CSV 必须带 UTF-8 BOM，否则 Excel 打开是乱码")
	}
	if !strings.Contains(body, "时间,UID,账号,事件,结果") {
		t.Fatalf("CSV 表头不符: %q", strings.SplitN(body, "\n", 2)[0])
	}
	// 筛选必须同样作用于导出：这条是「列表与导出两套 SQL 漂移」的唯一守卫。
	if !strings.Contains(body, "ghost") {
		t.Fatal("导出应含被筛中的 login_failed 行")
	}
	if strings.Contains(body, "olive") {
		t.Fatal("导出不得包含被筛掉的其它事件")
	}
}

func TestAdminAuthEventsMetaAndAccess(t *testing.T) {
	gdb, r := authTestEnv(t)
	seedAuthConfig(t, "optional")
	sess, _ := seedAdminWithEvents(t, gdb, r, "rita")

	rec := adminGET(t, r, "/api/admin/auth-events/meta", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("meta 应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	var meta struct {
		Events []struct {
			Event string `json:"event"`
			Label string `json:"label"`
		} `json:"events"`
		RetentionDays int `json:"retention_days"`
	}
	if err := json.Unmarshal(decodeEnv(t, rec).Data, &meta); err != nil {
		t.Fatalf("解析 meta 失败: %v", err)
	}
	if len(meta.Events) != len(authevent.Options()) {
		t.Fatalf("事件选项应与白名单等长，实际 %d / %d", len(meta.Events), len(authevent.Options()))
	}
	for _, ev := range meta.Events {
		if ev.Event == "" || ev.Label == "" {
			t.Fatalf("事件选项缺 key 或中文名: %+v", ev)
		}
	}
	if meta.RetentionDays != 90 {
		t.Fatalf("保留天数应读配置（默认 90），实际 %d", meta.RetentionDays)
	}

	// 普通用户禁止读登录流水。
	registerUser(t, r, "sam")
	userSess, _, _ := loginAs(t, r, "sam", "UA/user", nil)
	denied := adminGET(t, r, "/api/admin/auth-events", userSess)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("普通用户访问应 403，实际 %d %s", denied.Code, denied.Body.String())
	}
	// 未登录 401。
	if anon := adminGET(t, r, "/api/admin/auth-events", nil); anon.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d", anon.Code)
	}
	// 导出同样在管理组内。
	if anon := adminGET(t, r, "/api/admin/auth-events/export", nil); anon.Code != http.StatusUnauthorized {
		t.Fatalf("未登录导出应 401，实际 %d", anon.Code)
	}
}
