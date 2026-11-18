// Package releasecheck 负责「有没有新版本」这一件事：读配置里的清单地址、抓一份
// release.json、解析成最小结构、与当前版本比较。
//
// 边界：
//   - 它**不决定**要不要更新，也不触发更新；只回答"清单说什么"与"是否更新"。
//   - 它**不判断**机器上跑的是哪一版（那是 /api/health 的 data.version）。
//   - 抓取失败是一种**状态**（默认拒绝方向：结果标为未知），不是"已是最新"。
package releasecheck

import (
	"regexp"
	"strconv"
	"strings"
)

// versionRe 是本系统认可的版本形状：vMAJOR.MINOR.PATCH + 可选后缀。
//
// 为什么限制 1–9 位数字：int64 装得下，而 10 位以上的版本号只可能来自脏数据；
// 早点判定为非法，好过在比较时悄悄溢出成一个负数。
var versionRe = regexp.MustCompile(`^v?([0-9]{1,9})\.([0-9]{1,9})\.([0-9]{1,9})(?:[-+][0-9A-Za-z.]+)?$`)

// commitRe 是提交号的形状（短号 7 位起）。
var commitRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// Version 是版本的数值三元组。
type Version struct {
	Major int64
	Minor int64
	Patch int64
}

// ParseVersion 解析版本串。后缀（`-rc1` / `+build`）**不参与**排序。
//
// 为什么不实现预发布顺序：`v2.2.0-rc1` 与 `v2.2.0` 谁新需要完整的 semver 语义
// （预发布 < 正式、再按标识符逐段比），而本系统的发布只有正式 tag。与其给一个
// 看起来对、边界错的比较，不如明确"后缀不参与排序"。
func ParseVersion(raw string) (Version, bool) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return Version{}, false
	}
	nums := [3]int64{}
	for i := 0; i < 3; i++ {
		n, err := strconv.ParseInt(m[i+1], 10, 64)
		if err != nil {
			return Version{}, false
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2]}, true
}

// ValidVersion 报告一个串是否是可比较的版本。
func ValidVersion(raw string) bool {
	_, ok := ParseVersion(raw)
	return ok
}

// ValidTag 报告一个串能否安全地当作 git tag 使用。
//
// 它会被拼进 `git checkout refs/tags/<tag>` 与 docker 镜像名，所以只允许
// 版本形状的字符集：没有空格、没有 `;`、没有 `$`、没有 `/`。
func ValidTag(tag string) bool { return ValidVersion(tag) }

// Compare 比较两个版本：a>b 返回 1，相等 0，a<b 返回 -1。
// 任一方不是合法版本时 ok=false（调用方必须把这种情况当作"不知道"，不能当作"相等"）。
func Compare(a, b string) (int, bool) {
	va, ok := ParseVersion(a)
	if !ok {
		return 0, false
	}
	vb, ok := ParseVersion(b)
	if !ok {
		return 0, false
	}
	as := [3]int64{va.Major, va.Minor, va.Patch}
	bs := [3]int64{vb.Major, vb.Minor, vb.Patch}
	for i := 0; i < 3; i++ {
		switch {
		case as[i] > bs[i]:
			return 1, true
		case as[i] < bs[i]:
			return -1, true
		}
	}
	return 0, true
}

// IsNewer 报告 latest 是否比 current 新。
// comparable=false 表示"至少有一方不是版本号"（例如本地构建的 dev），
// 此时**不能**当作"没有更新"，只能当作"判断不了"。
func IsNewer(latest, current string) (newer bool, comparable bool) {
	c, ok := Compare(latest, current)
	if !ok {
		return false, false
	}
	return c > 0, true
}
