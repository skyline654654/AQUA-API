// 本文件实现「用户自备密钥（BYOK）」在转发链路中的选路接入。
//
// 意图（Why）：
//
//	用户在门户配置了自己的 NVIDIA Key 后，应当能用本网关的统一协议去调NVIDIA，
//	而不是"自己拿curl 去打NVIDIA"。但他可能同时也在用站点的公共渠道——
//	那就需要一个明确的规则决定"这次调用走哪边"。
//
// 本文件的方案：把「用户自备密钥」包装成一个**虚拟渠道**，
// 由 listCandidates 在常规渠道之前把它插到候选集首位。
//
// 为什么选"虚拟渠道"而不是在 forwardChat 里另开一条分支：
//
//	转发链路有大量横切关注点——协议适配、流式改写、失败重试、超时控制、
//	错误脱敏、计费落库。旁路一条分支意味着这些逻辑都要复制一遍，
//	而复制出来的分支会立刻与主链路漂移（改一处忘另一处）。
//	包装成渠道后，它就是候选集里的一个普通成员，
//	重试/超时/脱敏/落库全部自动生效，零重复。
//
// 计费语义（本文件最重要的一条铁律）：
//
//	BYOK 调用【不计站内额度】。理由：用户的钱已经付给 NVIDIA 了，
//	再扣站内额度就是双重收费——这是会让用户立刻弃用本站的设计。
//	但仍要落usage_logs：审计、统计、成本归因、排行榜全都依赖它，
//	且"这次调用的 token 消耗"对用户理解自己的用量有价值。
//	实现方式是给 Channel 打个内部标记（byokUserKeyID != 0），
//	由 settleQuota 识别后跳过扣费但保留记账。
//
// 流转（Flow）：
//
//	listCandidates
//	  └─ byokVirtualChannel（命中？）→ 插到候选集[0]
//	     → forwardChat 正常转发（用户 Key 走 Authorization 头）
//	       → recordUsage → settleQuota 见ByOK 标记 → 跳过扣费、只记账
//
// 扩展（Extend）：
//
//	支持"用户可关闭优先自备"开关时：在 byokVirtualChannel 里读用户偏好设置，
//	偏好关闭时返回 nil（不插虚拟渠道），退化为纯公共渠道行为。
package relay

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/xiaosu4610/aqua-api/internal/byok"
	"github.com/xiaosu4610/aqua-api/internal/model"
	"github.com/xiaosu4610/aqua-api/internal/reqctx"
)

// byokVirtualChannelID 是虚拟渠道的固定 ID。
//
// 为什么取 0：0 在本项目里一贯表示"不适用 / 无来源"
// （如 channel_id=0 表示请求在选渠道前就失败），
// 且 sampleChannelHealth 对 channel_id=0 直接跳过——
// 这正好让 BYOK 调用不污染"公共渠道健康度"这条统计线。
// 用真实渠道 ID 空间里的值反而危险：它可能与某个真实渠道撞车，
// 让 BYOK 请求被记到别人头上。
const byokVirtualChannelID = 0

// byokFailureThreshold 是 BYOK 连续失败多少次后熔断。
//
// 取 3 次：与 userKeyRepository.MarkFailure 的默认阈值一致。
// 比公共渠道更敏感是合理的——用户自备 Key 的失败几乎总是
// "Key 无效/额度耗尽/区域不匹配"，继续重试只是白白打上游。
const byokFailureThreshold = 3

// byokBaseCooldown 是 BYOK 首次熔断的冷却时长（之后按失败次数指数退避）。
const byokBaseCooldown = 5 * time.Minute

// byokVirtualChannel 尝试为当前请求构造一个"用户自备密钥"的虚拟渠道。
//
// 返回 nil 表示"本次请求不走 BYOK"，可能的原因：
//   - 部署未启用（r.userKeys 为 nil）；
//   - 请求未经令牌鉴权（没有用户身份）；
//   - 该用户没有配置任何启用的自备密钥；
//   - 所有已配置的 Key 都处于停用或熔断状态。
//
// 设计取舍：为什么放在 listCandidates 而不是鉴权阶段就决定？
// 因为"这个模型是否允许用自备 Key"依赖模型名（Key 可以限定模型清单），
// 而模型名要到解析请求体之后才知道。放在候选集构造阶段刚好两者都齐备。
func (r *Relay) byokVirtualChannel(ctx context.Context, modelName string) *model.Channel {
	if r.userKeys == nil {
		return nil
	}
	identity, ok := reqctx.IdentityFrom(ctx)
	if !ok || identity.UserID == 0 {
		// 无用户身份（内部调用 / 测试）：不参与 BYOK。
		return nil
	}

	now := time.Now()
	// 按用户逐个 provider 尝试：先命中的胜出。
	// provider 数量很少（白名单当前只有 NVIDIA），顺序遍历的成本可忽略。
	for _, provider := range byok.Providers() {
		key, err := r.userKeys.FindEnabledForProvider(ctx, identity.UserID, provider.Key)
		if err != nil || key == nil {
			continue
		}
		// 三道门：启用、模型允许、未熔断。缺一不可。
		if !key.IsEnabled(now) {
			continue
		}
		if !key.AllowsModel(modelName) {
			continue
		}
		baseURL, ok := byok.ResolveBaseURL(provider, key.BaseURL)
		if !ok {
			continue
		}
		// 熔断状态在这里是"看得见但仍不选"——用户应从明确的提示
		//（"密钥暂时熔断"）而不是"请求莫名失败"里发现问题。
		if key.IsCoolingDown(now) {
			continue
		}
		return byokChannelFromKey(key, provider, baseURL, modelName)
	}
	return nil
}

// byokChannelFromKey 由自备密钥构造一个虚拟渠道。
//
// 复用真实渠道的结构体是刻意的：转发链路的全部横切逻辑
// （协议适配、鉴权头、重试、超时、脱敏）都认渠道，
// 造一个新类型只会让这些地方全部要加类型判断。
func byokChannelFromKey(key *model.UserKey, provider byok.Provider, baseURL, modelName string) *model.Channel {
	// Models 只放本次请求的模型名：虚拟渠道只服务"当前这一跳"，
	// 它的存在时间就是单次请求，不承担"声明支持哪些模型"的语义。
	models := []string{modelName}
	if list := key.ModelList(); len(list) > 0 {
		models = list
	}
	return &model.Channel{
		ID:   byokVirtualChannelID,
		Name: "BYOK:" + provider.Label + labelSuffix(key.Label),
		// Group 留空：BYOK 不属于任何路由分组。分组只影响
		// "选哪些公共渠道"与"按哪组计价"，而这两件事对 BYOK 都不成立。
		BaseURL: baseURL,
		// APIKey 是用户自己的凭据。鉴权方式由 TypeKey 决定，
		// 因此类型取 provider 的 key（与渠道目录里的 key 同名）。
		APIKey: key.APIKey,
		Models: models,
		Status: model.ChannelStatusEnabled,
		Weight: 1,
		// TypeKey 复用渠道目录的注册名：白名单里的 provider key
		// 必须同时是一个已登记的渠道类型（见 byok.AuthHeaders 的白名单校验），
		// 这样鉴权头与协议适配器就都能自动对上。
		TypeKey: provider.Key,
		// ExtraConfig 里携带"用户 Key ID"：>0 表示这是 BYOK 调用，
		// 结算与失败统计据此跳过站内扣费（见 settleQuota）。
		//
		// 为什么不新增 model.Channel 的导出字段：加字段会让每个渠道的
		// 存储、序列化、校验都要考虑它（而真实渠道永远用不到）。
		// 藏进既有的 ExtraConfig 里，零侵入——真实渠道没有这个键，
		// 这条路径自然只对虚拟渠道生效。
		ExtraConfig: map[string]string{byokKeyIDExtraKey: itoa64(key.ID)},
	}
}

// byokKeyIDExtraKey 是虚拟渠道里承载"用户 Key ID"的内部键名。
//
// 为什么不新增 model.Channel 的导字段：加字段会让每个渠道的
// 存储、序列化、校验都要考虑它（而真实渠道永远用不到）。
// 藏进既有的 Extra（扩展参数）里，零侵入——真实渠道的 Extra 为空，
// 这条路径自然只对虚拟渠道生效。
const byokKeyIDExtraKey = "__byok_user_key_id"

// labelSuffix 返回带空格的备注后缀（空备注返回空串）。
func labelSuffix(label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return ""
	}
	return "/" + label
}

// itoa64 把 uint64 转为字符串。
func itoa64(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// byokUserKeyID 从渠道取出 BYOK 的用户 Key ID；非 BYOK 渠道返回 0。
//
// 判定依据是内部标记而非"ID==0"：ID==0 也会出现在
// "请求在选渠道前就失败"的场景，混用会把那类请求误判成 BYOK。
func byokUserKeyID(ch *model.Channel) uint64 {
	if ch == nil || ch.ExtraConfig == nil {
		return 0
	}
	raw := ch.ExtraConfig[byokKeyIDExtraKey]
	if raw == "" {
		return 0
	}
	var id uint64
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0
		}
		id = id*10 + uint64(c-'0')
	}
	return id
}

// markUserKeySuccess 在 BYOK 调用成功后清零该 Key 的失败计数与熔断。
//
// 非 BYOK 渠道（byokUserKeyID == 0）与仓储缺失时都是零开销返回：
// 这条路径对绝大多数请求都会被调用，代价必须接近零。
func (r *Relay) markUserKeySuccess(ctx context.Context, ch *model.Channel) {
	keyID := byokUserKeyID(ch)
	if keyID == 0 || r.userKeys == nil {
		return
	}
	// 用独立超时：原请求的 context 可能已因客户端断开而取消，
	// 复用它会让"用户取消请求"变成"Key 状态不更新"。
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := r.userKeys.MarkSuccess(writeCtx, keyID); err != nil {
		slog.Warn("重置自备密钥失败计数失败", "error", err, "user_key_id", keyID)
	}
}

// markUserKeyFailure 在 BYOK 调用最终失败后累加失败计数（达阈值熔断）。
func (r *Relay) markUserKeyFailure(ctx context.Context, ch *model.Channel) {
	keyID := byokUserKeyID(ch)
	if keyID == 0 || r.userKeys == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := r.userKeys.MarkFailure(writeCtx, keyID, byokFailureThreshold, byokBaseCooldown); err != nil {
		slog.Warn("记录自备密钥失败失败", "error", err, "user_key_id", keyID)
	}
}

// IsFreeViaUserKey 实现鉴权中间件的「按人免费」扩展点。
//
// 判定与 byokVirtualChannel 的三道门完全一致（启用 / 模型允许 / 未熔断）——
// 两条路径的判据必须一致，否则会出现"鉴权放行但转发时不选"（白跑一次）
// 或"鉴权拦下但转发时本可用"（功能形同虚设）的错位。
//
// 为什么放在 Relay 而非 Billing：它需要的是 userKeys 仓储，
// 而 Billing 的职责是价格与额度，不该知道"用户自备密钥"这件事。
// 鉴权中间件用可选接口（type assertion）接这个能力，未启用时自动降级。
func (r *Relay) IsFreeViaUserKey(userID uint64, modelName string) bool {
	if r == nil || r.userKeys == nil || userID == 0 || modelName == "" {
		return false
	}
	now := time.Now()
	for _, provider := range byok.Providers() {
		key, err := r.userKeys.FindEnabledForProvider(context.Background(), userID, provider.Key)
		if err != nil || key == nil {
			continue
		}
		if key.IsEnabled(now) && key.AllowsModel(modelName) {
			return true
		}
	}
	return false
}
