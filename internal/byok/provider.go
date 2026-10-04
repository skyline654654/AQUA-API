// Package byok —— 本文件是「用户自备密钥（BYOK）」可用的上游提供方目录。
//
// 意图（Why）：
//
//	BYOK 与「渠道」的区别在于归属：渠道是站点的资产，BYOK 的凭据属于用户。
//	两者共享同一份上游能力描述（接什么地址、什么鉴权头、有哪些模型），
//	若各维护一份 inevitably 会漂移——渠道目录加了新上游却忘了同步 BYOK，
//	用户就会看到"这个上游明明能用，我的Key 却不让填"。
//
//	因此本包**从 channeltype 目录派生**而非另立一份：
//	  - 单一真值：地址、协议、鉴权方式全部来自 channeltype.Type；
//	  - 白名单控制：只有显式标记为"允许 BYOK"的上游才对用户开放，
//	    否则任何渠道类型都会变成"用户可自助接入"——这会让站点的
//	    上游协议细节与风控策略暴露给普通用户。
//
// 流转（Flow）：
//
//	server 校验 user_keys.provider 是否在白名单
//	  → relay 选路时按 provider 取默认 BaseURL / 鉴权头
//	    → 用用户自己的 Key向上游发请求
//
// 扩展（Extend）：
//
//	新增可 BYOK 的上游：在 channeltype.Type 上加 ByokAllowed 字段并置true，
//	本包的目录自动包含它，无需在此改代码。
package byok

import (
	"sort"
	"strings"

	"github.com/xiaosu4610/aqua-api/internal/channeltype"
)

// Provider 是「用户可自助接入的上游」的一份可对外展示的描述。
//
// 它是 channeltype.Type 的**对外视图**：只暴露用户需要知道的字段
// （名称、默认地址、可选模型），不暴露协议/鉴权等内部实现细节——
// 那些是站长配置渠道时用的，不该出现在面向普通用户的密钥管理页。
type Provider struct {
	// Key 是稳定标识，落进 user_keys.provider（改名会导致老数据失效）。
	Key string `json:"key"`
	// Label 是展示名（如「NVIDIA NIM」）。
	Label string `json:"label"`
	// DefaultBaseURL 是默认接入点；用户未填 base_url 时使用它。
	DefaultBaseURL string `json:"default_base_url"`
	// BaseURLEditable 表示用户是否可以改成自己的接入点。
	BaseURLEditable bool `json:"base_url_editable"`
	// Notes 是一句话说明，帮用户判断"我的 Key 能不能用在这里"。
	Notes string `json:"notes"`
	// SuggestedModels 是该上游常见可调用的模型（仅作提示，不限制用户）。
	//
	// 为什么不把它变成白名单约束：各账号 region / 配额不同，
	// 实际能调什么模型以 NVIDIA 后台为准。给提示但不强约束更诚实。
	SuggestedModels []string `json:"suggested_models,omitempty"`
}

// byokProviders 是允许用户自助接入的上游白名单（key → 展示信息）。
//
// 为什么用白名单而不是"channeltype 里所有 Available 的都行"：
// 站点里有些上游是自建的（协议特殊、只对特定客户开放），
// 让普通用户填自己的 Key 打过去既无意义也有风控风险。
// 白名单是刻意的收敛，把"技术上可行"与"产品上开放"分开。
var byokProviders = map[string]Provider{
	"nvidia": {
		Key:             "nvidia",
		Label:           "NVIDIA NIM",
		DefaultBaseURL:  "https://integrate.api.nvidia.com/v1",
		BaseURLEditable: true,
		Notes: "NVIDIA 官方模型推理服务（OpenAI 兼容）。填入你在 build.nvidia.com 申请到的 API Key，" +
			"即可用你自己的 NVIDIA 额度访问模型，费用由 NVIDIA 直接向你收取。",
		SuggestedModels: []string{
			"meta/llama-3.3-70b-instruct",
			"meta/llama-3.1-405b-instruct",
			"deepseek-ai/deepseek-r1",
			"qwen/qwen2.5-coder-32b-instruct",
			"nvidia/llama-3.1-nemotron-70b-instruct",
		},
	},
}

// Providers 返回全部可 BYOK 的上游（按 key 升序，保证前端展示顺序稳定）。
func Providers() []Provider {
	result := make([]Provider, 0, len(byokProviders))
	for _, p := range byokProviders {
		result = append(result, p)
	}
	// map 遍历顺序随机，不排序会导致每次刷新下拉框顺序都在变。
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result
}

// Lookup 按标识查找；不存在返回 false。
func Lookup(key string) (Provider, bool) {
	p, ok := byokProviders[strings.TrimSpace(key)]
	return p, ok
}

// IsAllowed 判断某 provider 是否开放用户自助接入。
func IsAllowed(key string) bool {
	_, ok := Lookup(key)
	return ok
}

// ResolveBaseURL 返回本次调用应使用的上游地址。
//
// 优先用户自填（各账号 region 不同，接入点可能不同）；
// 用户未填时回退白名单里的默认地址。两者都空才报错——
// 那说明白名单条目本身没配全，属于配置错误。
func ResolveBaseURL(provider Provider, userBaseURL string) (string, bool) {
	if custom := strings.TrimSpace(userBaseURL); custom != "" {
		if !provider.BaseURLEditable {
			// 不允许改地址的上游不接受自定义接入点（防止被指向任意地址，
			// 那等于让平台替用户代理任意域名，风险由平台承担）。
			return "", false
		}
		return strings.TrimRight(custom, "/"), true
	}
	if provider.DefaultBaseURL == "" {
		return "", false
	}
	return provider.DefaultBaseURL, true
}

// AuthHeaders 返回该 provider 需要的鉴权请求头。
//
// 目前支持的 BYOK 上游都是 OpenAI 兼容 + Bearer 鉴权，
// 因此这里返回 Bearer 头。若将来接入需要特殊头的上游
// （如 Azure 的 api-key），在 switch 里加一个分支即可。
func AuthHeaders(providerKey, apiKey string) map[string]string {
	key := strings.TrimSpace(providerKey)
	if key == "" || strings.TrimSpace(apiKey) == "" {
		return nil
	}
	// 刻意要求"已登记在白名单"：不认识的 provider 不给鉴权头，
	// 避免有人把 provider 填成任意值后让平台替他向任意地址发带凭据的请求。
	if !IsAllowed(key) {
		return nil
	}
	// NIM 与 OpenAI 兼容，标准 Bearer 即可。
	return map[string]string{"Authorization": "Bearer " + apiKey}
}

// SupportsModelList 判断该 provider 是否支持拉取模型清单。
//
// 用于密钥管理页的"测试连接"功能：能拉清单就能立刻验证 Key 是否有效，
// 比发一次对话请求便宜得多。
func SupportsModelList(providerKey string) bool {
	t, ok := channeltype.Find(providerKey)
	return ok && t.SupportsModelList && IsAllowed(providerKey)
}
