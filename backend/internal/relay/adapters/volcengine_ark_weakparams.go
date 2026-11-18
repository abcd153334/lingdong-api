package adapters

// 火山方舟**视频**的「提示词弱校验参数」检测（只留痕，不改行为）。
//
// 官方《创建视频生成任务》原文：
//
//	对于 resolution、ratio、duration、frames、seed、camera_fixed、watermark，
//	除了在 request body 中直接传入，也支持在文本提示词后追加 `--[parameters]` 的
//	弱校验方式传参，**所有模型均兼容**……弱校验方式：若参数填写错误，该参数将被忽略或触发报错。
//
// 这意味着提示词里可以藏一份**真实的生成参数**，而平台的计费只读 body。
// 于是「body 写 720p（按 720p 冻结结算）+ 提示词写 --rs 4k（上游按 4k 出片）」
// 在平台侧看起来完全正常 —— 计费档与交付档不一致，差额由平台承担。
//
// 本轮处置（产品决定）：**不拦、不剥**，只把冲突上报成可查的痕迹
// （结构化告警 + usages.anomaly=weak_param_pricing_conflict）。
// 不剥离的理由：剥掉等于改写用户的提示词，而这类参数对上游是有效的；
// 不拦的理由：已有脚本在用它，直接 400 会打断线上调用。
//
// 关键约束：**绝不修改出站字节**。本文件只做只读扫描。

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// arkWeakKind 是弱校验参数的类别（决定「值形状是否合法」与「是否影响计价」）。
type arkWeakKind int

const (
	arkWeakUnknown arkWeakKind = iota
	arkWeakResolution
	arkWeakDuration
	arkWeakFrames
	arkWeakRatio
	arkWeakSeed
	arkWeakBool
)

// arkWeakParam 是一条登记（canonical 名 + 类别）。
type arkWeakParam struct {
	canonical string
	kind      arkWeakKind
}

// arkWeakParams 是弱校验参数的**唯一登记表**（全名与官方示例里的短名）。
//
// 只登记文档点名的 7 个参数：多登记一个就会把普通文案里形如 `--xxx` 的内容
// 误当成参数，少登记一个就漏掉一类计价冲突。
var arkWeakParams = map[string]arkWeakParam{
	"rs":           {"resolution", arkWeakResolution},
	"resolution":   {"resolution", arkWeakResolution},
	"dur":          {"duration", arkWeakDuration},
	"duration":     {"duration", arkWeakDuration},
	"frames":       {"frames", arkWeakFrames},
	"rt":           {"ratio", arkWeakRatio},
	"ratio":        {"ratio", arkWeakRatio},
	"seed":         {"seed", arkWeakSeed},
	"cf":           {"camera_fixed", arkWeakBool},
	"camera_fixed": {"camera_fixed", arkWeakBool},
	"wm":           {"watermark", arkWeakBool},
	"watermark":    {"watermark", arkWeakBool},
}

// arkWeakLeftBoundary 是 `--` 左侧允许出现的字符（空白由调用方单独判）。
//
// 为什么要判左边界：URL 里的 `a--seed`、模型名里的 `seedream-5-0--preview`
// 都不是参数。不判就会把正常文案改判成「计价冲突」，告警一假就没人看了。
const arkWeakLeftBoundary = "。，、；：！？,;:!?）)】]\"'"

// arkScanWeakParams 从一段文本里切出弱校验参数（canonical → 值）。
//
// 识别失败的片段**原样忽略**：弱校验本来就是「写错了上游会忽略」，
// 平台更没有理由因为一段看不懂的文本报错。同一参数出现多次时保留第一次的值
// （冲突检测只关心「body 与提示词是否一致」，不关心提示词内部怎么写的）。
func arkScanWeakParams(text string) map[string]string {
	if !strings.Contains(text, "--") {
		return nil
	}
	out := make(map[string]string, 2)
	for i := 0; i+2 < len(text); {
		if text[i] != '-' || text[i+1] != '-' {
			i++
			continue
		}
		if !arkWeakBoundaryOK(text, i) {
			i += 2
			continue
		}
		// 名字：紧跟的 ASCII 字母（大小写都接受，统一小写查表）。
		j := i + 2
		for j < len(text) && isASCIIAlpha(text[j]) {
			j++
		}
		if j == i+2 {
			// `-- 作者注`：`--` 后不是名字，不认。
			i = j
			continue
		}
		name := strings.ToLower(text[i+2 : j])
		// 名字后必须是空白：`--rst`、`--seed11` 都不是参数。
		if j >= len(text) || !isSpaceByte(text[j]) {
			i = j
			continue
		}
		spec, ok := arkWeakParams[name]
		if !ok {
			i = j
			continue
		}
		// 值：跳过空白后读到下一个空白。
		k := j
		for k < len(text) && isSpaceByte(text[k]) {
			k++
		}
		v := k
		for v < len(text) && !isSpaceByte(text[v]) {
			v++
		}
		val := text[k:v]
		if val == "" || !arkWeakValueOK(spec.kind, val) {
			i = v
			continue
		}
		if _, dup := out[spec.canonical]; !dup {
			out[spec.canonical] = val
		}
		i = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// arkWeakBoundaryOK 判断 `--` 前面是不是一个合理边界。
//
// 必须按 **rune** 取前一个字符：`。` 在 UTF-8 里是 3 个字节，
// 用 text[idx-1] 拿到的是 0x82，中文标点边界会全部失效
// （症状是「小猫打哈欠。--rs 4k」识别不出来，套利窗口原样保留）。
func arkWeakBoundaryOK(text string, idx int) bool {
	if idx == 0 {
		return true
	}
	prev, _ := utf8.DecodeLastRuneInString(text[:idx])
	if prev == ' ' || prev == '\t' || prev == '\n' || prev == '\r' {
		return true
	}
	return strings.ContainsRune(arkWeakLeftBoundary, prev)
}

// arkWeakValueOK 按参数类别校验值形状（形状不对就当没写，交给上游自己忽略）。
func arkWeakValueOK(kind arkWeakKind, val string) bool {
	switch kind {
	case arkWeakResolution:
		switch strings.ToLower(val) {
		case "480p", "720p", "1080p", "2k", "4k", "-1":
			return true
		}
		return false
	case arkWeakDuration, arkWeakFrames, arkWeakSeed:
		_, err := strconv.ParseInt(val, 10, 64)
		return err == nil
	case arkWeakRatio:
		return val == "adaptive" || isDigitRatio(val)
	case arkWeakBool:
		v := strings.ToLower(val)
		return v == "true" || v == "false"
	}
	return false
}

// arkWeakNotesFromBody 比对「提示词里的计价参数」与「body 里的参数」，返回差异说明。
//
// 只关心会影响金额的三个（resolution / duration / frames）：
//   - ratio / seed / camera_fixed / watermark 不影响本平台计价，比对它们只会制造噪音；
//   - 三个里任何一个「body 没写」或「与 body 不一致」，都意味着计费档可能低于交付档。
//
// 原生通道（content[]）与规范体都走这一个函数：前者只替换 model、不改字节，
// 所以它比规范体更容易藏参数，反而更需要这条线索。
func arkWeakNotesFromBody(body []byte) []string {
	var probe struct {
		Prompt     string `json:"prompt"`
		Resolution string `json:"resolution"`
		Size       string `json:"size"`
		Duration   any    `json:"duration"`
		Seconds    any    `json:"seconds"`
		Frames     any    `json:"frames"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return nil
	}
	texts := make([]string, 0, len(probe.Content)+1)
	if p := strings.TrimSpace(probe.Prompt); p != "" {
		texts = append(texts, p)
	}
	for _, c := range probe.Content {
		if strings.EqualFold(strings.TrimSpace(c.Type), "text") && strings.TrimSpace(c.Text) != "" {
			texts = append(texts, c.Text)
		}
	}
	if len(texts) == 0 {
		return nil
	}
	weak := map[string]string{}
	for _, t := range texts {
		for k, v := range arkScanWeakParams(t) {
			if _, dup := weak[k]; !dup {
				weak[k] = v
			}
		}
	}
	if len(weak) == 0 {
		return nil
	}

	notes := make([]string, 0, 3)
	// resolution：body 侧 resolution 优先，其次 size（平台把 size 折叠成 resolution）。
	bodyRes := strings.ToLower(strings.TrimSpace(firstNonEmptyStr(probe.Resolution, probe.Size)))
	if v, ok := weak["resolution"]; ok && !strings.EqualFold(strings.ToLower(v), bodyRes) {
		notes = append(notes, arkWeakNote("resolution", v, bodyRes))
	}
	// duration：body 侧 duration 优先，其次 seconds（平台两者等价）。
	bodyDur := arkAnyToIntText(probe.Duration)
	if bodyDur == "" {
		bodyDur = arkAnyToIntText(probe.Seconds)
	}
	if v, ok := weak["duration"]; ok && v != bodyDur {
		notes = append(notes, arkWeakNote("duration", v, bodyDur))
	}
	// frames：内置模型全都不支持（官方仅 Seedance 1.0 pro 系列），
	// 提示词里写它只会让上游按帧数出片、而平台按秒计费。
	if v, ok := weak["frames"]; ok {
		notes = append(notes, arkWeakNote("frames", v, arkAnyToIntText(probe.Frames)))
	}
	return notes
}

// arkWeakNote 生成一条可读的差异说明（进日志，不进对外响应）。
func arkWeakNote(field, weakVal, bodyVal string) string {
	if bodyVal == "" {
		return "weak." + field + "=" + weakVal + "（body 未传）"
	}
	return "weak." + field + "=" + weakVal + "（body=" + bodyVal + "）"
}

// arkAnyToIntText 把 JSON 里的数字/数字串归一成十进制文本（空 = 没给）。
func arkAnyToIntText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case string:
		s := strings.TrimSpace(t)
		if _, err := strconv.ParseInt(s, 10, 64); err == nil {
			return s
		}
	}
	return ""
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// isDigitRatio 判断形如 16:9 的比例写法。
func isDigitRatio(s string) bool {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return false
	}
	for _, part := range []string{s[:i], s[i+1:]} {
		for k := 0; k < len(part); k++ {
			if part[k] < '0' || part[k] > '9' {
				return false
			}
		}
	}
	return true
}
