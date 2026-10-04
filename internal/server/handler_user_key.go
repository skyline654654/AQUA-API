// 本文件实现「用户自备密钥（BYOK）」的接口层。
//
// 意图（Why）：
//
//	部分用户愿意用自己的上游额度（例如 NVIDIA 官方 NIM 的 API Key），
//	但仍希望通过本网关的统一协议、鉴权、审计与用量统计来访问。
//	BYOK 让"凭据属于用户、调用仍走网关"成为可能。
//
// 安全边界（本文件最重要的部分）：
//
//  1. **明文绝不出现在响应里**。DTO 只给掩码（nv-***abcd），
//     这是不可协商的红线——密钥管理页的响应会经过浏览器、CDN、日志，
//     一旦回显明文就等于把它散布出去。
//  2. **归属由会话强制**。所有查询/修改都带 user_id 条件，
//     传什么 user_id 都会被忽略（沿用 handler_token.go 的既有纪律）。
//  3. **provider 必须在白名单内**。否则用户可以把 provider 填成任意值，
//     让平台替他把带凭据的请求发往任意地址。
//  4. **已保存的密钥不回显**。编辑时前端不提交 api_key 即视为"不修改"，
//     避免每次编辑都要重新传输明文（也避免日志/抓包泄露）。
//
// 流转（Flow）：
//
//	GET    /api/user/keys           列出我的密钥（掩码）
//	POST   /api/user/keys           新增（加密落库）
//	PATCH  /api/user/keys/:id       修改（api_key 留空 = 不改）
//	DELETE /api/user/keys/:id       删除
//	GET    /api/user/key-providers  可自助接入的上游清单（白名单）
//
// 扩展（Extend）：
//
//	新增 provider：在 internal/byok 白名单登记即可，本文件无需改动。
//	需要"一把 Key 多 provider"时：去掉仓储的唯一索引，改用同一个 model.UserKey
//	挂多条记录（每provider 一行），并在列表里按 provider 分组展示。
package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xiaosu4610/aqua-api/internal/byok"
	"github.com/xiaosu4610/aqua-api/internal/model"
	"github.com/xiaosu4610/aqua-api/internal/oai"
)

// maxUserKeyLength 是自备密钥的长度上限。
//
// 上限 512 字节：足够容纳任何主流厂商的 Key（含带前缀的长串），
// 同时防止有人把整个证书 / 多段拼接内容塞进来撑爆数据库与日志。
const maxUserKeyLength = 512

// userKeyDTO 是自备密钥的对外视图。
//
// 关键：没有 APIKey 字段——明文只存在于内部结构体，绝不参与序列化。
type userKeyDTO struct {
	ID       uint64 `json:"id"`
	Provider string `json:"provider"`
	// ProviderLabel 是白名单里的展示名（如「NVIDIA NIM」），省得前端再维护一份映射。
	ProviderLabel string `json:"provider_label"`
	Label         string `json:"label"`
	// MaskedKey 是掩码凭据（如 nv-***abcd），仅供用户辨认"我存的是哪一把"。
	MaskedKey string `json:"masked_key"`
	BaseURL   string `json:"base_url"`
	// EffectiveBaseURL 是实际会用的地址（用户自填优先，否则默认）。
	EffectiveBaseURL string   `json:"effective_base_url"`
	Models           []string `json:"models"`
	Status           int      `json:"status"`
	// StatusText 是状态的中文说明（启用 / 已停用 / 暂时熔断）。
	StatusText string `json:"status_text"`
	// FailCount 与 CooldownUntil 让用户明白"为什么我的 Key 暂时没被用"。
	FailCount     int   `json:"fail_count"`
	CooldownUntil int64 `json:"cooldown_until"`
	// CoolingDown 表示正处于熔断期（比 Status 更精确的"暂时不可用"）。
	CoolingDown bool  `json:"cooling_down"`
	LastUsedAt  int64 `json:"last_used_at"`
	CreatedAt   int64 `json:"created_at"`
	UpdatedAt   int64 `json:"updated_at"`
}

// userKeyRequest 是新增/修改自备密钥的请求体。
//
// 字段用指针：修改时未提交的字段保持原值。
// api_key 尤其需要这个语义——前端"只改备注"时不提交密钥，
// 服务端就不该要求重新传明文。
type userKeyRequest struct {
	Provider *string `json:"provider"`
	Label    *string `json:"label"`
	APIKey   *string `json:"api_key"`
	BaseURL  *string `json:"base_url"`
	Models   *string `json:"models"`
	Status   *int    `json:"status"`
}

// handleMyKeyProviders 返回可自助接入的上游清单（白名单）。
//
// 前端用它渲染"选择哪个上游"的表单；把它做成接口而不是前端硬编码，
// 是为了让"开放了哪些上游"始终以服务端为准——前端发版滞后不会导致
// 用户看到一个其实已关闭的上游选项。
func (s *Server) handleMyKeyProviders(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"providers": byok.Providers()})
}

// handleMyListKeys 返回当前用户的自备密钥列表（掩码）。
func (s *Server) handleMyListKeys(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}
	if s.deps.UserKeys == nil {
		s.respondInternalError(c, "自备密钥功能未启用")
		return
	}

	keys, err := s.deps.UserKeys.ListByUser(c.Request.Context(), user.ID)
	if err != nil {
		s.respondInternalError(c, "查询自备密钥失败")
		return
	}

	now := time.Now()
	items := make([]userKeyDTO, 0, len(keys))
	for _, k := range keys {
		items = append(items, toUserKeyDTO(k, now))
	}
	c.JSON(http.StatusOK, gin.H{"keys": items})
}

// handleMyCreateKey 新增一条自备密钥（加密落库）。
func (s *Server) handleMyCreateKey(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}
	if s.deps.UserKeys == nil {
		s.respondInternalError(c, "自备密钥功能未启用")
		return
	}

	var req userKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeUserError(c, http.StatusBadRequest,
			"请求参数格式错误", oai.TypeInvalidRequest, "invalid_request")
		return
	}
	// 新增时 provider / api_key 是必填，其余可缺省。
	provider := strings.TrimSpace(derefString(req.Provider))
	if provider == "" {
		writeUserError(c, http.StatusBadRequest,
			"请选择要接入的上游", oai.TypeInvalidRequest, "missing_field")
		return
	}
	apiKey := strings.TrimSpace(derefString(req.APIKey))
	if apiKey == "" {
		writeUserError(c, http.StatusBadRequest,
			"请填写 API Key", oai.TypeInvalidRequest, "missing_field")
		return
	}
	if len(apiKey) > maxUserKeyLength {
		writeUserError(c, http.StatusBadRequest,
			"API Key 过长", oai.TypeInvalidRequest, "invalid_request")
		return
	}

	key := &model.UserKey{
		UserID:   user.ID,
		Provider: provider,
		Label:    strings.TrimSpace(derefString(req.Label)),
		APIKey:   apiKey,
		BaseURL:  strings.TrimSpace(derefString(req.BaseURL)),
		Models:   normalizeModelListInput(derefString(req.Models)),
		Status:   model.UserKeyStatusEnabled,
	}
	if req.Status != nil {
		key.Status = model.UserKeyStatus(*req.Status)
	}

	if err := s.validateUserKey(key); err != nil {
		s.respondUserKeyError(c, err)
		return
	}
	if err := s.deps.UserKeys.Create(c.Request.Context(), key); err != nil {
		s.respondUserKeyError(c, err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{"key": toUserKeyDTO(key, time.Now())})
}

// handleMyUpdateKey 修改自备密钥。
//
// 语义约定：api_key 未提交或为空串 = 保持原密钥不变。
// 这是刻意的——见文件头安全边界的第 4 条。
func (s *Server) handleMyUpdateKey(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}
	if s.deps.UserKeys == nil {
		s.respondInternalError(c, "自备密钥功能未启用")
		return
	}
	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	key, err := s.deps.UserKeys.GetByID(ctx, id, user.ID)
	if err != nil {
		s.respondUserKeyError(c, err)
		return
	}

	var req userKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeUserError(c, http.StatusBadRequest,
			"请求参数格式错误", oai.TypeInvalidRequest, "invalid_request")
		return
	}

	// 逐字段按"提交了才改"的语义更新。
	if req.Provider != nil {
		key.Provider = strings.TrimSpace(*req.Provider)
	}
	if req.Label != nil {
		key.Label = strings.TrimSpace(*req.Label)
	}
	if req.BaseURL != nil {
		key.BaseURL = strings.TrimSpace(*req.BaseURL)
	}
	if req.Models != nil {
		key.Models = normalizeModelListInput(*req.Models)
	}
	if req.Status != nil {
		key.Status = model.UserKeyStatus(*req.Status)
	}
	// 密钥本体：留空 = 不改；非空 = 替换，并清空熔断状态
	//（用户主动换Key 说明旧Key 的失败计数对新Key 没有意义）。
	if newKey := strings.TrimSpace(derefString(req.APIKey)); newKey != "" {
		if len(newKey) > maxUserKeyLength {
			writeUserError(c, http.StatusBadRequest,
				"API Key 过长", oai.TypeInvalidRequest, "invalid_request")
			return
		}
		key.APIKey = newKey
		key.FailCount = 0
		key.CooldownUntil = 0
	}

	if err := s.validateUserKey(key); err != nil {
		s.respondUserKeyError(c, err)
		return
	}
	if err := s.deps.UserKeys.Update(ctx, key); err != nil {
		s.respondUserKeyError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"key": toUserKeyDTO(key, time.Now())})
}

// handleMyDeleteKey 删除自备密钥。
//
// 硬删除而非停用：凭据已经在用户手里，停用一条记录并不能"收回"它，
// 留着反而让用户以为"平台还存着我的 Key"（实际它已从库里消失）。
// 若将来需要"临时禁用"的安全开关，应另加字段，而不是复用删除。
func (s *Server) handleMyDeleteKey(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}
	if s.deps.UserKeys == nil {
		s.respondInternalError(c, "自备密钥功能未启用")
		return
	}
	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	if err := s.deps.UserKeys.Delete(c.Request.Context(), id, user.ID); err != nil {
		s.respondUserKeyError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": id})
}

// validateUserKey 校验自备密钥的字段合法性与 provider 白名单。
//
// 单独抽出而非内联：新增与修改两条路径都要用它，
// 内联两份必然出现"新增校验了、修改漏了"的不一致。
//
// 注意这里把 model.UserKey.Validate 的错误也包成 userKeyValidationError：
// 仓储层的 Create/Update 也会再调一次 Validate，那次的错误会原样冒泡上来，
// 不包一层的话用户改个错别字会看到 500。
func (s *Server) validateUserKey(key *model.UserKey) error {
	if err := key.Validate(); err != nil {
		return validationErr(err.Error())
	}
	if !byok.IsAllowed(key.Provider) {
		return validationErr("该上游暂不支持自备密钥")
	}
	// 地址合法性：不允许改地址的上游不接受自定义接入点（见 byok.ResolveBaseURL）。
	provider, _ := byok.Lookup(key.Provider)
	if _, ok := byok.ResolveBaseURL(provider, key.BaseURL); !ok {
		return validationErr("接入点地址不合法，或该上游不允许自定义地址")
	}
	return nil
}

// userKeyValidationError 标记「用户自己能改正」的字段校验失败。
//
// 为什么要显式类型而不是靠错误文案判断：文案判断既脆弱（改一个字就失效），
// 又危险（数据库错误里恰好含"密钥"二字时会被误判成校验错误，把内部细节回显给用户）。
// 显式类型让"回显文案"这件事只发生在确实该回显的路径上。
type userKeyValidationError struct{ msg string }

func (e userKeyValidationError) Error() string { return e.msg }

func validationErr(msg string) error { return userKeyValidationError{msg: msg} }

// respondUserKeyError 把领域错误翻译成合适的 HTTP 响应。
//
// 铁律：内部错误绝不回显细节（含"主密钥已变更"这类只有站长需要知道的诊断），
// 只在服务端日志留痕。
func (s *Server) respondUserKeyError(c *gin.Context, err error) {
	var verr userKeyValidationError
	switch {
	case errors.Is(err, model.ErrUserKeyNotFound):
		writeUserError(c, http.StatusNotFound,
			"密钥不存在", oai.TypeInvalidRequest, "not_found")
	case errors.Is(err, model.ErrUserKeyTaken):
		writeUserError(c, http.StatusConflict,
			"该上游已配置过密钥，请先停用或删除原有密钥", oai.TypeInvalidRequest, "conflict")
	case errors.As(err, &verr):
		// 字段校验失败属于"用户能自己改正"的问题，直接回显文案有助于自助修复，
		// 因此用 400 而不是 500。
		writeUserError(c, http.StatusBadRequest,
			verr.msg, oai.TypeInvalidRequest, "invalid_request")
	default:
		// 仓储层在 Create/Update 里还会再调一次 key.Validate()，它抛出的
		// 是 fmt.Errorf 的裸错误（没有 store: 前缀），这里按"无前缀"识别。
		// 之所以能这样区分：所有真正的存储故障都经过 fmt.Errorf 包装，
		// 必然带 "store: " 前缀；不带前缀的只可能是字段校验。
		if msg := err.Error(); msg != "" && !strings.HasPrefix(msg, "store: ") {
			writeUserError(c, http.StatusBadRequest,
				msg, oai.TypeInvalidRequest, "invalid_request")
			return
		}
		s.respondInternalError(c, "自备密钥操作失败")
	}
}

// toUserKeyDTO 把领域对象转换为对外视图（掩码，绝不含明文）。
func toUserKeyDTO(key *model.UserKey, now time.Time) userKeyDTO {
	label := key.Provider
	effectiveURL := key.BaseURL
	if provider, ok := byok.Lookup(key.Provider); ok {
		label = provider.Label
		if resolved, ok := byok.ResolveBaseURL(provider, key.BaseURL); ok {
			effectiveURL = resolved
		}
	}

	statusText := "已停用"
	if key.IsEnabled(now) {
		statusText = "启用中"
	} else if key.IsCoolingDown(now) {
		// 熔断要说清"这是平台暂时隔离，不是你关掉了"——
		// 否则用户会去设置里反复开关试图"修复"。
		statusText = "暂时熔断（连续失败过多，稍后自动恢复）"
	}

	models := key.ModelList()
	if models == nil {
		models = []string{}
	}

	return userKeyDTO{
		ID:               key.ID,
		Provider:         key.Provider,
		ProviderLabel:    label,
		Label:            key.Label,
		MaskedKey:        key.MaskedKey(),
		BaseURL:          key.BaseURL,
		EffectiveBaseURL: effectiveURL,
		Models:           models,
		Status:           int(key.Status),
		StatusText:       statusText,
		FailCount:        key.FailCount,
		CooldownUntil:    key.CooldownUntil,
		CoolingDown:      key.IsCoolingDown(now),
		LastUsedAt:       key.LastUsedAt,
		CreatedAt:        key.CreatedAt.Unix(),
		UpdatedAt:        key.UpdatedAt.Unix(),
	}
}

// normalizeModelListInput 归一模型清单输入（逗号/空格分隔 → 逗号分隔、去空白、去重）。
func normalizeModelListInput(raw string) string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';' || r == '，' || r == '\n' || r == '\t'
	})
	seen := make(map[string]struct{}, len(fields))
	kept := make([]string, 0, len(fields))
	for _, f := range fields {
		name := strings.TrimSpace(f)
		if name == "" {
			continue
		}
		// 大小写不敏感去重：模型名不区分大小写时，重复项会造成清单里出现两条。
		lower := strings.ToLower(name)
		if _, dup := seen[lower]; dup {
			continue
		}
		seen[lower] = struct{}{}
		kept = append(kept, name)
	}
	return strings.Join(kept, ",")
}

// derefString 解引用可能为 nil 的字符串指针。
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
