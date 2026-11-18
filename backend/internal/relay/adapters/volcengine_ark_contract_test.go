package adapters

import (
	"strings"
	"testing"

	"lingdong-api/internal/apidoc"
)

// TestArkVendorWhitelistMatchesDoc 锁死「对外字段表 ↔ 出站白名单」同源（B7-7）。
//
// 两份清单必须彼此覆盖，任一方向缺失都是用户可见的错误，而且都不报错：
//
//  1. 白名单有、文档没写 → 参数能用但没人知道，等于不存在（用户照文档传参永远传不到）；
//  2. 文档写了、白名单没放 → 文档承诺了，出站时被静默丢弃，
//     用户看到的是「模型不按我说的生成」，查不出原因（这正是 arkMetaWhitelist 存在的原因）。
//
// 判据只用两个集合：`arkMetaWhitelist`（原样透传到 Ark 顶层）与 `arkSpecOwnedKeys`
// （适配器自己消费/翻译的键）。**不引入第三份清单** —— 那样守卫就退化成
// 「三个地方各写一遍，谁也没被谁约束」。
func TestArkVendorWhitelistMatchesDoc(t *testing.T) {
	// 视频提交的字段契约在族文档里（Seedance 走火山 Ark），
	// 不在 endpointDocs：那张表只放平台自有端点。
	ep, ok := apidoc.FamilyEndpointOf(apidoc.FamilyVideosSeedance, apidoc.DocumentKey{Kind: apidoc.KindVideoSubmit})
	if !ok {
		t.Fatal("Seedance 族缺少 video/submit 文档")
	}
	doc := ep.Spec

	// 顶层字段名 → 是否厂商扩展（TierVendor）。
	// 只收顶层：`input_reference[].type` 这类子路径不是出站顶层键。
	topFields := map[string]apidoc.Field{}
	for _, f := range doc.Fields {
		if strings.Contains(f.Name, "[") || strings.Contains(f.Name, ".") {
			continue
		}
		topFields[f.Name] = f
	}

	// 方向一：白名单里的每个键都必须在文档里出现过。
	for _, k := range arkVendorTopKeys {
		if _, ok := topFields[k]; !ok {
			t.Errorf("白名单键 %q 未出现在 %s 字段表中：用户照文档传参永远传不到这个字段", k, doc.Name)
		}
	}

	// 方向二：文档中标注「Seedance / 部分模型」的顶层字段，必须真的被 Ark 接受。
	// 只看这两类标注：其余（可灵、即梦）本来就不该出现在 Ark 白名单里。
	for name, f := range topFields {
		if !arkModelsOfField(f.OnlyModels) {
			continue
		}
		if arkMetaWhitelist[name] || arkSpecOwnedKeys[name] || name == "metadata" {
			continue
		}
		t.Errorf("文档把 %q 标成 Ark 可用（%q），但它既不在出站白名单也不在适配器自管键里："+
			"用户传了会被静默丢弃", name, f.OnlyModels)
	}
}

// arkModelsOfField 判断 OnlyModels 是否声明「Ark（Seedance）支持」。
//
// 「部分模型」也算：它在本仓库的语义就是「其中包含 Ark 内置模型」，
// 含糊标注必须按「支持」校验 —— 否则改标注就能绕过守卫。
func arkModelsOfField(onlyModels string) bool {
	s := strings.TrimSpace(onlyModels)
	if s == "" {
		return false
	}
	return strings.Contains(s, "Seedance") || strings.Contains(s, "部分模型")
}
