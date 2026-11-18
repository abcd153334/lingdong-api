package conf

import (
	"fmt"
	"os"
	"strings"
)

// sensitiveEnvKeys 是「加载进 Env 之后就该从进程环境里消失」的变量。
//
// 环境变量对同 UID 的任意进程都可读（Linux /proc/<pid>/environ），插件子进程
// 默认还会整份继承。密钥只在启动瞬间需要一次，之后一律从 EnvConfig() 取。
var sensitiveEnvKeys = []string{
	"DATABASE_URL",
	"JWT_SECRET",
	"APP_ENCRYPTION_KEY",
	"REDIS_PASSWORD",
}

// UnsetSensitiveEnv 清除敏感环境变量；必须在 Load() 之后调用（Load 之后所有消费者读 EnvConfig()）。
func UnsetSensitiveEnv() {
	for _, k := range sensitiveEnvKeys {
		_ = os.Unsetenv(k)
	}
}

// SensitiveEnvKeys 返回敏感键名（只读副本，供测试与启动自检使用）。
func SensitiveEnvKeys() []string {
	out := make([]string, len(sensitiveEnvKeys))
	copy(out, sensitiveEnvKeys)
	return out
}

// publicSecretValues 是**在本仓库里公开过的**示例密钥。
//
// 它们出现在 docker-compose.yml / .env.example / 开发脚本里，任何读过源码的人
// 都能拿它签名。留作默认值等于「部署即失陷」：
//   - JWT_SECRET 空或公开 → 可离线自签 HS256，伪造 uid=1 即超级管理员
//     （鉴权只按 uid 查库、role 取 DB，不需要知道任何口令）；
//   - APP_ENCRYPTION_KEY 公开 → users 的 API KEY 密文、渠道密钥、/v1/media 能力
//     链接的派生密钥全部可被还原或伪造。
//
// 所以这里不是"建议"，是启动闸门：生产环境命中这些值直接拒绝启动。
var publicSecretValues = map[string]string{
	"dev-only-change-me":               "JWT_SECRET",
	"0123456789abcdef0123456789abcdef": "APP_ENCRYPTION_KEY",
}

// minJWTSecretLen 是 HS256 建议的最小密钥长度（RFC 7518 §3.2：≥ 摘要长度 256 bit）。
const minJWTSecretLen = 32

// ValidateSecrets 校验「一旦泄露就等于交出全站」的密钥，以及只在测试环境允许的后门。
//
// dev/test 只返回警告（本地起服务不该被强口令卡住），其余环境一律拒绝启动 ——
// 带病启动的表现是"一切正常，但任何人都能当管理员"，比启动失败危险得多。
func ValidateSecrets(e Env) (warnings []string, err error) {
	// APP_ENV 必须先显式声明：它决定 Cookie Secure、CSRF 来源放宽、示例密钥是否放行。
	// 给默认值等于「忘了配 = 自动获得 dev 的全部放宽」，这是最容易被忽略的一类失陷。
	if strings.TrimSpace(e.AppEnv) == "" {
		return nil, fmt.Errorf("APP_ENV 未配置：必须显式声明 dev/test/prod（缺省不再视为 dev）")
	}

	devLike := e.AppEnv == "dev" || e.AppEnv == "test"

	// 测试固定验证码 = 万能答案。生产出现它等于人机验证被关掉。
	if !devLike && strings.TrimSpace(e.CaptchaTestCode) != "" {
		return nil, fmt.Errorf("LD_CAPTCHA_TEST_CODE 只能用于 dev/test：生产设置它等于关闭人机验证")
	}

	// CORS 白名单含 * 时浏览器会拒绝 credentials，而"回显任意 Origin"等于放弃 CSRF 防护。
	if !devLike && strings.Contains(e.AllowedOrigins, "*") {
		return nil, fmt.Errorf("LD_ALLOWED_ORIGINS 不允许包含 *（携带凭证的跨域请求不能使用通配来源）")
	}

	if devLike {
		if _, bad := publicSecretValues[e.AppEncryptionKey]; bad || strings.TrimSpace(e.AppEncryptionKey) == "" {
			warnings = append(warnings, "APP_ENCRYPTION_KEY 是公开示例值：库里的 API KEY / 渠道密钥可被还原")
		}
		if strings.Contains(e.AllowedOrigins, "*") {
			warnings = append(warnings, "LD_ALLOWED_ORIGINS 含 *：浏览器会拒绝携带凭证的跨域请求")
		}
		return warnings, nil
	}

	switch {
	case strings.TrimSpace(e.AppEncryptionKey) == "":
		return nil, fmt.Errorf("APP_ENCRYPTION_KEY 未配置：拒绝启动（空密钥无法保护库内密文）")
	case isPublicSecret(e.AppEncryptionKey):
		return nil, fmt.Errorf("APP_ENCRYPTION_KEY 仍是公开示例值：拒绝启动，请改用 openssl rand -hex 32 生成")
	}
	return nil, nil
}

func isPublicSecret(v string) bool {
	_, bad := publicSecretValues[strings.TrimSpace(v)]
	return bad
}
