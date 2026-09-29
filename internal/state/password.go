package state

// 口令哈希封装（v0.3 W1，RBAC 设计 §2.1）：argon2id PHC 串编码/解析/校验。
// 写侧参数冻结为 m=64MiB、t=2、p=1、keyLen=32、salt 16B 随机；校验侧按
// 存量串内参数重导出（日后参数上调不破坏旧口令），比对走
// subtle.ConstantTimeCompare（常量时间）。PHC 形态：
//
//	$argon2id$v=19$m=65536,t=2,p=1$<salt-b64>$<hash-b64>
//
// （base64 RawStd——无填充标准字母表，PHC 惯例。）明文口令与哈希串都不入
// 日志/事件/审计（secret 纪律，state-model §2.9）。

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id 写侧冻结参数（RBAC 设计 §2.1）；读侧参数取自串内（见
// VerifyPassword），本组仅约束新哈希。
const (
	passwordMemoryKiB = 64 * 1024 // 64 MiB
	passwordTimeCost  = 2
	passwordThreads   = 1
	passwordKeyLen    = 32
	passwordSaltLen   = 16
)

// ErrPasswordHashValid 表示口令哈希串不是合法的 argon2id PHC 形态（字段
// 缺失/版本不符/参数残缺/base64 破损——篡改或非本平台产出的串）。
var ErrPasswordHashInvalid = errors.New("password hash malformed")

// HashPassword 以冻结参数派生 argon2id 并编码为 PHC 串（存储形态）。
func HashPassword(password string) (string, error) {
	salt := make([]byte, passwordSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("state: read password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, passwordTimeCost, passwordMemoryKiB, passwordThreads, passwordKeyLen)
	return encodePasswordPHC(salt, key), nil
}

// VerifyPassword 按存量 PHC 串的参数重导出并常量时间比对：匹配返回
// (true, nil)；不匹配 (false, nil)；串非法 (false, ErrPasswordHashInvalid)。
func VerifyPassword(password, encoded string) (bool, error) {
	salt, want, params, err := decodePasswordPHC(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, params.timeCost, params.memoryKiB, params.threads, uint32(len(want))) //nolint:gosec // G115：len(want) = argon2 摘要长度（32B），恒在 uint32 值域
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, nil
	}
	return true, nil
}

// passwordParams 是 PHC 串内携带的派生参数（读侧口径）。
type passwordParams struct {
	memoryKiB uint32
	timeCost  uint32
	threads   uint8
}

// encodePasswordPHC 以写侧冻结参数编码 PHC 串。
func encodePasswordPHC(salt, key []byte) string {
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, passwordMemoryKiB, passwordTimeCost, passwordThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key))
}

// decodePasswordPHC 解析 PHC 串：算法固定 argon2id、版本必须与本库一致
// （x/crypto/argon2 当前 v=19）；任何残缺/篡改形态一律
// ErrPasswordHashInvalid（fail-closed，不做宽松兜底）。
func decodePasswordPHC(encoded string) (salt, key []byte, params passwordParams, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return nil, nil, passwordParams{}, ErrPasswordHashInvalid
	}
	var version int
	if _, scanErr := fmt.Sscanf(parts[2], "v=%d", &version); scanErr != nil || version != argon2.Version {
		return nil, nil, passwordParams{}, ErrPasswordHashInvalid
	}
	var m, t, p int64
	if n, scanErr := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); scanErr != nil || n != 3 ||
		m <= 0 || t <= 0 || p <= 0 || m > 1<<32-1 || t > 1<<32-1 || p > 255 {
		return nil, nil, passwordParams{}, ErrPasswordHashInvalid
	}
	b64 := base64.RawStdEncoding
	if salt, err = b64.DecodeString(parts[4]); err != nil || len(salt) != passwordSaltLen {
		return nil, nil, passwordParams{}, ErrPasswordHashInvalid
	}
	if key, err = b64.DecodeString(parts[5]); err != nil || len(key) != passwordKeyLen {
		return nil, nil, passwordParams{}, ErrPasswordHashInvalid
	}
	return salt, key, passwordParams{memoryKiB: uint32(m), timeCost: uint32(t), threads: uint8(p)}, nil
}
