package presets

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// 本文件守「出厂种子本身」的三条底线。它不连库 —— 数据脏了不该等到写库才炸。
//
//  1. **每个内置渠道都必须有可跳转的官网**：新用户第一眼看到的就是渠道名 + 官网入口，
//     留空等于给了一条"没有出处"的渠道（运营也无法判断这是哪一家）。
//  2. **种子里绝不能出现凭据或租户私有对象**：种子会进公开仓 + 进每个新实例，
//     一次误导出就是全网都知道的密钥（或别人账号里的工作流 ID）。
//  3. **组标识唯一、版本合法**：marker 撞车会让后一组永远不写入（且不报错）。

// mustHaveWebsite 断言每个内置组都配了合法官网（这是产品决策，不是建议）。
func TestBuiltinGroupsAllHaveWebsite(t *testing.T) {
	groups := builtinPresetGroups()
	if len(groups) == 0 {
		t.Fatal("presets_builtin.json 为空：出厂骨架没了，新实例将没有任何参考配置")
	}
	for _, g := range groups {
		w := strings.TrimSpace(g.Channel.Website)
		if w == "" {
			t.Errorf("%s 的渠道 %q 没有配置官网：内置渠道必须显式给出上游官网", g.MarkerKey, g.Channel.Name)
			continue
		}
		u, err := url.Parse(w)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			t.Errorf("%s 的官网不是合法 http(s) 地址：%q", g.MarkerKey, w)
		}
	}
}

// 现网导出的组必须都在：少一组 = 少一整套渠道与模型，而启动不会报错。
func TestBuiltinGroupsCoverProductionSet(t *testing.T) {
	want := []string{
		"builtin_presets.duomiapi",
		"builtin_presets.metaso",
		"builtin_presets.metaso-2",
		"builtin_presets.autodl",
		"builtin_presets.deepseek",
	}
	have := map[string]bool{}
	for _, g := range builtinPresetGroups() {
		have[g.MarkerKey] = true
	}
	for _, k := range want {
		if !have[k] {
			t.Errorf("内置预设缺组 %s（现网在用的配置不能从出厂骨架里消失）", k)
		}
	}
}

// 种子必须干净：无凭据、无租户私有对象、无上传图标。
func TestBuiltinSeedHasNoSecrets(t *testing.T) {
	raw := string(builtinPresetJSON)

	// 三类凭据形态：OpenAI 风格、AWS、PEM 私钥。
	secretPatterns := map[string]*regexp.Regexp{
		"sk- 密钥": regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{8,}`),
		"AWS AK": regexp.MustCompile(`\bAKIA[0-9A-Z]{12,}`),
		"PEM 私钥": regexp.MustCompile(`BEGIN [A-Z ]*PRIVATE KEY`),
		"上传图标对象": regexp.MustCompile(`"upload:`),
	}
	for name, re := range secretPatterns {
		if loc := re.FindString(raw); loc != "" {
			t.Errorf("种子正文出现%s：%s（凭据与租户对象绝不能进出厂数据）", name, loc)
		}
	}

	// keys / key_meta / balance 是运行期状态，不是出厂事实：出现即说明导出工具漏了清洗。
	for _, banned := range []string{`"keys":`, `"key_meta":`, `"balance_cny":`, `"health":`} {
		if strings.Contains(raw, banned) {
			t.Errorf("种子正文出现运行期字段 %s：请用 tools/presetdump 重新导出", banned)
		}
	}
}

// 组标识唯一 + 版本合法：marker 撞车会让后一组静默不写入。
func TestBuiltinGroupKeysUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, g := range presetGroups() {
		if seen[g.MarkerKey] {
			t.Errorf("marker 重复：%s（后一组将永远不写入）", g.MarkerKey)
		}
		seen[g.MarkerKey] = true
		if g.Version < 1 {
			t.Errorf("%s 的 version 必须 ≥ 1（0 与「未写入」不可区分）", g.MarkerKey)
		}
		if !strings.HasPrefix(g.RowGroup(), "builtin.") {
			t.Errorf("%s 的行标记不是 builtin.*：%q", g.MarkerKey, g.RowGroup())
		}
	}
}
