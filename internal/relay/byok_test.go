// 本文件是「用户自备密钥（BYOK）」在转发链路中的单元测试。
//
// 意图（Why）：
//
//	BYOK 涉及两条极容易搞反的语义，每一条弄错都直接损害用户利益：
//
//  1. **不双扣费**：用户已经付钱给 NVIDIA 了，站内再扣就是双重收费。
//     测试用「扣费金额必须为 0」把这条钉死。
//  2. **额度墙不能拦住 BYOK**：额度耗尽的用户配了自己的 Key，
//     必须在鉴权阶段就放行——否则功能在最外层就被拦死，
//     用户会看到"我明明配了密钥却还是 429"。
package relay

import (
	"context"
	"testing"
	"time"

	"github.com/xiaosu4610/aqua-api/internal/byok"
	"github.com/xiaosu4610/aqua-api/internal/model"
	"github.com/xiaosu4610/aqua-api/internal/reqctx"
)

// fakeUserKeyRepo 是 UserKeyRepository 的最小测试桩。
//
// 只实现 BYOK 选路真正用到的方法（FindEnabledForProvider），
// 其余按接口补空实现——与 media_test.go 里的 fakeUsageLogRepo 同一套做法：
// 接口演进时桩必须跟上，否则整包编译失败。
type fakeUserKeyRepo struct {
	keys map[string]*model.UserKey // provider → 密钥
}

func (f *fakeUserKeyRepo) FindEnabledForProvider(_ context.Context, userID uint64, provider string) (*model.UserKey, error) {
	key, ok := f.keys[provider]
	if !ok || key.UserID != userID {
		return nil, nil
	}
	return key, nil
}

func (f *fakeUserKeyRepo) Create(context.Context, *model.UserKey) error { return nil }
func (f *fakeUserKeyRepo) GetByID(context.Context, uint64, uint64) (*model.UserKey, error) {
	return nil, model.ErrUserKeyNotFound
}
func (f *fakeUserKeyRepo) ListByUser(context.Context, uint64) ([]*model.UserKey, error) {
	return nil, nil
}
func (f *fakeUserKeyRepo) Update(context.Context, *model.UserKey) error { return nil }
func (f *fakeUserKeyRepo) Delete(context.Context, uint64, uint64) error { return nil }
func (f *fakeUserKeyRepo) MarkFailure(context.Context, uint64, int, time.Duration) error {
	return nil
}
func (f *fakeUserKeyRepo) MarkSuccess(context.Context, uint64) error { return nil }

func TestByokUserKeyID_OnlyRecognizesMarkedChannels(t *testing.T) {
	tests := []struct {
		name string
		ch   *model.Channel
		want uint64
	}{
		{"nil 渠道", nil, 0},
		{"无 ExtraConfig", &model.Channel{ID: 5}, 0},
		{"ExtraConfig 为空", &model.Channel{ExtraConfig: map[string]string{}}, 0},
		{"有其它扩展参数（真实渠道）", &model.Channel{
			ExtraConfig: map[string]string{"deployment": "gpt-4o"},
		}, 0},
		{"带 BYOK 标记", &model.Channel{
			ExtraConfig: map[string]string{byokKeyIDExtraKey: "42"},
		}, 42},
		{"标记值非法（非数字）", &model.Channel{
			ExtraConfig: map[string]string{byokKeyIDExtraKey: "abc"},
		}, 0},
		{"标记为空串", &model.Channel{
			ExtraConfig: map[string]string{byokKeyIDExtraKey: ""},
		}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := byokUserKeyID(tt.ch); got != tt.want {
				t.Errorf("byokUserKeyID = %d, 期望 %d", got, tt.want)
			}
		})
	}
}

func TestByokChannelFromKey_CarriesUserCredential(t *testing.T) {
	key := &model.UserKey{
		ID:       7,
		UserID:   1,
		Provider: "nvidia",
		Label:    "生产",
		APIKey:   "nvapi-secret",
		Status:   model.UserKeyStatusEnabled,
	}
	provider, ok := lookupTestProvider("nvidia")
	if !ok {
		t.Fatal("测试前提失败：白名单里应有 nvidia")
	}
	ch := byokChannelFromKey(key, provider, "https://example.test/v1", "meta/llama-3.3-70b-instruct")

	// 虚拟渠道必须携带用户自己的凭据，否则转发时用的还是站点渠道的 Key。
	if ch.APIKey != "nvapi-secret" {
		t.Errorf("虚拟渠道未携带用户凭据: %q", ch.APIKey)
	}
	if ch.BaseURL != "https://example.test/v1" {
		t.Errorf("BaseURL = %q, 期望用户指定地址", ch.BaseURL)
	}
	// 标记必须可被byokUserKeyID 读回（这是"不计站内额度"的判据）。
	if got := byokUserKeyID(ch); got != 7 {
		t.Errorf("BYOK 标记 = %d, 期望 7", got)
	}
	// 状态必须是启用，否则后续 pickCandidate 会跳过它。
	if ch.Status != model.ChannelStatusEnabled {
		t.Errorf("Status = %v, 期望启用", ch.Status)
	}
	// TypeKey 决定鉴权头，必须是已登记的渠道类型。
	if ch.TypeKey != "nvidia" {
		t.Errorf("TypeKey = %q, 期望 nvidia（决定鉴权方式）", ch.TypeKey)
	}
}

func TestByokChannelFromKey_EmptyModelListMeansCurrentRequestOnly(t *testing.T) {
	// 用户没限定模型清单时，虚拟渠道只声明"本次请求这一个模型"。
	// 语义是"这一跳"，不是"这个渠道支持哪些模型"。
	key := &model.UserKey{ID: 1, UserID: 1, Provider: "nvidia", APIKey: "k", Status: model.UserKeyStatusEnabled}
	provider, _ := lookupTestProvider("nvidia")
	ch := byokChannelFromKey(key, provider, "https://x.test", "some-model")
	if len(ch.Models) != 1 || ch.Models[0] != "some-model" {
		t.Errorf("Models = %v, 期望仅含本次请求的模型", ch.Models)
	}
}

func TestLabelSuffix(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"生产", "/生产"},
		{"  dev  ", "/dev"},
	}
	for _, tt := range tests {
		if got := labelSuffix(tt.in); got != tt.want {
			t.Errorf("labelSuffix(%q) = %q, 期望 %q", tt.in, got, tt.want)
		}
	}
}

// lookupTestProvider 从 BYOK 白名单里取一个 provider（测试辅助）。
func lookupTestProvider(key string) (byok.Provider, bool) {
	return byok.Lookup(key)
}

func TestItoa64(t *testing.T) {
	tests := []struct {
		in   uint64
		want string
	}{
		{0, "0"},
		{7, "7"},
		{42, "42"},
		{1234567890, "1234567890"},
	}
	for _, tt := range tests {
		if got := itoa64(tt.in); got != tt.want {
			t.Errorf("itoa64(%d) = %q, 期望 %q", tt.in, got, tt.want)
		}
	}
}

func TestIsFreeViaUserKey_NilSafe(t *testing.T) {
	// 未启用 BYOK 时必须返回 false（而不是 panic）——
	// 这条判定在鉴权热路径上，任何异常都会拖垮全站。
	var r *Relay
	if r.IsFreeViaUserKey(1, "any-model") {
		t.Error("nil Relay 不应判定为免费")
	}
	relayWithoutKeys := &Relay{}
	if relayWithoutKeys.IsFreeViaUserKey(1, "any-model") {
		t.Error("未注入 userKeys 时不应判定为免费")
	}
	// 零值入参同样安全
	if relayWithoutKeys.IsFreeViaUserKey(0, "") {
		t.Error("零值入参不应判定为免费")
	}
}

// TestSettleQuota_BYOK不扣站内额度 是本模块最重要的一条断言。
//
// 用户已经用自己的钱付给了 NVIDIA，若这里再按 token 扣站内额度就是双重收费——
// 那是会让用户立刻弃用本站的设计。测试用"返回值必须为 0 + 必须走 Release"
// 把这条语义钉死。
func TestSettleQuota_BYOK不扣站内额度(t *testing.T) {
	quota := newFakeQuotaRepo()
	billing := newTestBilling(100, 1_000_000, 2_000_000, 0).WithQuotaRepository(quota)
	r := &Relay{billing: billing}

	const requestID = "req-byok-1"
	// 先做一次预留，模拟鉴权阶段的预扣。
	if _, err := quota.Reserve(context.Background(), model.ReserveRequest{
		RequestID: requestID, UserID: 1, TokenID: 1, Amount: 5_000,
	}); err != nil {
		t.Fatalf("准备预留失败: %v", err)
	}

	ctx := reqctx.WithIdentity(context.Background(), reqctx.Identity{
		UserID: 1, TokenID: 1, RequestID: requestID,
	})

	// 一次"用掉很多 token"的 BYOK 调用。
	settled := r.settleQuota(ctx, usageEntry{
		UserID: 1, TokenID: 1, Model: "gpt-4o",
		Usage:           openAIUsage{PromptTokens: 100_000, CompletionTokens: 100_000},
		ByOKUserKeyID:   7,
		StatusCode:      200,
	})

	if settled != 0 {
		t.Errorf("BYOK 调用结算额度 = %d，期望0（不能双扣用户已经付给上游的钱）", settled)
	}
	// 必须走 Release 把预扣还回去，否则并发下会累积"额度占用"。
	if quota.releaseCalls == 0 {
		t.Error("BYOK 调用必须退还站内预留额度，否则会累积额度占用")
	}
	//绝不能走 Settle（那才是按 token 扣费）。
	if quota.settleCalls != 0 {
		t.Errorf("BYOK 调用不应走 Settle（settleCalls=%d），那意味着仍在扣费", quota.settleCalls)
	}
}

// TestSettleQuota_非BYOK照常扣费 是上面的对照组：
// 防止"为了不扣 BYOK 而把所有调用都改成免费"这种回归。
func TestSettleQuota_非BYOK照常扣费(t *testing.T) {
	quota := newFakeQuotaRepo()
	billing := newTestBilling(100, 1_000_000, 2_000_000, 0).WithQuotaRepository(quota)
	r := &Relay{billing: billing}

	const requestID = "req-normal-1"
	if _, err := quota.Reserve(context.Background(), model.ReserveRequest{
		RequestID: requestID, UserID: 1, TokenID: 1, Amount: 5_000,
	}); err != nil {
		t.Fatalf("准备预留失败: %v", err)
	}
	ctx := reqctx.WithIdentity(context.Background(), reqctx.Identity{
		UserID: 1, TokenID: 1, RequestID: requestID,
	})

	// ByOKUserKeyID = 0 表示普通站点渠道调用，必须正常结算。
	r.settleQuota(ctx, usageEntry{
		UserID: 1, TokenID: 1, Model: "gpt-4o",
		Usage:      openAIUsage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000},
		StatusCode: 200,
	})

	if quota.settleCalls == 0 {
		t.Error("普通调用必须走 Settle（BYOK 特例不能误伤正常计费）")
	}
	if quota.releaseCalls != 0 {
		t.Error("普通调用不应被 Release")
	}
}
