// Package zcrypto 实现 ZCode credentials 的 enc:v1 加解密。
//
// 逆向自 ZCode app.asar：
//   - 算法：aes-256-gcm
//   - 格式：enc:v1:<nonce_base64url>.<authTag_base64url>.<cipherText_base64url>
//   - key：sha256(secret)
//   - secret：优先 ZCODE_CREDENTIAL_SECRET 环境变量，否则
//     zcode-credential-fallback:<platform>:<homedir>:<username>（机器绑定）
//
// macOS 上 platform=darwin，已在真实环境验证可解密官方客户端生成的密文。
package zcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"runtime"
	"strings"
)

const (
	Prefix    = "enc:v1:"
	nonceSize = 12
	tagSize   = 16 // Node.js aes-256-gcm 默认 authTag 长度
)

// DefaultSecret 返回与 ZCode 客户端一致的机器绑定密钥材料。
func DefaultSecret() string {
	if v := os.Getenv("ZCODE_CREDENTIAL_SECRET"); v != "" {
		return v
	}
	username := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		username = u.Username
	}
	home, _ := os.UserHomeDir()
	return fmt.Sprintf("zcode-credential-fallback:%s:%s:%s", runtime.GOOS, home, username)
}

func deriveKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

func IsEncrypted(v string) bool { return strings.HasPrefix(v, Prefix) }

// B64URLDecode 兼容带/不带 padding 的 base64url。
func B64URLDecode(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

func b64urlEncode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Decrypt 解密 enc:v1 密文；非密文原样返回。
func Decrypt(value, secret string) (string, error) {
	if !IsEncrypted(value) {
		return value, nil
	}
	parts := strings.Split(strings.TrimPrefix(value, Prefix), ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("enc:v1 格式不正确")
	}
	nonce, err := B64URLDecode(parts[0])
	if err != nil {
		return "", fmt.Errorf("nonce 解码失败: %w", err)
	}
	tag, err := B64URLDecode(parts[1])
	if err != nil {
		return "", fmt.Errorf("authTag 解码失败: %w", err)
	}
	ciphertext, err := B64URLDecode(parts[2])
	if err != nil {
		return "", fmt.Errorf("cipherText 解码失败: %w", err)
	}
	gcm, err := newGCM(secret)
	if err != nil {
		return "", err
	}
	// Go 的 GCM Open 要求 ciphertext||tag 拼接
	plain, err := gcm.Open(nil, nonce, append(ciphertext, tag...), nil)
	if err != nil {
		return "", fmt.Errorf("解密失败（密钥派生规则可能已变更）: %w", err)
	}
	return string(plain), nil
}

// Encrypt 加密为 enc:v1 格式。
func Encrypt(plain, secret string) (string, error) {
	gcm, err := newGCM(secret)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(plain), nil)
	ct, tag := sealed[:len(sealed)-tagSize], sealed[len(sealed)-tagSize:]
	return Prefix + b64urlEncode(nonce) + "." + b64urlEncode(tag) + "." + b64urlEncode(ct), nil
}

// DecryptJSON 解密并解析 JSON，失败返回 nil。
func DecryptJSON(value, secret string) map[string]any {
	plain, err := Decrypt(value, secret)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(plain), &m); err != nil {
		return nil
	}
	return m
}

func newGCM(secret string) (cipher.AEAD, error) {
	block, err := aes.NewCipher(deriveKey(secret))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
