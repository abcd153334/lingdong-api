package keyscope_test

// 本文件守的是「作用域判据只有一处」这件事本身。它是所有隔离用例的地基：
// 判据一旦退化成 `app_id > 0`，下游所有过滤会一起 fail-open。

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/keyscope"
)

func ctxWith(k *model.APIKey) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if k != nil {
		c.Set("api_key_record", k)
	}
	return c
}

func TestOfWithoutKeyIsAccountScope(t *testing.T) {
	s, ok := keyscope.Of(ctxWith(nil))
	if !ok || s.Hosted || s.AppID != 0 || s.KeyID != 0 {
		t.Fatalf("无 KEY 上下文应为账户作用域，实际 %+v ok=%v", s, ok)
	}
}

func TestOfUserKeyIsAccountScope(t *testing.T) {
	k := &model.APIKey{ID: 7, Kind: model.KeyKindUser, AppID: 0}
	s, ok := keyscope.Of(ctxWith(k))
	if !ok || s.Hosted || s.AppID != 0 {
		t.Fatalf("自建 KEY 应为账户作用域，实际 %+v ok=%v", s, ok)
	}
}

func TestOfHostedKeyIsAppScope(t *testing.T) {
	k := &model.APIKey{ID: 8, Kind: model.KeyKindHosted, AppID: 3}
	s, ok := keyscope.Of(ctxWith(k))
	if !ok || !s.Hosted || s.AppID != 3 || s.KeyID != 8 {
		t.Fatalf("托管密钥应带应用作用域，实际 %+v ok=%v", s, ok)
	}
}

func TestOfHostedWithoutAppIsInconsistent(t *testing.T) {
	k := &model.APIKey{ID: 9, Kind: model.KeyKindHosted, AppID: 0}
	s, ok := keyscope.Of(ctxWith(k))
	if ok {
		t.Fatalf("hosted 且 app_id=0 必须判为自相矛盾")
	}
	// 即使判为不合法，仍要带回 KeyID/Hosted：调用方据此做"按 KEY 收口"，
	// 不能因为拒绝而把它当成账户作用域。
	if !s.Hosted || s.KeyID != 9 {
		t.Fatalf("非法作用域仍应标明 hosted/keyID，实际 %+v", s)
	}
}

func TestAppFilter(t *testing.T) {
	cases := []struct {
		name       string
		key        *model.APIKey
		wantAppID  uint64
		wantScoped bool
		wantOK     bool
	}{
		{"无凭证", nil, 0, false, true},
		{"自建 KEY", &model.APIKey{ID: 1, Kind: model.KeyKindUser}, 0, false, true},
		{"托管密钥", &model.APIKey{ID: 2, Kind: model.KeyKindHosted, AppID: 5}, 5, true, true},
		{"托管无应用", &model.APIKey{ID: 3, Kind: model.KeyKindHosted, AppID: 0}, 0, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			appID, scoped, ok := keyscope.AppFilter(ctxWith(tc.key))
			if appID != tc.wantAppID || scoped != tc.wantScoped || ok != tc.wantOK {
				t.Fatalf("期望 (%d,%v,%v)，实际 (%d,%v,%v)",
					tc.wantAppID, tc.wantScoped, tc.wantOK, appID, scoped, ok)
			}
		})
	}
}

func TestConsistent(t *testing.T) {
	if !keyscope.Consistent(nil) {
		t.Fatal("nil KEY 应视为自洽（未鉴权路径）")
	}
	if !keyscope.Consistent(&model.APIKey{Kind: model.KeyKindUser}) {
		t.Fatal("自建 KEY 应自洽")
	}
	if !keyscope.Consistent(&model.APIKey{Kind: model.KeyKindHosted, AppID: 1}) {
		t.Fatal("有归属应用的托管密钥应自洽")
	}
	if keyscope.Consistent(&model.APIKey{Kind: model.KeyKindHosted, AppID: 0}) {
		t.Fatal("hosted 且 app_id=0 必须判为不自洽")
	}
}
