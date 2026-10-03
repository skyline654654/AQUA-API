// 本文件是「用量预测」的单元测试。
//
// 意图（Why）：
//
//	estimateDaysLeft 的返回值直接决定"是否给用户发预警邮件"。
//	算错有两个方向，且都有真实代价：
//	  - 偏乐观（少算天数）：用户以为还有时间，结果当天就断——预警失去意义；
//	  - 偏悲观（多算天数）：用户被无谓地催促充值，产生反感并关闭邮件。
//	另一个方向的错误更糟：把"无法预测"（返回 -1）当成"已耗尽"（0），
//	会给刚注册、还没用过的用户群发"余额不足"邮件——这是事故级事故。
package server

import (
	"testing"
	"time"
)

// timeAt 是构造固定时刻的小助手（避免测试里散落 time.Date 的长参数）。
func timeAt(y int, m time.Month, d, h, min, s int) time.Time {
	return time.Date(y, m, d, h, min, s, 0, time.UTC)
}

func TestEstimateDaysLeft_NormalCases(t *testing.T) {
	tests := []struct {
		name         string
		remain       int64
		daily        int64
		want         int
	}{
		{"整除：1000余额/100每天= 10 天", 1000, 100, 10},
		{"不整除向上取整：1000/300 = 4 天", 1000, 300, 4},
		{"刚好一天", 100, 100, 1},
		{"只剩一点：10/1000 = 1 天（不取 0）", 10, 1000, 1},
		{"余量充足：100000/10 = 10000 天", 100000, 10, 10000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := estimateDaysLeft(tt.remain, tt.daily); got != tt.want {
				t.Errorf("estimateDaysLeft(%d, %d) = %d, 期望 %d",
					tt.remain, tt.daily, got, tt.want)
			}
		})
	}
}

func TestEstimateDaysLeft_AlreadyDrained(t *testing.T) {
	// 余额已耗尽（≤0）→ 返回 0，语义是"已经没钱了"而非"还能用 0 天"。
	if got := estimateDaysLeft(0, 100); got != 0 {
		t.Errorf("余额为 0 时返回 %d, 期望 0", got)
	}
	if got := estimateDaysLeft(-50, 100); got != 0 {
		t.Errorf("余额为负时返回 %d, 期望 0（透支不应被当成还能用）", got)
	}
}

func TestEstimateDaysLeft_UnpredictableReturnsMinusOne(t *testing.T) {
	// 日均消耗 ≤ 0 → 返回 -1（无法预测）。
	// 这条最关键：调用方若把 -1 当0 处理，会给从未用过的用户
	// 发"余额不足"邮件。测试把这条语义钉死。
	if got := estimateDaysLeft(1000, 0); got != -1 {
		t.Errorf("日均消耗为 0 时返回 %d, 期望 -1（不可预测）", got)
	}
	if got := estimateDaysLeft(1000, -50); got != -1 {
		t.Errorf("日均消耗为负时返回 %d, 期望 -1（不可预测）", got)
	}
	// 且必须优先于"余额≤0"的判定顺序问题：
	// 余额为 0 且日均为 0 时，语义应是"已耗尽"（0）而非"不可预测"，
	// 因为"钱用完了"是确定事实，比"还能用几天"更重要。
	if got := estimateDaysLeft(0, 0); got != 0 {
		t.Errorf("余额与日均为 0 时返回 %d, 期望 0（已耗尽是确定事实）", got)
	}
}

func TestAlertWindowBucket_SameKindSameWindowSameBucket(t *testing.T) {
	// 冷却语义：同类预警在同一窗口内必须得到相同的桶键，
	// 否则唯一索引拦不住重复发送。
	now := timeAt(2026, 10, 3, 14, 0, 0)
	b1 := alertWindowBucket("quota_drain", now)
	b2 := alertWindowBucket("quota_drain", now.Add(time.Hour))
	// 紧急预警冷却 6 小时，1 小时后仍在同一桶内
	if b1 != b2 {
		t.Errorf("同类预警 1 小时后落入不同桶: %d vs %d", b1, b2)
	}
}

func TestAlertWindowBucket_CrossesWindowBoundary(t *testing.T) {
	// 跨过冷却窗口后必须换桶，否则用户永远收不到第二封。
	start := timeAt(2026, 10, 3, 0, 0, 0)
	b1 := alertWindowBucket("quota_drain", start)
	b2 := alertWindowBucket("quota_drain", start.Add(7*time.Hour))
	if b1 == b2 {
		t.Errorf("跨 7 小时后仍在同一桶（冷却窗口 %d 小时失效）", quotaAlertDrainCooldownHours)
	}
}

func TestAlertCooldownHours_EmergencyIsMoreFrequent(t *testing.T) {
	// 紧急预警（即将耗尽）必须比普通预警（余额不足）更频繁——
	// 它的价值恰恰在于"反复确认用户看到了"。
	drain := alertCooldownHours("quota_drain")
	low := alertCooldownHours("quota_low")
	if drain >= low {
		t.Errorf("紧急预警冷却(%dh) 应短于普通预警(%dh)", drain, low)
	}
}
