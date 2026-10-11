// Package crypt 提供口令哈希与校验能力（阶段 0 安全止血）。
//
// 设计要点：
//   - 新口令一律使用 bcrypt（带随机盐），彻底替换原来的无盐 MD5；
//   - 兼容历史数据：老库存的是 32 位十六进制 MD5，登录时仍能校验成功，
//     但返回 needRehash=true，由调用方在登录成功后透明升级为 bcrypt，
//     存量账号因此无需强制改密、也不会被锁死；
//   - 不提供、也不接受任何"默认口令"。
package crypt

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const (
	// DefaultCost 是 bcrypt 代价因子。12 在常见服务器上约 200~300ms，
	// 对在线爆破与离线字典攻击都有足够成本。
	DefaultCost = 12
	// MinPasswordLength 最小口令长度。
	MinPasswordLength = 8
	// MaxPasswordLength bcrypt 只使用前 72 字节，超出部分会被静默丢弃，
	// 因此显式拒绝而不是让用户以为长口令更安全。
	MaxPasswordLength = 72
)

var (
	// ErrEmptyPassword 空口令。
	ErrEmptyPassword = errors.New("password must not be empty")
	// ErrPasswordTooShort 口令过短。
	ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	// ErrPasswordTooLong 口令过长。
	ErrPasswordTooLong = fmt.Errorf("password must be at most %d bytes", MaxPasswordLength)
	// ErrWeakPassword 命中弱口令/历史默认口令黑名单。
	ErrWeakPassword = errors.New("password is too weak or is a known default password")
	// ErrUnsupportedHash 无法识别的口令哈希格式。
	ErrUnsupportedHash = errors.New("unsupported password hash format")
)

var legacyMD5Pattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// weakPasswords 是明确拒绝的口令集合，包含本项目历史硬编码默认口令
// （旧版 initUser 使用的 root/123456）以及常见弱口令。
var weakPasswords = map[string]struct{}{
	"123456":        {},
	"12345678":      {},
	"123456789":     {},
	"admin":         {},
	"admin123":      {},
	"password":      {},
	"password1":     {},
	"crony":         {},
	"crony123":      {},
	"root":          {},
	"qwerty":        {},
	"abc123":        {},
	"111111":        {},
	"000000":        {},
	"iloveyou":      {},
	"letmein":       {},
	"changeme":      {},
	"P@ssw0rd":      {},
	"passw0rd":      {},
	"administrator": {},
}

// ValidatePasswordStrength 校验口令强度。用于注册与改密，
// 校验失败必须拒绝写入，不能只打印告警。
func ValidatePasswordStrength(password string) error {
	if password == "" {
		return ErrEmptyPassword
	}
	if len([]byte(password)) > MaxPasswordLength {
		return ErrPasswordTooLong
	}
	// 先判弱口令黑名单，再判长度：像 "123456"/"admin" 这类历史默认口令
	// 应当给出"弱口令/默认口令"这个更准确的拒绝原因。
	if _, weak := weakPasswords[strings.ToLower(password)]; weak {
		return ErrWeakPassword
	}
	if len([]rune(password)) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	return nil
}

// HashPassword 使用 bcrypt 生成口令哈希。
func HashPassword(password string) (string, error) {
	if err := ValidatePasswordStrength(password); err != nil {
		return "", err
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hashed), nil
}

// IsLegacyHash 判断哈希是否为历史遗留的无盐 MD5（32 位十六进制）。
func IsLegacyHash(hash string) bool {
	return legacyMD5Pattern.MatchString(strings.TrimSpace(hash))
}

// LegacyMD5 计算历史格式的 MD5 哈希，仅用于兼容校验与存量数据迁移测试。
//
// Deprecated: 不要用于新的口令存储。
func LegacyMD5(password string) string {
	sum := md5.Sum([]byte(password))
	return hex.EncodeToString(sum[:])
}

// VerifyPassword 校验口令。
//
// 返回值：
//   - ok：口令是否正确；
//   - needRehash：口令正确但存储格式是遗留 MD5，调用方应升级为 bcrypt。
func VerifyPassword(hash, password string) (ok bool, needRehash bool) {
	hash = strings.TrimSpace(hash)
	if hash == "" || password == "" {
		return false, false
	}
	if IsLegacyHash(hash) {
		expected := LegacyMD5(password)
		// 使用常量时间比较，避免时序侧信道。
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(hash)), []byte(expected)) == 1 {
			return true, true
		}
		return false, false
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return false, false
	}
	return true, false
}
