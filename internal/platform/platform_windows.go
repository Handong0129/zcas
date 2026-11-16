//go:build windows

// Package platform Windows 平台实现。
//
// 进程管理用系统自带的 tasklist/taskkill；启动路径从 zcode:// 协议注册项解析
// （不硬编码安装目录，兼容 Program Files / Program Files (x86) / 按用户安装）。
// 凭据密钥：enc:v1 用机器绑定密钥（zcrypto.DefaultSecret）；enc:v2:os 的密钥在
// Windows 凭据管理器（见 ReadKeychainSecret，ZCode 3.18.2+ 默认格式）。
package platform

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// hidExec 构造隐藏控制台的命令。GUI 进程派生控制台子进程时若不设
// CREATE_NO_WINDOW，Windows 会为子进程新开一个一闪而过的黑框；
// 本文件所有子进程（tasklist/reg/powershell/cmd 等）都必须走这里。
func hidExec(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	return cmd
}

// ProcessName Windows 上 ZCode 主进程与各子进程共用的映像名。
const ProcessName = "ZCode.exe"

var (
	ErrZCodeNotRunning = errors.New("ZCode 未在运行")
	ErrUnsupported     = errors.New("Windows 暂不支持读取 OS 凭据库（ZCode 在 Windows 使用 enc:v1）")
)

// zcodeExeRe 从注册表命令值里提取第一个被引号包裹的 .exe 路径。
var zcodeExeRe = regexp.MustCompile(`"([^"]+\.exe)"`)

// IsZCodeRunning 用 tasklist 按映像名过滤；输出含 ZCode.exe 即在运行
// （ZCode 关窗会驻留托盘，托盘内退出后此处才会翻转为未运行，与用户预期一致）。
// 不看退出码、不依赖本地化提示文案（无匹配时 tasklist 仍返回 0 并打印本地化提示）。
func IsZCodeRunning() bool {
	out, err := hidExec("tasklist", "/FI", "IMAGENAME eq "+ProcessName, "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), strings.ToLower(ProcessName))
}

// KillZCode 先尝试优雅关闭（taskkill 发 WM_CLOSE），超时后 /F 强制，再等确认。
// /T 连带子进程一起结束（Electron 在 Windows 上多进程同名）。
func KillZCode(wait time.Duration) error {
	if !IsZCodeRunning() {
		return nil
	}
	_ = hidExec("taskkill", "/IM", ProcessName, "/T").Run()
	if waitGone(wait) {
		return nil
	}
	_ = hidExec("taskkill", "/IM", ProcessName, "/T", "/F").Run()
	if waitGone(3 * time.Second) {
		return nil
	}
	return errors.New("关闭 ZCode 超时，请手动退出 ZCode 后重试")
}

func waitGone(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !IsZCodeRunning() {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return !IsZCodeRunning()
}

// LaunchZCode 解析 zcode:// 协议注册项得到 exe 路径后启动。
// 用 cmd start 派生独立进程（首个引号参数是窗口标题，故先给空标题）。
func LaunchZCode() error {
	exe, err := ZCodeExePath()
	if err != nil {
		return err
	}
	return hidExec("cmd", "/C", "start", "", exe).Run()
}

// zcodeExePath 依次尝试注册表协议项与常见安装目录，返回 ZCode.exe 路径。
// ZCodeExePath 返回已安装 ZCode.exe 的路径（协议归还等外部使用者也要用它）。
// 会跳过指向 zcas 自身的注册：OAuth 接管期间 HKCU 的 zcode:// 指向我们自己。
func ZCodeExePath() (string, error) {
	self, _ := os.Executable()
	for _, root := range []string{
		`HKCU\Software\Classes\zcode\shell\open\command`,
		`HKLM\Software\Classes\zcode\shell\open\command`,
		`HKLM\Software\WOW6432Node\Classes\zcode\shell\open\command`,
	} {
		out, err := hidExec("reg", "query", root).Output()
		if err != nil {
			continue
		}
		if m := zcodeExeRe.FindStringSubmatch(string(out)); m != nil {
			if !strings.EqualFold(filepath.Clean(m[1]), filepath.Clean(self)) {
				return m[1], nil
			}
			continue
		}
	}
	for _, p := range []string{
		`C:\Program Files\ZCode\ZCode.exe`,
		`C:\Program Files (x86)\ZCode\ZCode.exe`,
	} {
		if fileExists(p) {
			return p, nil
		}
	}
	return "", errors.New("未找到 ZCode 安装路径（注册表 zcode:// 协议项与常见目录均未命中）")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// OpenURL 用系统默认浏览器打开 URL。
func OpenURL(url string) error {
	return hidExec("rundll32", "url.dll,FileProtocolHandler", url).Run()
}

var cachedZCodeVersion string

// ZCodeVersion 读取 ZCode.exe 的产品版本（billing 的 app_version 参数应与真实客户端一致）。
// 取不到则返回空，由 config.BillingAppVersion 兜底。
func ZCodeVersion() string {
	if cachedZCodeVersion != "" {
		return cachedZCodeVersion
	}
	exe, err := ZCodeExePath()
	if err != nil {
		return ""
	}
	out, err := hidExec("powershell", "-NoProfile", "-Command",
		fmt.Sprintf("(Get-Item -LiteralPath '%s').VersionInfo.ProductVersion", exe)).Output()
	if err == nil {
		cachedZCodeVersion = strings.TrimSpace(string(out))
	}
	return cachedZCodeVersion
}

var cachedOSVersion string

// OSVersion 返回 Windows 版本号（X-Os-Version 头用），形如 10.0.19045（对齐 Node os.release()）。
func OSVersion() string {
	if cachedOSVersion != "" {
		return cachedOSVersion
	}
	out, err := hidExec("powershell", "-NoProfile", "-Command",
		`$v=[Environment]::OSVersion.Version; '{0}.{1}.{2}' -f $v.Major,$v.Minor,$v.Build`).Output()
	if err == nil {
		cachedOSVersion = strings.TrimSpace(string(out))
	}
	return cachedOSVersion
}

// ReadKeychainSecret 读取 Windows 凭据管理器里的通用凭据（ZCode 3.18.2+ 的 enc:v2:os
// 密钥存放处，服务名与 macOS 钥匙串一致）。Electron 侧的写入格式没有公开文档，
// 按常见 keytar 形态依次尝试：TargetName=service、service@account、service/account，
// 命中后校验 UserName 是否为 account（ZCode 用凭据文件路径的 sha256 当 account）。
func ReadKeychainSecret(service, account string) (string, error) {
	for _, target := range []string{service, service + "@" + account, service + "/" + account} {
		if blob, user, ok := credRead(target); ok {
			if user != "" && user != account {
				continue
			}
			if v := strings.TrimSpace(decodeCredBlob(blob)); v != "" {
				return v, nil
			}
		}
	}
	// 兜底：枚举通用凭据按服务名/account 匹配（覆盖 keyring-rs 的 "account.service" 等
	// 其他 TargetName 形态）。UserName 或 TargetName 含 account 的优先，其次唯一的服务名命中。
	var byService []string
	for _, c := range credEnumerate() {
		v := strings.TrimSpace(decodeCredBlob(c.blob))
		if v == "" || !strings.Contains(strings.ToLower(c.target), strings.ToLower(service)) {
			continue
		}
		if c.user == account || strings.Contains(c.target, account) {
			return v, nil
		}
		byService = append(byService, v)
	}
	if len(byService) == 1 {
		return byService[0], nil
	}
	return "", fmt.Errorf("凭据管理器中未找到 %s 的密钥（enc:v2:os）", service)
}

// decodeCredBlob 凭据内容转字符串：keytar 写 UTF-8，keyring-rs 写 UTF-16LE（奇数位全 0）。
func decodeCredBlob(b []byte) string {
	if len(b) >= 2 && len(b)%2 == 0 {
		utf16le := true
		for i := 1; i < len(b); i += 2 {
			if b[i] != 0 {
				utf16le = false
				break
			}
		}
		if utf16le {
			u := make([]uint16, len(b)/2)
			for i := range u {
				u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
			}
			return windows.UTF16ToString(u)
		}
	}
	return string(b)
}

type credEntry struct {
	target, user string
	blob         []byte
}

// credEnumerate 列出当前用户的全部通用凭据（CredEnumerateW，失败返回空）。
func credEnumerate() []credEntry {
	var count uint32
	var list **winCredential
	ret, _, _ := procCredEnumerateW.Call(0, 0,
		uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&list)))
	if ret == 0 || list == nil {
		return nil
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(list)))
	var out []credEntry
	for _, p := range unsafe.Slice(list, count) {
		if p == nil || p.Type != 1 { // 仅 CRED_TYPE_GENERIC
			continue
		}
		e := credEntry{target: windows.UTF16PtrToString(p.TargetName)}
		if p.UserName != nil {
			e.user = windows.UTF16PtrToString(p.UserName)
		}
		if p.CredentialBlobSize > 0 && p.CredentialBlob != nil {
			e.blob = append([]byte(nil), unsafe.Slice(p.CredentialBlob, p.CredentialBlobSize)...)
		}
		out = append(out, e)
	}
	return out
}

// windows FILETIME / CREDENTIAL 布局（advapi32 CredReadW）。
type winFiletime struct{ Low, High uint32 }

type winCredential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        winFiletime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

var (
	modadvapi32        = windows.NewLazySystemDLL("advapi32.dll")
	procCredReadW      = modadvapi32.NewProc("CredReadW")
	procCredEnumerateW = modadvapi32.NewProc("CredEnumerateW")
	procCredFree       = modadvapi32.NewProc("CredFree")
)

// credRead 读取通用凭据（type=1 CRED_TYPE_GENERIC），返回密文 blob 与 UserName。
func credRead(target string) (blob []byte, userName string, ok bool) {
	tp, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return nil, "", false
	}
	var p *winCredential
	ret, _, _ := procCredReadW.Call(
		uintptr(unsafe.Pointer(tp)),
		1, // CRED_TYPE_GENERIC
		0,
		uintptr(unsafe.Pointer(unsafe.Pointer(&p))),
	)
	if ret == 0 || p == nil {
		return nil, "", false
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(p)))
	if p.CredentialBlobSize > 0 && p.CredentialBlob != nil {
		blob = make([]byte, p.CredentialBlobSize)
		copy(blob, unsafe.Slice(p.CredentialBlob, p.CredentialBlobSize))
	}
	return blob, windows.UTF16PtrToString(p.UserName), true
}

// SetURLHandler 把 URL scheme 的打开方式（HKCU）指向指定 exe（命令形如 `"exe" "%1"`）。
// 只写 HKCU：用户级覆盖优先于 HKLM，删除 HKCU 键即回落到系统级注册。
func SetURLHandler(scheme, exe string) error {
	key := `HKCU\Software\Classes\` + scheme
	cmd := `"` + exe + `" "%1"`
	// Windows 11 24H2 的「共享宿主」值会让 Shell 忽略传统 command 改弹应用选择框，
	// 接管前先清掉（值不存在时静默失败，无害）
	_ = hidExec("reg", "delete", key, "/v", "SharedHostApplication", "/f").Run()
	for _, args := range [][]string{
		{"reg", "add", key, "/ve", "/d", "URL:" + scheme + " Protocol", "/f"},
		{"reg", "add", key, "/v", "URL Protocol", "/d", "", "/f"},
		{"reg", "add", key + `\shell\open\command`, "/ve", "/d", cmd, "/f"},
	} {
		if out, err := hidExec(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("reg add 失败: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	// 现代协议选择框的候选来自 Default Programs 注册（RegisteredApplications +
	// Capabilities\UrlAssociations，Zoom/Slack/Parallels 都是这套）；
	// 只写 Classes 下的传统/SupportedProtocols 注册都进不了候选列表（VM 实测）
	appKey := `HKCU\Software\Classes\Applications\` + filepath.Base(exe)
	for _, args := range [][]string{
		{"reg", "add", appKey, "/ve", "/d", "zcas", "/f"},
		{"reg", "add", appKey + `\SupportedProtocols\` + scheme, "/ve", "/d", "", "/f"},
		{"reg", "add", appKey + `\DefaultIcon`, "/ve", "/d", exe + ",0", "/f"},
		{"reg", "add", appKey + `\shell\open\command`, "/ve", "/d", cmd, "/f"},
		{"reg", "add", `HKCU\Software\RegisteredApplications`, "/v", "zcas", "/d", `Software\zcas\Capabilities`, "/f"},
		{"reg", "add", `HKCU\Software\zcas\Capabilities`, "/v", "ApplicationName", "/d", "zcas", "/f"},
		{"reg", "add", `HKCU\Software\zcas\Capabilities`, "/v", "ApplicationIcon", "/d", exe + ",0", "/f"},
		{"reg", "add", `HKCU\Software\zcas\Capabilities\UrlAssociations`, "/v", scheme, "/d", filepath.Base(exe), "/f"},
	} {
		if out, err := hidExec(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("reg add 失败: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// SetURLHandlerCommand 把 scheme 的打开命令写为完整命令串（含参数/引号），
// 用于归还接管前暂存的原命令（原命令可能带额外参数，不能按纯 exe 重构）。
func SetURLHandlerCommand(scheme, command string) error {
	key := `HKCU\Software\Classes\` + scheme
	if out, err := hidExec("reg", "add", key+`\shell\open\command`, "/ve", "/t", "REG_SZ", "/d", command, "/f").CombinedOutput(); err != nil {
		return fmt.Errorf("reg add 失败: %v: %s", err, strings.TrimSpace(string(out)))
	}
	removeModernRegistration(scheme)
	return nil
}

// removeModernRegistration 删除我们写入的新模型 scheme 注册（归还时用，
// 避免我们继续留在「选择应用」候选里抢走 ZCode 自己的回调）。
func removeModernRegistration(scheme string) {
	if self, err := os.Executable(); err == nil {
		appKey := `HKCU\Software\Classes\Applications\` + filepath.Base(self)
		_ = hidExec("reg", "delete", appKey, "/f").Run()
	}
	_ = hidExec("reg", "delete", `HKCU\Software\zcas`, "/f").Run()
	_ = hidExec("reg", "delete", `HKCU\Software\RegisteredApplications`, "/v", "zcas", "/f").Run()
}

// DeleteURLHandler 删除用户级 scheme 注册（回落到系统级/其他应用的注册）。
func DeleteURLHandler(scheme string) {
	_ = hidExec("reg", "delete", `HKCU\Software\Classes\`+scheme, "/f").Run()
	removeModernRegistration(scheme)
}

// GetURLHandlerCommand 返回 scheme 当前用户级注册的打开命令（无注册返回空串）。
func GetURLHandlerCommand(scheme string) string {
	out, err := hidExec("reg", "query",
		`HKCU\Software\Classes\`+scheme+`\shell\open\command`, "/ve").Output()
	if err != nil {
		return ""
	}
	// 输出形如两行，取 REG_SZ 行的值部分
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.Index(line, "REG_SZ"); i >= 0 {
			return strings.TrimSpace(line[i+len("REG_SZ"):])
		}
	}
	return ""
}
