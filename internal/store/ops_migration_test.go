// 本文件验证「智能运营」新增的五张表与关键索引确实被迁移创建。
//
// 意图（Why）：
//
//	这些表是成本归因、滥用检测、动态权重、预警与 BYOK 的地基。
//	地基缺失时报错发生在首次请求（表现为 500 或"功能未启用"），
//	而不是启动期——那种失败很难在第一时间联想到"迁移没跑"。
//	把表与索引的存在性钉进测试，缺表会在 CI 就暴露。
//
//	为什么只测「存在性」而不测业务语义：
//	业务语义由各仓储的测试覆盖（见 insights_repo / user_key_repo 的测试），
//	这里只负责回答"结构到位了吗"，两者职责不重叠。
package store

import (
	"context"
	"testing"
)

// TestMigrate_运营表与索引存在 验证五张新表及其关键索引已建立。
func TestMigrate_运营表与索引存在(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tables := []struct {
		name string
		why  string
	}{
		{"user_keys", "BYOK：用户自备密钥的加密落库"},
		{"abuse_events", "盗 Key / 突发检测的事件留痕"},
		{"channel_health_samples", "动态权重的 EWMA 输入桶"},
		{"alert_notifications", "预警记录与冷却去重"},
	}
	for _, tbl := range tables {
		var count int
		if err := st.DB().QueryRowContext(ctx,
			"SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name = ?", tbl.name).Scan(&count); err != nil {
			t.Fatalf("查询表 %s 失败: %v", tbl.name, err)
		}
		if count != 1 {
			t.Errorf("表 %s 未创建（%s）", tbl.name, tbl.why)
		}
	}

	// 索引：它们不是"可选优化"，缺了会直接退化成全表扫。
	idxs := []struct {
		name string
		why  string
	}{
		// 选路热路径：每个请求都要按 (user_id, provider) 定位凭据
		{"idx_user_keys_user_provider", "BYOK 选路热路径"},
		// 唯一索引是"发信闸门"：没有它并发下会发出重复预警
		{"idx_alert_user_kind_bucket", "预警冷却去重（唯一索引）"},
		// 采样桶必须唯一，否则 upsert 会退化成插入多行
		{"idx_channel_health_channel_bucket", "采样桶唯一性（upsert 前提）"},
		// 成本归因的聚合路径
		{"idx_usage_logs_user_tag", "成本归因按 (用户, 标签) 聚合"},
	}
	for _, idx := range idxs {
		var count int
		if err := st.DB().QueryRowContext(ctx,
			"SELECT COUNT(1) FROM sqlite_master WHERE type='index' AND name = ?", idx.name).Scan(&count); err != nil {
			t.Fatalf("查询索引 %s 失败: %v", idx.name, err)
		}
		if count != 1 {
			t.Errorf("索引 %s 未创建（%s）", idx.name, idx.why)
		}
	}
}

// TestMigrate_usage_logs含tag列 验证成本归因所需的 tag 列已加到既有的 usage_logs。
//
// 刻意单独测：ALTER TABLE ADD COLUMN 是"改既有表"的迁移，
// 与"建新表"是两类失败模式（前者可能因列已存在而报错，后者不会）。
func TestMigrate_usage_logs含tag列(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	rows, err := st.DB().QueryContext(ctx, "SELECT tag FROM usage_logs LIMIT 1")
	if err != nil {
		t.Fatalf("查询 usage_logs.tag 失败（迁移 0050 未应用？）: %v", err)
	}
	defer func() { _ = rows.Close() }()

	// 列存在时Query 不会报错；空表时 rows 为空是正常的。
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			t.Fatalf("遍历 usage_logs 失败: %v", err)
		}
	}
}
