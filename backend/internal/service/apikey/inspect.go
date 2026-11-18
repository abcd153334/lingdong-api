package apikey

import (
	"errors"
	"fmt"

	"gorm.io/gorm"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// Inspect 只回答一个问题：**这把明文对应哪一行**。它不判断这把钥能不能用。
//
// 存在的理由只有一个 —— 自检端点（GET /api/user/key/health）要回答
// "我为什么用不了"，而"用不了"的钥在 Authenticate 里就已经被拒了。
// 若自检也走 Authenticate，它只会在一切正常时可访问：三个诊断字段
// （status / disabled_reason / allow_ips）结构上永远是"正常"值，
// 等于挂着诊断的名义、拿不到诊断的信息。
//
// 与 Authenticate 的边界（这不是第二套判定）：
//   - 只做哈希比对（主钥 → 上一代），不判 status、不判有效期、不判 IP；
//   - 不续期、不刷新 last_used_at —— 被禁的钥不该因为"被看了一眼"而续命；
//   - **不构成任何授权**：没有任何数据端点用它放行。
//
// 安全上不扩大暴露面：调用方已经持有明文，而 401 的 error_code 本来就会
// 告诉它同样的结论（被禁 / 过期 / 已退役 / IP 不符）。这里只是把它一次说清。
func Inspect(plain string) (*model.APIKey, error) {
	h := Hash(plain)
	var k model.APIKey
	err := db.DB.Where("key_hash = ?", h).First(&k).Error
	if err == nil {
		return &k, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	// 上一代明文：接入方在重叠窗口里可能正拿着它，自检必须能诊断这一代。
	if err := db.DB.Where("prev_key_hash = ?", h).First(&k).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrKeyGone
		}
		return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	return &k, nil
}
