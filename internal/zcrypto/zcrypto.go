// Package zcrypto 实现 ZCode credentials 的 enc:v1 / enc:v2 加解密。
//
// 逆向自 ZCode app.asar（/out/host/index.js）：
//   - 算法：aes-256-gcm，密文体统一为 <nonce>.<authTag>.<cipherText>（三段 base64url）
//   - enc:v1:<payload>
//     key = sha256(secret)；secret 优先 ZCODE_CREDENTIAL_SECRET，否则机器绑定
//     zcode-credential-fallback:<platform>:<homedir>:<username>。
//   - enc:v2:<source>:<fp24>:<payload>
//     source ∈ {env, os}；fp24 = hex(sha256(key))[:24]，用于校验取到的 key 是否正确。
//     · env：ZCODE_CREDENTIAL_SECRET（key=sha256(secret)），或
//     ZCODE_CREDENTIAL_SECRET_FILE / 默认 <credDir>/credential-secret 文件（key=sha256(content)）。
//     · os：macOS 钥匙串项 service="ZCode Credential Encryption"、
//     account=hex(sha256(credentialsPath))，值为 43 字符 base64url（解码得 32 字节），
//     该 32 字节**直接**作为 AES key（不做 sha256）。
//
// ZCode 3.18.x 加载时会把 v1 记录迁移为 v2:os，故读取必须支持 v2；
// 写入仍产出 v1（机器绑定 secret，无需钥匙串），交由 ZCode 启动时自动迁移。
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
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"zcas/internal/paths"
	"zcas/internal/platform"
)

const (
	PrefixV1 = "enc:v1:"
	PrefixV2 = "enc:v2:"
	// Prefix 兼容旧引用，指向 v1。
	Prefix    = PrefixV1
	nonceSize = 12
	tagSize   = 16 // Node.js aes-256-gcm 默认 authTag 长度

	// keyringService ZCode 写入钥匙串时使用的 service 名（app.asar 常量）。
	keyringService = "ZCode Credential Encryption"
)

// DefaultSecret 返回与 ZCode 客户端一致的 v1 机器绑定密钥材料。
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

// IsEncrypted 判断值是否为 ZCode 密文（v1 或 v2）。
func IsEncrypted(v string) bool {
	return strings.HasPrefix(v, PrefixV1) || strings.HasPrefix(v, PrefixV2)
}

// B64URLDecode 兼容带/不带 padding 的 base64url。
func B64URLDecode(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

func b64urlEncode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Decrypt 解密 enc:v1 / enc:v2 密文；非密文原样返回。
// secret 仅用于 v1（v2 自带密钥来源，忽略该参数）。
func Decrypt(value, secret string) (string, error) {
	switch {
	case strings.HasPrefix(value, PrefixV2):
		return decryptV2(value)
	case strings.HasPrefix(value, PrefixV1):
		return openPayload(strings.TrimPrefix(value, PrefixV1), deriveKey(secret))
	default:
		return value, nil
	}
}

// Encrypt 加密为 enc:v1 格式（ZCode 启动时会自动迁移为 v2）。
func Encrypt(plain, secret string) (string, error) {
	gcm, err := newGCM(deriveKey(secret))
	if err != nil {
		return "", err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(plain), nil)
	ct, tag := sealed[:len(sealed)-tagSize], sealed[len(sealed)-tagSize:]
	return PrefixV1 + b64urlEncode(nonce) + "." + b64urlEncode(tag) + "." + b64urlEncode(ct), nil
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

// ===== enc:v2 =====

func decryptV2(value string) (string, error) {
	parts := strings.SplitN(strings.TrimPrefix(value, PrefixV2), ":", 3)
	if len(parts) != 3 {
		return "", fmt.Errorf("enc:v2 格式不正确")
	}
	source, fp, payload := parts[0], parts[1], parts[2]
	key, err := v2Key(source)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(fp, fingerprint24(key)) {
		return "", fmt.Errorf("enc:v2 密钥指纹不匹配（source=%s）", source)
	}
	return openPayload(payload, key)
}

// fingerprint24 = hex(sha256(key))[:24]，与 ZCode 一致。
func fingerprint24(key []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(key))[:24]
}

// v2Key 按来源解析 enc:v2 的 AES-256 密钥。
func v2Key(source string) ([]byte, error) {
	switch source {
	case "env":
		return envKey()
	case "os":
		return osKey()
	default:
		return nil, fmt.Errorf("enc:v2 未知密钥来源 %q", source)
	}
}

// envKey 复刻 ZCode resolveManagedSecret：环境变量 secret，或 credential-secret 文件。
func envKey() ([]byte, error) {
	if v := os.Getenv("ZCODE_CREDENTIAL_SECRET"); v != "" {
		return deriveKey(v), nil
	}
	file := os.Getenv("ZCODE_CREDENTIAL_SECRET_FILE")
	if file == "" {
		file = filepath.Join(filepath.Dir(paths.CredentialsFile()), "credential-secret")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("读取 credential-secret 失败: %w", err)
	}
	if len(b) < 32 || len(b) > 4096 {
		return nil, fmt.Errorf("credential-secret 长度非法(%d 字节)", len(b))
	}
	return deriveKey(string(b)), nil
}

var (
	osKeyOnce sync.Once
	osKeyVal  []byte
	osKeyErr  error
)

// osKey 读取钥匙串密钥并缓存（进程内密钥不变，避免重复 exec security）。
func osKey() ([]byte, error) {
	osKeyOnce.Do(func() { osKeyVal, osKeyErr = readOSKey() })
	return osKeyVal, osKeyErr
}

func readOSKey() ([]byte, error) {
	credPath := paths.CredentialsFile()
	account := fmt.Sprintf("%x", sha256.Sum256([]byte(credPath)))
	val, err := platform.ReadKeychainSecret(keyringService, account)
	if err != nil {
		return nil, err
	}
	key, err := B64URLDecode(strings.TrimSpace(val))
	if err != nil {
		return nil, fmt.Errorf("钥匙串密钥解码失败: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("钥匙串密钥长度非法(%d 字节)", len(key))
	}
	return key, nil
}

// ===== 通用 aes-256-gcm =====

// openPayload 解出 <nonce>.<authTag>.<cipherText> 三段式 base64url 密文。
func openPayload(payload string, key []byte) (string, error) {
	parts := strings.Split(payload, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("密文格式不正确")
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
	gcm, err := newGCM(key)
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

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
