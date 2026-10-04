// 本文件实现对局与棋谱的持久化。
//
// 意图（Why）：
//
//	对局的状态推进必须【原子】。一手的落地包含两件事：追加棋谱、更新快照。
//	若分两次写而中途失败，会留下"棋谱有这一手、快照里却没有"的不一致——
//	而恢复对局是以快照为准的，于是那一手被静默丢弃，复盘时表现为
//	"某一步莫名其妙没生效"。因此 AdvanceWithMove 用单事务完成两件事。
//
// 流转（Flow）：
//
//	server/handler_game → model.GameMatchRepository（本文件的实现）→ SQLite
//
// 扩展（Extend）：
//
//	新增列：先新开迁移脚本，再同步本文件的列清单常量与 scanMatch/scanMove。
//	注意列清单是【手写】的（不使用 SELECT *）：SELECT * 会让新增列后
//	Scan 的参数个数与顺序静默错位，是这类代码最经典的坑。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/xiaosu4610/aqua-api/internal/model"
)

// gameMatchColumns 是 game_matches 的显式列清单。
//
// 刻意不写 SELECT *：新增列时若忘了同步 Scan，SELECT * 会让列与参数
// 静默错位（把 A 列读进 B 字段），而且不会报错——最难查的一类缺陷。
const gameMatchColumns = `id, user_id, kind, mode, black_model, white_model,
	human_color, status, winner, move_count, error_text, state_json,
	created_at, updated_at`

// gameMoveColumns 是 game_moves 的显式列清单。
const gameMoveColumns = `id, match_id, seq, color, notation, raw_output,
	attempts, error_text, created_at`

type gameRepo struct {
	db *sql.DB
}

// NewGameMatchRepository 创建对局仓储。
func NewGameMatchRepository(db *sql.DB) model.GameMatchRepository {
	return &gameRepo{db: db}
}

func (r *gameRepo) Create(ctx context.Context, match *model.GameMatch) error {
	if match == nil {
		return errors.New("store: 对局为空")
	}
	now := time.Now()
	if match.CreatedAt.IsZero() {
		match.CreatedAt = now
	}
	match.UpdatedAt = now

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO game_matches
			(user_id, kind, mode, black_model, white_model, human_color,
			 status, winner, move_count, error_text, state_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		match.UserID, match.Kind, match.Mode, match.BlackModel, match.WhiteModel,
		match.HumanColor, match.Status, match.Winner, match.MoveCount,
		match.ErrorText, match.StateJSON, match.CreatedAt.Unix(), match.UpdatedAt.Unix())
	if err != nil {
		return fmt.Errorf("store: 创建对局失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: 读取对局 ID 失败: %w", err)
	}
	match.ID = uint64(id)
	return nil
}

func (r *gameRepo) GetByID(ctx context.Context, userID, id uint64) (*model.GameMatch, error) {
	// 查询条件带 user_id：越权读取他人对局必须在这里就断掉，
	// 不能只靠上层记得校验（上层漏一处就是一个越权漏洞）。
	row := r.db.QueryRowContext(ctx,
		`SELECT `+gameMatchColumns+` FROM game_matches WHERE id = ? AND user_id = ?`, id, userID)
	m, err := scanGameMatch(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrGameMatchNotFound
		}
		return nil, err
	}
	return m, nil
}

func (r *gameRepo) ListByUser(ctx context.Context, userID uint64, limit int) ([]*model.GameMatch, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+gameMatchColumns+` FROM game_matches
		 WHERE user_id = ? ORDER BY id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询对局列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]*model.GameMatch, 0, limit)
	for rows.Next() {
		m, err := scanGameMatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *gameRepo) Update(ctx context.Context, match *model.GameMatch) error {
	if match == nil {
		return errors.New("store: 对局为空")
	}
	match.UpdatedAt = time.Now()
	_, err := r.db.ExecContext(ctx, `
		UPDATE game_matches SET
			status = ?, winner = ?, move_count = ?, error_text = ?,
			state_json = ?, updated_at = ?
		WHERE id = ? AND user_id = ?`,
		match.Status, match.Winner, match.MoveCount, match.ErrorText,
		match.StateJSON, match.UpdatedAt.Unix(), match.ID, match.UserID)
	if err != nil {
		return fmt.Errorf("store: 更新对局失败: %w", err)
	}
	return nil
}

func (r *gameRepo) Delete(ctx context.Context, userID, id uint64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 先删棋谱再删对局：虽然棋谱没有外键约束（SQLite 默认不开外键），
	// 顺序仍然重要——反过来的话对局已消失而棋谱残留，成了孤儿数据。
	if _, err := tx.ExecContext(ctx, `DELETE FROM game_moves WHERE match_id = ?`, id); err != nil {
		return fmt.Errorf("store: 删除棋谱失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM game_matches WHERE id = ? AND user_id = ?`, id, userID); err != nil {
		return fmt.Errorf("store: 删除对局失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交删除失败: %w", err)
	}
	return nil
}

func (r *gameRepo) ListMoves(ctx context.Context, matchID uint64) ([]*model.GameMoveRecord, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+gameMoveColumns+` FROM game_moves WHERE match_id = ? ORDER BY seq ASC`, matchID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询棋谱失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*model.GameMoveRecord
	for rows.Next() {
		rec, err := scanGameMove(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// AdvanceWithMove 在同一事务内追加棋谱并更新对局。
//
// 这是本仓储唯一会"写两处"的方法，因此必须原子（见文件头说明）。
func (r *gameRepo) AdvanceWithMove(ctx context.Context, match *model.GameMatch, rec *model.GameMoveRecord) error {
	if match == nil || rec == nil {
		return errors.New("store: 对局或棋谱为空")
	}
	now := time.Now()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	match.UpdatedAt = now

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO game_moves
			(match_id, seq, color, notation, raw_output, attempts, error_text, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		match.ID, rec.Seq, rec.Color, rec.Notation, rec.RawOutput,
		rec.Attempts, rec.ErrorText, rec.CreatedAt.Unix())
	if err != nil {
		// 唯一索引 (match_id, seq) 冲突说明并发推进。
		// 这不是"数据损坏"，而是"有人抢先走了一步"——把它翻译成可识别的错误，
		// 让上层回一句"该局面已被推进，请刷新"而不是 500。
		return fmt.Errorf("%w: 追加棋谱失败（seq=%d）: %v", ErrGameMoveConflict, rec.Seq, err)
	}
	if id, idErr := res.LastInsertId(); idErr == nil {
		rec.ID = uint64(id)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE game_matches SET
			status = ?, winner = ?, move_count = ?, error_text = ?,
			state_json = ?, updated_at = ?
		WHERE id = ?`,
		match.Status, match.Winner, match.MoveCount, match.ErrorText,
		match.StateJSON, match.UpdatedAt.Unix(), match.ID); err != nil {
		return fmt.Errorf("store: 更新对局失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交对局推进失败: %w", err)
	}
	return nil
}

// ErrGameMoveConflict 表示同一对局的同一手已被写入（并发推进）。
//
// 单独定义而不是直接返回底层 SQL 错误：上层要据此给出"请刷新重试"这类
// 明确提示，而不是把 "UNIQUE constraint failed: ..." 抛给用户。
var ErrGameMoveConflict = errors.New("store: 该局面已被推进")

// scanGameMatch 把一行读成对局实体。
func scanGameMatch(row rowScanner) (*model.GameMatch, error) {
	var (
		m                    model.GameMatch
		createdAt, updatedAt int64
	)
	if err := row.Scan(
		&m.ID, &m.UserID, &m.Kind, &m.Mode, &m.BlackModel, &m.WhiteModel,
		&m.HumanColor, &m.Status, &m.Winner, &m.MoveCount, &m.ErrorText,
		&m.StateJSON, &createdAt, &updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取对局失败: %w", err)
	}
	m.CreatedAt = time.Unix(createdAt, 0)
	m.UpdatedAt = time.Unix(updatedAt, 0)
	return &m, nil
}

// scanGameMove 把一行读成棋谱记录。
func scanGameMove(row rowScanner) (*model.GameMoveRecord, error) {
	var (
		rec       model.GameMoveRecord
		createdAt int64
	)
	if err := row.Scan(
		&rec.ID, &rec.MatchID, &rec.Seq, &rec.Color, &rec.Notation,
		&rec.RawOutput, &rec.Attempts, &rec.ErrorText, &createdAt,
	); err != nil {
		return nil, fmt.Errorf("store: 读取棋谱失败: %w", err)
	}
	rec.CreatedAt = time.Unix(createdAt, 0)
	return &rec, nil
}
