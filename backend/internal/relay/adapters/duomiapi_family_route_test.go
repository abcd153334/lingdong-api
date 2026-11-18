package adapters

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"lingdong-api/internal/relay"
)

// 本文件锁「厂商升级型号零配置可用」这条链路本身：
// 族推断正确还不够，必须证明它能一路走到出站报文，且拦截分支真的零出站。
//
// 为什么不用 duomiModels 里的 ID 做样本：那些是内置档案，走的是精确表分支，
// 永远测不到族表。这里的样本全是「表里没有、只靠前缀推断」的新型号。

func TestDuomiFamilyZeroConfigAsync(t *testing.T) {
	cases := []struct {
		modelID  string
		wantPath string // 提交路径片段
		wantMode string // "" = 报文里不该出现 mode
	}{
		{"gpt-image-2.6-sunburst", "/images/generations", ""},
		{"nano-banana-3", "/nano-banana", ""},
		{"kling-v4-pro", "/videos/", "pro"},
		{"kling-v4-omni", "/videos/omni-video", "std"},
	}
	for _, c := range cases {
		t.Run(c.modelID, func(t *testing.T) {
			srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
				duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"t-1"}}`)
			})
			body := `{"prompt":"p","model":"` + c.modelID + `"}`
			// UpstreamModel 来自绑定（必填）。这里模拟「绑定已填上游真名」的真实状态 ——
			// 族表只负责协议，不负责上游名。
			req := &relay.TaskSubmitRequest{
				ModelID: c.modelID, UpstreamModel: c.modelID, Body: []byte(body),
			}
			if _, err := (DuoMi{}).SubmitTask(context.Background(), duomiCfg(srv.URL, nil), req); err != nil {
				t.Fatalf("同类新型号应零配置可用，实际 %v", err)
			}
			if req.SubKind == "" {
				t.Fatal("必须回写 SubKind（轮询靠它选端点）")
			}
			if rec.hits.Load() == 0 {
				t.Fatal("应真实出站一次")
			}
			if !strings.Contains(rec.path, c.wantPath) {
				t.Fatalf("提交路径应含 %q，实际 %q", c.wantPath, rec.path)
			}
			// 出站上游模型名必须是绑定值（这里与平台 ID 同名）。
			// 可灵用 model_name，其余协议用 model —— 这也顺带锁住「协议选对了」。
			key := "model"
			if strings.HasPrefix(c.wantPath, "/videos/") {
				key = "model_name"
			}
			if got, _ := rec.body[key].(string); got != c.modelID {
				t.Fatalf("出站 %s 应为 %q，实际 %q", key, c.modelID, got)
			}
			mode, hasMode := rec.body["mode"]
			if c.wantMode == "" {
				if hasMode {
					t.Fatalf("该族不该下发 mode，实际 %v", mode)
				}
			} else if mode != c.wantMode {
				t.Fatalf("mode 应为 %q，实际 %v", c.wantMode, mode)
			}
		})
	}
}

// C 类（协议族存在但没实现）必须与「模型没登记」区分开，且同样零出站。
func TestDuomiUnsupportedFamilyZeroEgress(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0}`)
	})
	_, _, err := submitDuomi(t, duomiCfg(srv.URL, nil), "veo-3", `{"prompt":"p"}`)
	if err == nil {
		t.Fatal("未实现的协议族应报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "尚未实现该协议") {
		t.Fatalf("文案应说明协议未实现，实际 %q", msg)
	}
	if strings.Contains(msg, "未知的多米模型") {
		t.Fatalf("C 类不能退化成「未知模型」——那会让运维反复试 override，实际 %q", msg)
	}
	var ce *relay.ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("必须是 ConfigError（不熔断渠道），实际 %T", err)
	}
	if rec.hits.Load() != 0 {
		t.Fatal("本地配置错误必须零出站")
	}
}

// 渠道 override 的显式协议必须赢过族推断：运营声明优先于任何猜测。
func TestDuomiOverrideWinsOverFamily(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"t-1"}}`)
	})
	// kling-v4-pro 本应推断成 kling，这里显式改成 nano（等价于「厂商换了信封」）。
	opts := []byte(`{"model_overrides":{"kling-v4-pro":{"protocol":"nano","upstream_model":"gemini-3-pro-image-preview"}}}`)
	_, req, err := submitDuomi(t, duomiCfg(srv.URL, opts), "kling-v4-pro", `{"prompt":"p"}`)
	if err != nil {
		t.Fatalf("override 应生效：%v", err)
	}
	if req.SubKind != duomiProtoNano {
		t.Fatalf("SubKind 应为 %q（override 优先），实际 %q", duomiProtoNano, req.SubKind)
	}
	if got, _ := rec.body["model"].(string); got != "gemini-3-pro-image-preview" {
		t.Fatalf("出站 model 应取 override 的上游名，实际 %q", got)
	}
}

// 新模型总开关必须覆盖同步桥。
//
// 旧实现只在 SubmitTask 里检查：运营置 0 止血图像模型后，
// /v1/images/generations 仍然照常出网、照常扣费 —— 开关形同虚设。
func TestDuomiNewModelGateOnSyncPath(t *testing.T) {
	t.Setenv("LD_DUOMI_NEW_MODELS", "0")
	ts, rec := startSyncSrv(t, &startDuomiSync{polls: 1})

	_, err := forwardDuomi(t, duomiCfg(ts.URL, nil), "gpt-image-2.6-sunburst",
		"/v1/images/generations", `{"model":"gpt-image-2.6-sunburst","prompt":"a cat"}`)
	if err == nil {
		t.Fatal("开关关闭时同步桥也必须拒绝")
	}
	if !strings.Contains(err.Error(), "LD_DUOMI_NEW_MODELS") {
		t.Fatalf("文案应点名开关，实际 %q", err.Error())
	}
	var ce *relay.ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("必须是 ConfigError，实际 %T", err)
	}
	if rec.submitPath != "" {
		t.Fatalf("关闭后不该有任何上游调用，实际提交到 %q", rec.submitPath)
	}
}

// 同步桥同样要能承接族内新型号（不只是异步）。
func TestDuomiFamilyZeroConfigSync(t *testing.T) {
	ts, rec := startSyncSrv(t, &startDuomiSync{polls: 1})
	resp, err := forwardDuomi(t, duomiCfg(ts.URL, nil), "gpt-image-2.6-sunburst",
		"/v1/images/generations", `{"model":"gpt-image-2.6-sunburst","prompt":"a cat"}`)
	if err != nil {
		t.Fatalf("同步桥应零配置承接族内新型号：%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", resp.StatusCode)
	}
	if got, _ := rec.submitBody["model"].(string); got != "gpt-image-2.6-sunburst" {
		t.Fatalf("出站 model 应为绑定值，实际 %q", got)
	}
}
