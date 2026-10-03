// 本文件是「运营域模型」的单元测试（场景标签、BYOK 密钥、渠道采样）。
//
// 意图（Why）：
//
//	这几个模型里有大量"看起来是小事、错了就出事"的方法：
//	  - NormalizeTag 决定归因能不能对上账（同一场景被拆成多行 = 看板失效）；
//	  - MaskedKey 是唯一的凭据泄露防线；
//	  - IsEnabled 同时考虑"停用"与"熔断"两种不可用，混淆会让用户困惑；
//	  - ChannelSample 的比率计算在空数据时必须给出中性值而非 0。
//	它们都是纯函数，测试成本极低，因此必须全覆盖。
package model

import (
	"testing"
	"time"
)

func TestNormalizeTag(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"空串归为 untagged", "", TagUntagged},
		{"纯空白归为 untagged", "   \t\n ", TagUntagged},
		{"去掉首尾空白", "  playground  ", "playground"},
		{"普通标签原样", "billing-gateway", "billing-gateway"},
		{"超长截断到 32 字节", longString(MaxTagLength+10), longString(MaxTagLength)},
		{"恰好 32 字节不截断", longString(MaxTagLength), longString(MaxTagLength)},
		{"中英混合保留", "支付网关-商户回调", "支付网关-商户回调"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeTag(tt.input); got != tt.want {
				t.Errorf("NormalizeTag(%q) = %q, 期望 %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestNormalizeTag_Idempotent 验证归一化是幂等的。
//
// 这条很重要：写入侧（relay）与读出侧（归因）都会调它，
// 非幂等会导致"归一两次得到不同结果"，进而让同一场景在不同路径下不一致。
func TestNormalizeTag_Idempotent(t *testing.T) {
	for _, input := range []string{"", "  x  ", longString(100), "正常"} {
		once := NormalizeTag(input)
		twice := NormalizeTag(once)
		if once != twice {
			t.Errorf("非幂等: NormalizeTag(%q)=%q 但再调一次得 %q", input, once, twice)
		}
	}
}

func longString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

func TestUserKey_MaskedKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{"空密钥返回空", "", ""},
		{"短密钥只显首字符", "abc", "a***"},
		{"超短密钥", "a", "a***"},
		{"标准长度显头尾各4", "nvapi-1234567890abcdef", "nvap***cdef"},
		{"恰好 8 位走短密钥分支", "12345678", "1***"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := &UserKey{APIKey: tt.key}
			if got := k.MaskedKey(); got != tt.want {
				t.Errorf("MaskedKey(%q) = %q, 期望 %q", tt.key, got, tt.want)
			}
		})
	}
}

// TestUserKey_MaskedKeyNeverLeaksSecret 是最重要的一条防线测试：
// 掩码结果里绝不能出现超过 4 位的原始片段。
func TestUserKey_MaskedKeyNeverLeaksSecret(t *testing.T) {
	secret := "nvapi-SUPERSECRETVALUE-1234567890"
	masked := (&UserKey{APIKey: secret}).MaskedKey()
	if masked == secret {
		t.Fatal("掩码等于原文——凭据直接泄露")
	}
	// 中间那段绝不能出现
	if contains(masked, "SUPERSECRET") {
		t.Errorf("掩码泄露了密钥中段: %q", masked)
	}
	// 长度必须显著短于原文
	if len(masked) >= len(secret)/2 {
		t.Errorf("掩码长度 %d 相对原文 %d 太长，信息泄露过多", len(masked), len(secret))
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestUserKey_IsEnabled(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		key  UserKey
		want bool
	}{
		{"启用且未熔断", UserKey{Status: UserKeyStatusEnabled}, true},
		{"已停用", UserKey{Status: UserKeyStatusDisabled}, false},
		{"启用但在熔断期", UserKey{
			Status:        UserKeyStatusEnabled,
			CooldownUntil: now.Add(10 * time.Minute).Unix(),
		}, false},
		{"启用且熔断已过期", UserKey{
			Status:        UserKeyStatusEnabled,
			CooldownUntil: now.Add(-10 * time.Minute).Unix(),
		}, true},
		{"熔断时间为 0（未熔断）", UserKey{Status: UserKeyStatusEnabled, CooldownUntil: 0}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.key.IsEnabled(now); got != tt.want {
				t.Errorf("IsEnabled = %v, 期望 %v", got, tt.want)
			}
		})
	}
	// nil 接收者必须安全（仓储在未命中时可能返回 nil 指针）
	var nilKey *UserKey
	if nilKey.IsEnabled(now) {
		t.Error("nil 密钥的 IsEnabled 应为 false")
	}
}

func TestUserKey_AllowsModel(t *testing.T) {
	open := &UserKey{Models: ""} // 空清单 = 不限制
	if !open.AllowsModel("任意模型") {
		t.Error("空清单应允许任意模型")
	}

	restricted := &UserKey{Models: " gpt-4o , claude-3 "}
	if !restricted.AllowsModel("gpt-4o") {
		t.Error("清单内模型应被允许")
	}
	if !restricted.AllowsModel("claude-3") {
		t.Error("清单内模型（带空白）应被允许")
	}
	if restricted.AllowsModel("gpt-3.5") {
		t.Error("清单外模型必须被拒绝——越界会让用户意外消耗他没预期的模型")
	}
	// 大小写：模型名不区分大小写时应视为同一个
	if restricted.AllowsModel("GPT-4O") {
		t.Error("大小写不同不应算同一模型（精确匹配，避免误放行）")
	}
}

func TestUserKey_Validate(t *testing.T) {
	tests := []struct {
		name    string
		key     UserKey
		wantErr bool
	}{
		{"合法", UserKey{UserID: 1, Provider: "nvidia", APIKey: "k", Status: UserKeyStatusEnabled}, false},
		{"无用户", UserKey{Provider: "nvidia", APIKey: "k", Status: UserKeyStatusEnabled}, true},
		{"无 provider", UserKey{UserID: 1, APIKey: "k", Status: UserKeyStatusEnabled}, true},
		{"provider 全是空白", UserKey{UserID: 1, Provider: "   ", APIKey: "k", Status: UserKeyStatusEnabled}, true},
		{"无密钥", UserKey{UserID: 1, Provider: "nvidia", Status: UserKeyStatusEnabled}, true},
		{"状态非法", UserKey{UserID: 1, Provider: "nvidia", APIKey: "k", Status: 99}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.key.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate 错误 = %v, 期望有错 = %v", err, tt.wantErr)
			}
		})
	}
}

func TestChannelSample_Ratios(t *testing.T) {
	// 正常样本：成功率 0.9，平均延迟 200ms
	s := &ChannelSample{Requests: 10, Success: 9, LatencySumMS: 2000}
	if diff := s.SuccessRate() - 0.9; diff > 0.001 || diff < -0.001 {
		t.Errorf("成功率 = %v, 期望 0.9", s.SuccessRate())
	}
	if diff := s.AvgLatencyMS() - 200; diff > 0.001 || diff < -0.001 {
		t.Errorf("平均延迟 = %v, 期望 200", s.AvgLatencyMS())
	}
}

func TestChannelSample_EmptySampleIsNeutral(t *testing.T) {
	// 空样本的成功率必须是 1（中性）而不是 0——
	// EWMA 里"没有数据"表示"没有负面证据"，返回 0 会把空闲渠道压到最低权重。
	empty := &ChannelSample{}
	if empty.SuccessRate() != 1 {
		t.Errorf("空样本成功率 = %v, 期望 1（中性）", empty.SuccessRate())
	}
	if empty.AvgLatencyMS() != 0 {
		t.Errorf("空样本平均延迟 = %v, 期望 0", empty.AvgLatencyMS())
	}
	// nil 也必须安全
	var nilSample *ChannelSample
	if nilSample.SuccessRate() != 1 || nilSample.AvgLatencyMS() != 0 {
		t.Error("nil 采样的比率方法应返回中性值")
	}
}
