package store

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func fakeJWT(userID string) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`))
	p := base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"` + userID + `"}`))
	return h + "." + p + ".fakesig"
}

func liveState() (map[string]any, map[string]any) {
	cred := map[string]any{
		"oauth:active_provider":      "enc:v1:aaa",
		"oauth:bigmodel:access_token": "enc:v1:bbb",
		"zcodejwttoken":              "enc:v1:ccc",
		"unrelated_setting":          "keep-me",
	}
	cfg := map[string]any{
		"provider": map[string]any{
			"builtin:bigmodel-start-plan": map[string]any{
				"enabled": true,
				"options": map[string]any{"apiKey": fakeJWT("111"), "baseURL": "https://x"},
			},
			"custom:my-openrouter": map[string]any{
				"enabled": true,
				"options": map[string]any{"apiKey": "sk-third-party-key"},
			},
		},
		"editor": map[string]any{"fontSize": 14},
	}
	return cred, cfg
}

func TestSliceOnlyTakesAccountFields(t *testing.T) {
	cred, cfg := liveState()
	snap := Slice(cred, cfg)

	if _, ok := snap.CredentialFields["unrelated_setting"]; ok {
		t.Fatal("切片不应包含非账号字段 unrelated_setting")
	}
	for _, k := range []string{"oauth:active_provider", "oauth:bigmodel:access_token", "zcodejwttoken"} {
		if _, ok := snap.CredentialFields[k]; !ok {
			t.Fatalf("切片缺少账号字段 %s", k)
		}
	}
	if _, ok := snap.ProviderSlots["builtin:bigmodel-start-plan"]; !ok {
		t.Fatal("切片应包含明文 JWT 的账号槽位")
	}
	if _, ok := snap.ProviderSlots["custom:my-openrouter"]; ok {
		t.Fatal("切片不应包含第三方 provider 槽位")
	}
}

func TestApplyPreservesNonAccountConfig(t *testing.T) {
	_, cfg := liveState()
	snap := Slice(map[string]any{
		"oauth:active_provider": "enc:v1:xxx",
		"zcodejwttoken":         "enc:v1:yyy",
	}, cfg)

	// 当前登录态：另一个账号的字段 + 用户自配
	curCred := map[string]any{
		"oauth:active_provider": "enc:v1:old",
		"zcodejwttoken":         "enc:v1:old",
		"unrelated_setting":     "keep-me",
	}
	curCfg := map[string]any{
		"provider": map[string]any{
			"builtin:zai": map[string]any{ // 旧账号的槽位（明文 JWT）应被清掉
				"enabled": true,
				"options": map[string]any{"apiKey": fakeJWT("999")},
			},
			"custom:my-openrouter": map[string]any{ // 第三方 key 应保留
				"enabled": true,
				"options": map[string]any{"apiKey": "sk-third-party-key"},
			},
		},
		"editor": map[string]any{"fontSize": 14},
	}

	newCred, newCfg := Apply(snap, curCred, curCfg)

	if newCred["zcodejwttoken"] != "enc:v1:yyy" {
		t.Fatal("zcodejwttoken 未被替换为快照值")
	}
	if newCred["unrelated_setting"] != "keep-me" {
		t.Fatal("非账号字段被误删")
	}
	providers := newCfg["provider"].(map[string]any)
	if _, ok := providers["builtin:zai"]; ok {
		t.Fatal("旧账号的 JWT 槽位应被清除")
	}
	if _, ok := providers["builtin:bigmodel-start-plan"]; !ok {
		t.Fatal("快照槽位未写入")
	}
	if _, ok := providers["custom:my-openrouter"]; !ok {
		t.Fatal("第三方 provider 槽位被误删")
	}
	editor := newCfg["editor"].(map[string]any)
	if editor["fontSize"].(float64) != 14 {
		t.Fatal("无关配置被改动")
	}
	// 原对象不被修改（Apply 必须深拷贝）
	if _, ok := curCfg["provider"].(map[string]any)["builtin:zai"]; !ok {
		t.Fatal("Apply 修改了输入对象")
	}
}

func TestApplyMergesSlotPreservingExtraFields(t *testing.T) {
	_, cfg := liveState()
	snap := Slice(map[string]any{}, cfg)

	// 当前配置里同 id 槽位没有 apiKey（非账号槽位，不会被清除）但带有快照没有的额外字段
	curCfg := map[string]any{
		"provider": map[string]any{
			"builtin:bigmodel-start-plan": map[string]any{
				"enabled": false,
				"options": map[string]any{"timeout": 30},
			},
		},
	}
	_, newCfg := Apply(snap, map[string]any{}, curCfg)
	slot := newCfg["provider"].(map[string]any)["builtin:bigmodel-start-plan"].(map[string]any)
	opts := slot["options"].(map[string]any)
	if opts["timeout"].(float64) != 30 {
		t.Fatal("合并时丢失了快照没有的额外字段 timeout")
	}
	if opts["apiKey"] != fakeJWT("111") {
		t.Fatal("apiKey 冲突时应以快照值为准")
	}
	if slot["enabled"] != true {
		t.Fatal("enabled 未被快照值覆盖")
	}
}

func TestNonJWTBuiltinSlotIsAccountSlot(t *testing.T) {
	// bigmodel API key（非 JWT 格式）也是账号绑定的，必须随切换替换（MissCat 事故回归）
	cred := map[string]any{"zcodejwttoken": "enc:v1:x"}
	cfg := map[string]any{
		"provider": map[string]any{
			"builtin:bigmodel-coding-plan": map[string]any{
				"enabled": false,
				"options": map[string]any{"apiKey": "fa05e444abc123.xyz456"},
			},
			"builtin:bigmodel": map[string]any{ // 无 apiKey 的内置槽位是 provider 级配置，保留
				"enabled": true,
			},
			"custom:third-party": map[string]any{
				"enabled": true,
				"options": map[string]any{"apiKey": "sk-whatever"},
			},
		},
	}
	snap := Slice(cred, cfg)
	if _, ok := snap.ProviderSlots["builtin:bigmodel-coding-plan"]; !ok {
		t.Fatal("非 JWT apiKey 的 builtin 槽位也应被切片捕获")
	}
	if _, ok := snap.ProviderSlots["builtin:bigmodel"]; ok {
		t.Fatal("无 apiKey 的 builtin 槽位不应被捕获")
	}
	if _, ok := snap.ProviderSlots["custom:third-party"]; ok {
		t.Fatal("自定义 provider 不应被捕获")
	}

	// Apply：当前登录态里另一个账号的非 JWT builtin key 必须被清掉，自定义 key 保留
	newCred, newCfg := Apply(snap, map[string]any{}, cfg)
	_ = newCred
	providers := newCfg["provider"].(map[string]any)
	slot := providers["builtin:bigmodel-coding-plan"].(map[string]any)
	if slot["options"].(map[string]any)["apiKey"] != "fa05e444abc123.xyz456" {
		t.Fatal("builtin 槽位应被快照值替换")
	}
	if _, ok := providers["custom:third-party"]; !ok {
		t.Fatal("自定义 provider 被误删")
	}
}

func TestSnapshotJSONRoundTrip(t *testing.T) {
	cred, cfg := liveState()
	snap := Slice(cred, cfg)
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var back Snapshot
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.CredentialFields) != len(snap.CredentialFields) ||
		len(back.ProviderSlots) != len(snap.ProviderSlots) {
		t.Fatal("快照 JSON 往返后字段丢失")
	}
}
