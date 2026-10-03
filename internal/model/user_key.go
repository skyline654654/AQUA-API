// 本文件定义「用户自备密钥（BYOK）」领域模型。
//
// 意图（Why）：
//
//	部分用户愿意用自己的上游额度（例如 NVIDIA 官方 NIM 的 API Key），
//	但仍希望通过本网关的统一协议、鉴权、审计与用量统计来访问。
//	渠道（Channel）表达的是【站点的资产】，无法承载"凭据属于某个用户"
//	这层语义——混进渠道会导致"这个渠道的钱算谁的"无法区分。
//	BYOK 补上这一层：凭据归属用户、加密托管、按用户身份选用。
//
// 流转（Flow）：
//
//	用户在门户添加 → server 校验归属与 provider 合法性
//	  → store 用 cipher.Encrypt 加密落库（与渠道密钥同一套 AES-256-GCM）
//	    → relay 选路时按 (user_id, provider) 命中并 Decrypt 使用
//	      → 调用仍落 usage_logs（保留审计与统计）
//
// 扩展（Extend）：
//
//	支持更多 provider 时，在 Provider 校验处扩展白名单即可；
//	若将来允许一把 Key 覆盖多个 provider，把唯一索引改为 (user_id, provider)。
package model

import (
	"context"
	"errors"
	"strings"
	"time"
)

// UserKeyStatus 是用户自备密钥的启用状态。
type UserKeyStatus int

const (
	// UserKeyStatusEnabled 启用：可参与选路。
	UserKeyStatusEnabled UserKeyStatus = 1
	// UserKeyStatusDisabled 已停用：保留配置但不参与选路。
	UserKeyStatusDisabled UserKeyStatus = 2
)

// 领域错误。
var (
	// ErrUserKeyNotFound 表示未找到该用户自备密钥。
	ErrUserKeyNotFound = errors.New("model: 用户自备密钥不存在")
	// ErrUserKeyTaken 表示同一用户同一 provider 已有启用中的密钥。
	ErrUserKeyTaken = errors.New("model: 该用户已为此 provider 配置了密钥")
)

// UserKey 是用户自备并托管在平台的上游凭据。
//
// 安全约束：APIKey 仅存在于内存中，仓库层负责加密后落库；
// 任何对外 DTO 都绝不能包含明文（见 server 层 toUserKeyDTO）。
type UserKey struct {
	ID     uint64
	UserID uint64
	// Provider 是上游标识（如 nvidia）。空串视为非法——
	// 没有 provider 就无法决定请求发往哪个上游，配置等于无效。
	Provider string
	// Label 是用户自定义备注（如"生产环境"），仅自己可见，便于分辨多把 Key。
	Label string
	// APIKey 是明文凭据（仅在服务内部流转，绝不出现在响应里）。
	APIKey string
	// BaseURL 是自定义接入点；空串表示用 provider 的默认地址。
	// 存在的意义：NVIDIA 等官方端点有多个区域/版本接入点，
	// 用户可能因账号区域而需要指向不同地址。
	BaseURL string
	// Models 是该 Key 可调用的模型清单（逗号分隔）；空串表示不限制。
	Models string
	Status UserKeyStatus
	// FailCount 是连续失败次数（鉴权失败等），达到阈值触发熔断。
	FailCount int
	// CooldownUntil 是熔断恢复时间（Unix 秒）；0 = 未熔断。
	CooldownUntil int64
	LastUsedAt  int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// IsEnabled 判断该密钥当前是否可参与选路。
//
// 停用与熔断是两回事：停用是用户主动关掉，熔断是平台因连续失败
// 暂时隔离——都表现为"不可用"，但在界面上要能区分原因。
func (k *UserKey) IsEnabled(now time.Time) bool {
	if k == nil || k.Status != UserKeyStatusEnabled {
		return false
	}
	if k.CooldownUntil > 0 && now.Unix() < k.CooldownUntil {
		return false
	}
	return true
}

// IsCoolingDown 判断该密钥是否处于熔断期（用于给用户明确的提示文案）。
func (k *UserKey) IsCoolingDown(now time.Time) bool {
	return k != nil && k.CooldownUntil > 0 && now.Unix() < k.CooldownUntil
}

// MaskedKey 返回掩码形式的凭据（界面上显示用，绝不回显明文）。
//
// 规则与渠道密钥一致：短 Key 只显示首尾，长 Key 保留头尾各 4 位。
func (k *UserKey) MaskedKey() string {
	key := strings.TrimSpace(k.APIKey)
	if len(key) <= 8 {
		if key == "" {
			return ""
		}
		return key[:1] + "***"
	}
	return key[:4] + "***" + key[len(key)-4:]
}

// ModelList 返回该 Key 允许调用的模型清单（已拆分去空白）。
func (k *UserKey) ModelList() []string {
	if k == nil || strings.TrimSpace(k.Models) == "" {
		return nil
	}
	parts := strings.Split(k.Models, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// AllowsModel 判断该 Key 是否允许调用指定模型。
//
// 空清单 = 不限制（该 provider 的任意模型都可以尝试）；
// 有清单时必须精确匹配——BYOK 的语义是"用户指定自己的额度范围"，
// 越界调用会让用户意外消耗他没预期的模型。
func (k *UserKey) AllowsModel(modelName string) bool {
	if k == nil {
		return false
	}
	list := k.ModelList()
	if len(list) == 0 {
		return true
	}
	name := strings.TrimSpace(modelName)
	for _, m := range list {
		if m == name {
			return true
		}
	}
	return false
}

// Validate 校验用户自备密钥的字段合法性。
func (k *UserKey) Validate() error {
	if k == nil {
		return errors.New("密钥不能为空")
	}
	if k.UserID == 0 {
		return errors.New("密钥必须归属某个用户")
	}
	if strings.TrimSpace(k.Provider) == "" {
		return errors.New("provider 不能为空")
	}
	if strings.TrimSpace(k.APIKey) == "" {
		return errors.New("密钥内容不能为空")
	}
	if k.Status != UserKeyStatusEnabled && k.Status != UserKeyStatusDisabled {
		return errors.New("密钥状态非法")
	}
	return nil
}

// UserKeyRepository 定义用户自备密钥的持久化操作。
type UserKeyRepository interface {
	// Create 新增一条自备密钥，成功后回填 ID 与时间戳。
	// 同一用户同一 provider 已有启用密钥时返回 ErrUserKeyTaken。
	Create(ctx context.Context, key *UserKey) error

	// GetByID 按主键查询；不存在时返回 ErrUserKeyNotFound。
	// 返回值含明文 APIKey，仅供服务内部转发使用。
	GetByID(ctx context.Context, id, userID uint64) (*UserKey, error)

	// ListByUser 返回某用户的全部自备密钥（按 provider 升序）。
	ListByUser(ctx context.Context, userID uint64) ([]*UserKey, error)

	// FindEnabledForProvider 返回该用户在该 provider 下的启用密钥（可能为 nil）。
	//
	// 选路热路径每请求都会调用，因此实现必须走 (user_id, provider) 索引。
	FindEnabledForProvider(ctx context.Context, userID uint64, provider string) (*UserKey, error)

	// Update 更新自备密钥（密钥内容、备注、模型清单、状态等）。
	Update(ctx context.Context, key *UserKey) error

	// Delete 删除指定用户的自备密钥；不存在时返回 ErrUserKeyNotFound。
	Delete(ctx context.Context, id, userID uint64) error

	// MarkFailure 记录一次失败：累加失败次数，达阈值则进入熔断。
	//
	// 熔断时长随失败次数递增（退避）：偶发失败冷却短，
	// 持续失败冷却长——避免"Key 真的失效了还在持续打上游"。
	MarkFailure(ctx context.Context, id uint64, threshold int, baseCooldown time.Duration) error

	// MarkSuccess 记录一次成功：清零失败次数与熔断。
	MarkSuccess(ctx context.Context, id uint64) error
}
