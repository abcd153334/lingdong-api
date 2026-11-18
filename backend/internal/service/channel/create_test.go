package channel

import (
	"fmt"
	"strings"
	"testing"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/channelkeys"
)

// 本文件守「创建入口」与增量写（applyOps）的密钥规格必须一致。
//
// 历史缺口：Update 里有 Dedupe，Create 没有 —— 于是新建渠道粘贴两遍同一把 key
// 会落成两行同 keyID 的条目，面板上两行共享一个启用开关（关一个另一个也关），
// 而冷却键只有一个。这类"只有从某个入口进才出问题"的差异最难在联调里发现。

// TestCreateDedupesAndNormalizesKeys 断言创建时逐条归一 + 去重（大小写不同不算重复）。
func TestCreateDedupesAndNormalizesKeys(t *testing.T) {
	withChannelDB(t)

	r := channelReq("去重渠道")
	// sk-A 与 sk-a 是两个不同明文 → HMAC 不同 → 必须都保留。
	r.Keys = []string{"sk-a", " sk-a ", "sk-b", "sk-A", "", "   "}

	item, err := Create(r, "")
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	var c model.Channel
	if err := db.DB.First(&c, item.ID).Error; err != nil {
		t.Fatalf("读取渠道失败: %v", err)
	}
	plains := DecryptKeys(c.Keys)
	if len(plains) != 3 {
		t.Fatalf("应去重成 3 把，实际 %v", plains)
	}
	want := []string{"sk-a", "sk-b", "sk-A"}
	for i := range want {
		if plains[i] != want[i] {
			t.Fatalf("顺序与归一不符：期望 %v，实际 %v（去重必须保序，否则轮转顺序会变）", want, plains)
		}
	}
}

// TestCreateRejectsOversizedPool 断言条数上限在创建期就生效（增量写那边同样有）。
func TestCreateRejectsOversizedPool(t *testing.T) {
	withChannelDB(t)

	r := channelReq("超量渠道")
	r.Keys = make([]string, 0, channelkeys.MaxKeysPerChannel+1)
	for i := 0; i <= channelkeys.MaxKeysPerChannel; i++ {
		// 必须两两不同：重复的会在去重后掉到 100 以下，用例就变成"通过但没测到上限"。
		r.Keys = append(r.Keys, fmt.Sprintf("sk-%03d", i))
	}

	_, err := Create(r, "")
	if err == nil || !strings.Contains(err.Error(), "100") {
		t.Fatalf("超过 %d 把应被拒并报出上限，实际 %v", channelkeys.MaxKeysPerChannel, err)
	}
}

// TestCreateRejectsMalformedKey 断言"内嵌空白 / 超长"在创建期就被拦。
//
// 放进去的后果不是报错而是静默 401：上游收到一把带空格的 key，
// 排查方向会跑到"账号被封"，而真正的原因只是从表格里粘错了一行。
func TestCreateRejectsMalformedKey(t *testing.T) {
	withChannelDB(t)

	cases := []struct {
		name string
		key  string
		want string
	}{
		{"内嵌空格", "sk-a sk-b", "空格"},
		{"内嵌制表符", "sk-a\tsk-b", "空格"},
		{"超长", "sk-" + strings.Repeat("x", channelkeys.MaxKeyLen), "过长"},
	}
	for _, c := range cases {
		r := channelReq("坏密钥渠道-" + c.name)
		r.Keys = []string{c.key}
		_, err := Create(r, "")
		if err == nil {
			t.Fatalf("%s 应被拒绝", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s 的文案应含 %q，实际 %v", c.name, c.want, err)
		}
	}
}
