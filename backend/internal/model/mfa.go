package model

import "time"

// UserMFA 是用户的 TOTP 二次验证配置（一人一行）。
type UserMFA struct {
	// UID 既是主键也是归属：一个用户只有一套 TOTP 密钥。
	UID uint64 `gorm:"column:uid;primaryKey;autoIncrement:false"`
	// SecretEnc 是 crypto.Encrypt 后的密文。
	//
	// **必须加密**：TOTP 密钥等同于「第二因素的私钥」，
	// 明文落库时任何一个只读数据库账号都能凭空生成有效动态码。
	SecretEnc string `gorm:"column:secret_enc;type:text;not null"`
	// Enabled 为 false 表示「已生成密钥但还没验证过一次」，此时不能用于登录。
	Enabled   bool       `gorm:"column:enabled;not null;default:false"`
	CreatedAt time.Time  `gorm:"type:timestamptz;not null"`
	EnabledAt *time.Time `gorm:"type:timestamptz"`
}

func (UserMFA) TableName() string { return "user_mfa" }

// UserMFABackupCode 是一次性备份码。
//
// 存 sha256(uid|code) 而不是 bcrypt：备份码是 8 位 base32（40 bit 熵，且带 uid 盐），
// 一次性使用，逐条 bcrypt 会让「用完 10 个码」这种极端情况变慢且无收益。
type UserMFABackupCode struct {
	ID  uint64 `gorm:"primaryKey"`
	UID uint64 `gorm:"column:uid;not null"`
	// CodeHash = hex(sha256(uid|规范化后的码))。
	CodeHash  string     `gorm:"column:code_hash;size:64;not null"`
	UsedAt    *time.Time `gorm:"type:timestamptz"`
	CreatedAt time.Time  `gorm:"type:timestamptz;not null"`
}

func (UserMFABackupCode) TableName() string { return "user_mfa_backup_codes" }
