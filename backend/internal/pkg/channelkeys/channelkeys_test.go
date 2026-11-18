package channelkeys

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// 本文件是密钥池的**行为契约**：下标保序与稳定标识是多 Key 正确性的地基，
// 一旦破坏，冷却/归因/账单归属会同时错位且都不会报错。

func mustEncrypt(t *testing.T, keys ...string) model.JSON {
	t.Helper()
	raw, err := Encrypt(keys)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	return raw
}

// TestEncryptDecryptRoundTrip 补上历史空白：加解密此前没有任何用例。
func TestEncryptDecryptRoundTrip(t *testing.T) {
	keys := []string{"sk-aaa-111", "sk-bbb-222", "xhz-ccc-333"}
	raw := mustEncrypt(t, keys...)

	res := Decrypt(raw)
	if res.Invalid || res.Total != 3 || res.Failed != 0 {
		t.Fatalf("解密结果异常: %+v", res)
	}
	for i, e := range res.Entries {
		if e.Index != i {
			t.Errorf("第 %d 条 Index 应为 %d，实际 %d", i, i, e.Index)
		}
		if e.Plain != keys[i] {
			t.Errorf("第 %d 条明文错位: 期望 %q 实际 %q", i, keys[i], e.Plain)
		}
		if e.ID == "" || len(e.ID) != 32 {
			t.Errorf("第 %d 条 keyID 应为 32 位 hex，实际 %q", i, e.ID)
		}
	}
}

// TestDecryptPreservesIndexOnFailure 是本包最核心的回归锁。
//
// 旧实现跳过解密失败项，导致返回数组下标左移：第 3 把坏掉时，第 2 把的
// 冷却会被记到第 1 把头上。这里断言「失败项保留 + 后续项 Index 不变」。
func TestDecryptPreservesIndexOnFailure(t *testing.T) {
	raw := mustEncrypt(t, "sk-good-1", "sk-corrupt", "sk-good-2")

	var tokens []string
	if err := json.Unmarshal(raw, &tokens); err != nil {
		t.Fatalf("解析 token 失败: %v", err)
	}
	tokens[1] = "not-a-valid-fernet-token"
	broken, err := json.Marshal(tokens)
	if err != nil {
		t.Fatalf("重新序列化失败: %v", err)
	}

	res := Decrypt(model.JSON(broken))
	if res.Total != 3 || res.Failed != 1 {
		t.Fatalf("应为 3 条、1 条失败，实际 total=%d failed=%d", res.Total, res.Failed)
	}
	if len(res.Entries) != 3 {
		t.Fatalf("失败项必须保留，实际只剩 %d 条", len(res.Entries))
	}
	if res.Entries[1].OK() {
		t.Error("中间那条应标记为失败")
	}
	if res.Entries[2].Index != 2 {
		t.Errorf("失败项之后的 Index 不得左移，期望 2 实际 %d", res.Entries[2].Index)
	}
	if res.Entries[2].Plain != "sk-good-2" {
		t.Errorf("第 3 条明文错位: %q", res.Entries[2].Plain)
	}
	// 第 3 条的 ID 必须与「单独加密它」算出的 ID 完全一致 —— 这是冷却键稳定的前提。
	want := ID([]byte("sk-good-2"))
	if res.Entries[2].ID != want {
		t.Errorf("keyID 受失败项影响: 期望 %s 实际 %s", want, res.Entries[2].ID)
	}
}

// TestDecryptInvalidColumn 列本身被写坏时只能报 Invalid，绝不能 panic。
func TestDecryptInvalidColumn(t *testing.T) {
	for _, raw := range []model.JSON{
		model.JSON(`not json`),
		model.JSON(`{"keys":["a"]}`),
		model.JSON(`[1,2,3]`),
	} {
		res := Decrypt(raw)
		if !res.Invalid {
			t.Errorf("非法列 %s 应标记 Invalid，实际 %+v", raw, res)
		}
		if len(res.Entries) != 0 {
			t.Errorf("非法列不应产出条目，实际 %d 条", len(res.Entries))
		}
		if res.FailedCount() != 1 {
			t.Errorf("非法列应按 1 次失败计数，实际 %d", res.FailedCount())
		}
	}
}

func TestDecryptEmpty(t *testing.T) {
	if res := Decrypt(nil); res.Invalid || res.Total != 0 || len(res.Entries) != 0 {
		t.Fatalf("空池应返回零值结果，实际 %+v", res)
	}
	if _, err := Encrypt(nil); err != ErrEmptyPool {
		t.Fatalf("空池加密必须报 ErrEmptyPool，实际 %v", err)
	}
}

// TestIDStableAndDomainSeparated 断言 keyID 的性质：同明文恒同、与裸哈希不同。
func TestIDStableAndDomainSeparated(t *testing.T) {
	a := ID([]byte("sk-same"))
	b := ID([]byte("sk-same"))
	if a != b {
		t.Fatalf("同一明文必须得到同一 ID: %s vs %s", a, b)
	}
	if a == ID([]byte("sk-other")) {
		t.Fatal("不同明文不应撞 ID")
	}
	sum := sha256.Sum256([]byte("sk-same"))
	if a == hex.EncodeToString(sum[:])[:32] {
		t.Fatal("keyID 不得等于裸 sha256（低熵 key 可被离线反查）")
	}
	if len(a) != 32 {
		t.Fatalf("keyID 应为 32 位 hex，实际 %d: %s", len(a), a)
	}
}

func TestFingerprint(t *testing.T) {
	long := "sk-abcdefghijklmnopqrstuvwxyz"
	got := Fingerprint(long)
	if !strings.HasPrefix(got, "sk-abc") || !strings.HasSuffix(got, "wxyz") {
		t.Errorf("长 key 指纹应为前6+后4，实际 %q", got)
	}
	if strings.Contains(got, "defghijklmnopqrstuv") {
		t.Errorf("指纹暴露了中间段: %q", got)
	}
	for _, short := range []string{"short", "sk-test", "123456789012345"} {
		if Fingerprint(short) != "******" {
			t.Errorf("短 key %q 必须全遮，实际 %q", short, Fingerprint(short))
		}
	}
}

func TestDedupe(t *testing.T) {
	in := []string{"sk-a", "sk-b", "sk-a", "sk-c", "sk-b"}
	out, removed := Dedupe(in)
	if removed != 2 {
		t.Fatalf("应移除 2 条重复，实际 %d", removed)
	}
	want := []string{"sk-a", "sk-b", "sk-c"}
	if len(out) != len(want) {
		t.Fatalf("去重后条数应为 %d，实际 %d (%v)", len(want), len(out), out)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("去重必须保序：期望 %v 实际 %v", want, out)
		}
	}
	// 全部唯一时原样返回，removed=0
	if got, r := Dedupe([]string{"sk-x", "sk-y"}); r != 0 || len(got) != 2 {
		t.Fatalf("无重复时不应改动: %v removed=%d", got, r)
	}
}
