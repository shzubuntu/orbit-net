package account

import (
	"crypto/rand"
	"encoding/hex"

	"golang.org/x/crypto/bcrypt"
)

// randID 生成随机 hex ID(账户/设备 ID,避免可枚举)。
func randID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "x" + hex.EncodeToString(b) // rand 失败时 b 全零,兜底不返回空串
	}
	return hex.EncodeToString(b)
}

// HashToken bcrypt 哈希设备 token。
// 公网场景设备每次重连都做一次比对的在热路径上,用 MinCost 兼顾速度。
func HashToken(token string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.MinCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// VerifyToken 校验明文 token 与存储的 bcrypt 哈希。
func VerifyToken(hash, token string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(token)) == nil
}

// NewToken 生成 32 字节 hex 随机 token(一次性返回给用户,库中只存哈希)。
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
