// 本文件是 model.UserKeyRepository 的 SQL 实现（用户自备密钥 BYOK）。
//
// 意图（Why）：
//
//	自备密钥与渠道密钥的安全要求完全一致：明文绝不入库。
//	因此本仓储直接复用注入的 *crypto.Cipher（AES-256-GCM），
//	与 channel_key_repo 保持同一套加密格式与密钥管理方式。
//
// 流转（Flow）：
//
//	server 校验归属与 provider
//	  → Create/Update：Validate → cipher.Encrypt → 落库（只存密文）
//	    → List/Get/Find：读库 → cipher.Decrypt → 返回（含明文，仅内部使用）
//
//	扩展（Extend）：
//
//	支持"一把 Key 多 provider"时，把唯一索引改为 (user_id, provider) 的
//	普通索引，并把 FindEnabledForProvider 改为 ListEnabled 后按序挑选。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiaosu4610/aqua-api/internal/crypto"
	"github.com/xiaosu4610/aqua-api/internal/model"
)

// userKeyRepository 是 model.UserKeyRepository 的 SQL 实现，并发安全。
type userKeyRepository struct {
	db     *sql.DB
	cipher *crypto.Cipher
}

// NewUserKeyRepository 创建用户自备密钥仓储。
func NewUserKeyRepository(db *sql.DB, cipher *crypto.Cipher) model.UserKeyRepository {
	return &userKeyRepository{db: db, cipher: cipher}
}

const userKeyColumns = `id, user_id, provider, label, api_key_enc, base_url, models,
	status, fail_count, cooldown_until, last_used_at, created_at, updated_at`

// Create 新增一条自备密钥（api_key_enc 存密文）。
func (r *userKeyRepository) Create(ctx context.Context, key *model.UserKey) error {
	if err := key.Validate(); err != nil {
		// 不加 store: 前缀：这是"调用方字段不对"，不是"存储层故障"，
		// 上层需要据此返回 400 并回显文案，而不是笼统的 500。
		return err
	}

	encrypted, err := r.cipher.Encrypt(key.APIKey)
	if err != nil {
		return fmt.Errorf("store: 加密自备密钥失败: %w", err)
	}

	now := time.Now()
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO user_keys (user_id, provider, label, api_key_enc, base_url, models,
			status, fail_count, cooldown_until, last_used_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, 0, ?, ?)`,
		key.UserID, strings.TrimSpace(key.Provider), key.Label, encrypted, key.BaseURL,
		key.Models, int(key.Status), now.Unix(), now.Unix())
	if err != nil {
		// 唯一索引冲突 → 同一用户同一 provider 已有启用密钥。
		// 归一成领域错误，让上层能翻译成"请先停用原有密钥"而不是 500。
		if isUniqueViolation(err) {
			return model.ErrUserKeyTaken
		}
		return fmt.Errorf("store: 新增自备密钥失败: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: 读取自备密钥 ID 失败: %w", err)
	}
	key.ID = uint64(id)
	key.CreatedAt = now
	key.UpdatedAt = now
	return nil
}

// GetByID 按主键查询（自动限定 user_id，防止越权读他人密钥）。
func (r *userKeyRepository) GetByID(ctx context.Context, id, userID uint64) (*model.UserKey, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+userKeyColumns+" FROM user_keys WHERE id = ? AND user_id = ?", id, userID)
	key, err := r.scanUserKey(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrUserKeyNotFound
		}
		return nil, fmt.Errorf("store: 读取自备密钥失败: %w", err)
	}
	return key, nil
}

// ListByUser 返回某用户的全部自备密钥（provider 升序）。
func (r *userKeyRepository) ListByUser(ctx context.Context, userID uint64) ([]*model.UserKey, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+userKeyColumns+" FROM user_keys WHERE user_id = ? ORDER BY provider ASC, id ASC", userID)
	if err != nil {
		return nil, fmt.Errorf("store: 列出自备密钥失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]*model.UserKey, 0, 4)
	for rows.Next() {
		key, err := r.scanUserKey(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 读取自备密钥失败: %w", err)
		}
		result = append(result, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历自备密钥失败: %w", err)
	}
	return result, nil
}

// FindEnabledForProvider 返回该用户在该 provider 下的启用密钥（不存在时返回 nil, nil）。
//
// 选路热路径每次请求都会调用，必须走 (user_id, provider, status) 索引。
// 返回 nil 而非错误是刻意的："没有自备密钥"是常态而非异常。
func (r *userKeyRepository) FindEnabledForProvider(ctx context.Context, userID uint64, provider string) (*model.UserKey, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+userKeyColumns+" FROM user_keys WHERE user_id = ? AND provider = ? AND status = ?",
		userID, strings.TrimSpace(provider), int(model.UserKeyStatusEnabled))
	key, err := r.scanUserKey(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: 查找自备密钥失败: %w", err)
	}
	if !key.IsEnabled(time.Now()) {
		// 处于熔断期：视同"当前不可用"，但返回 key 让调用方能给出
		// "密钥暂时熔断"的明确提示，而不是含糊的"无自备密钥"。
		return key, nil
	}
	return key, nil
}

// Update 更新自备密钥。
func (r *userKeyRepository) Update(ctx context.Context, key *model.UserKey) error {
	if err := key.Validate(); err != nil {
		// 同Create：字段错误原样上抛，让上层能翻译成 400 + 可读文案。
		return err
	}

	encrypted, err := r.cipher.Encrypt(key.APIKey)
	if err != nil {
		return fmt.Errorf("store: 加密自备密钥失败: %w", err)
	}

	now := time.Now()
	res, err := r.db.ExecContext(ctx, `
		UPDATE user_keys
		SET provider = ?, label = ?, api_key_enc = ?, base_url = ?, models = ?, status = ?, updated_at = ?
		WHERE id = ? AND user_id = ?`,
		strings.TrimSpace(key.Provider), key.Label, encrypted, key.BaseURL,
		key.Models, int(key.Status), now.Unix(), key.ID, key.UserID)
	if err != nil {
		if isUniqueViolation(err) {
			return model.ErrUserKeyTaken
		}
		return fmt.Errorf("store: 更新自备密钥失败: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		// 记录不存在或不属于该用户——两者对调用方意义相同（找不到）。
		return model.ErrUserKeyNotFound
	}
	key.UpdatedAt = now
	return nil
}

// Delete 删除指定用户的自备密钥。
func (r *userKeyRepository) Delete(ctx context.Context, id, userID uint64) error {
	res, err := r.db.ExecContext(ctx,
		"DELETE FROM user_keys WHERE id = ? AND user_id = ?", id, userID)
	if err != nil {
		return fmt.Errorf("store: 删除自备密钥失败: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return model.ErrUserKeyNotFound
	}
	return nil
}

// MarkFailure 记录一次失败：累加计数，达阈值则熔断（冷却时长随失败次数递增）。
//
// 退避的意义：偶发失败（网络抖动）冷却短，避免不必要地停用一把好 Key；
// 持续失败（Key 真的失效）冷却长，避免持续打无效上游。
func (r *userKeyRepository) MarkFailure(ctx context.Context, id uint64, threshold int, baseCooldown time.Duration) error {
	if threshold <= 0 {
		threshold = 3 // 缺省阈值：连续 3 次失败才熔断
	}
	if baseCooldown <= 0 {
		baseCooldown = 5 * time.Minute
	}

	var failCount int
	err := r.db.QueryRowContext(ctx,
		"SELECT fail_count FROM user_keys WHERE id = ?", id).Scan(&failCount)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.ErrUserKeyNotFound
		}
		return fmt.Errorf("store: 读取自备密钥失败计数失败: %w", err)
	}

	failCount++
	var cooldownUntil int64
	if failCount >= threshold {
		// 指数退避，封顶 24 小时：5m → 10m → 20m … 上限 24h。
		shift := failCount - threshold
		if shift > 6 {
			shift = 6
		}
		cooldown := baseCooldown << shift
		if max := 24 * time.Hour; cooldown > max {
			cooldown = max
		}
		cooldownUntil = time.Now().Add(cooldown).Unix()
	}

	if _, err := r.db.ExecContext(ctx,
		"UPDATE user_keys SET fail_count = ?, cooldown_until = ?, updated_at = ? WHERE id = ?",
		failCount, cooldownUntil, time.Now().Unix(), id); err != nil {
		return fmt.Errorf("store: 更新自备密钥失败计数失败: %w", err)
	}
	return nil
}

// MarkSuccess 记录一次成功：清零失败计数与熔断。
func (r *userKeyRepository) MarkSuccess(ctx context.Context, id uint64) error {
	if _, err := r.db.ExecContext(ctx,
		"UPDATE user_keys SET fail_count = 0, cooldown_until = 0, last_used_at = ?, updated_at = ? WHERE id = ?",
		time.Now().Unix(), time.Now().Unix(), id); err != nil {
		return fmt.Errorf("store: 重置自备密钥失败计数失败: %w", err)
	}
	return nil
}

// scanUserKey 把一行映射为领域对象（并解密 api_key_enc）。
func (r *userKeyRepository) scanUserKey(row rowScanner) (*model.UserKey, error) {
	var (
		id                                     uint64
		userID, failCount                      int
		provider, label, keyEnc, baseURL, mod  string
		status                                 int
		cooldownUntil, lastUsedAt              int64
		createdAt, updatedAt                   int64
	)
	if err := row.Scan(&id, &userID, &provider, &label, &keyEnc, &baseURL, &mod,
		&status, &failCount, &cooldownUntil, &lastUsedAt, &createdAt, &updatedAt); err != nil {
		return nil, err
	}

	plain, err := r.cipher.Decrypt(keyEnc)
	if err != nil {
		// 解密失败通常意味着 AQUA_APP_KEY 被换过。这类记录不能静默丢弃，
		// 也不该把原始密文当密钥用——直接报错，让上层提示"主密钥已变更"。
		return nil, fmt.Errorf("store: 解密自备密钥失败（主密钥是否已变更？）: %w", err)
	}

	return &model.UserKey{
		ID:            id,
		UserID:        uint64(userID),
		Provider:      provider,
		Label:         label,
		APIKey:        plain,
		BaseURL:       baseURL,
		Models:        mod,
		Status:        model.UserKeyStatus(status),
		FailCount:     failCount,
		CooldownUntil: cooldownUntil,
		LastUsedAt:    lastUsedAt,
		CreatedAt:     time.Unix(createdAt, 0),
		UpdatedAt:     time.Unix(updatedAt, 0),
	}, nil
}
