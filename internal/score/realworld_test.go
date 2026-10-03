// 真实生产数据回归测试（2026-10-03 用户实际榜单截图）。
//
// 意图（Why）：
//
//	抽象数据集（A/B/E 系列）验证的是**公式**，但真实榜单会暴露抽象数据看不到的
//	分布形态：极端离群值（某人 token 是榜尾的 20 倍）、单维碾压（token 第一但请求垫底）、
//	大量用户聚集在低分段。v2.3（P90 锚点）在这批数据上就会出现"一整列 100 分"
//	和"榜首反而不是最强用户"的错觉——这两点都已由本文件锁死。
//
//	数据来源：用户提供的线上截图（付费榜 8 人 / 免费榜 8 人的请求数与 Token）。
//
// 流转（Flow）：
//
//	go test ./internal/score/ -run RealWorld -v
package score

import (
	"math"
	"testing"
)

// 真实付费榜：token 与请求呈反向分布，skyline117 两维都最强。
// 旧算法下 skyline117 被排在第 7、榜首是 token 排第 2 的 Alistair——纯线性归一
// 把"请求数"和"token"当同量纲相加，量纲小的维度被淹没。
var realPaid = []Entry{
	{"Alistair_MioFog", 111454718, 1661},
	{"huan102916tcuyi", 92891, 114},
	{"FaiaMorgana", 0, 126},
	{"易123", 16462, 27},
	{"ssq350624", 47834515, 362},
	{"xiaomiao", 1388367, 577},
	{"skyline117", 990097957, 2383},
	{"Tauru", 14546749, 4293},
}

// 真实免费榜：token 量级高度接近（4.9M~273M），请求数离散（432~1601）。
// 旧算法下 8 人里有 8 个拿到 100 分，榜单完全失去区分度。
var realFree = []Entry{
	{"MFScelebrateDevOp", 273454306, 639},
	{"zbs", 143853444, 953},
	{"CDDEDEG", 56545818, 881},
	{"1145", 55162401, 1601},
	{"ApolloO", 55127809, 432},
	{"33/hl", 53440726, 943},
	{"sx", 53068766, 444},
	{"360zx", 49252084, 1151},
}

// TestRealWorld_Free_NoScoreFlood 验证免费榜不再"满分泛滥"。
//
// 回归背景：8 个用户全部 100.0 分，界面上完全看不出谁强谁弱——
// 根因是 v2.3 用 P90 作上锚点，8 人里 6 人超过 P90，于是集体触顶。
func TestRealWorld_Free_NoScoreFlood(t *testing.T) {
	res := ScoreBoard(realFree)

	// 硬要求：满分行数必须为 0
	if n := countAtOrAbove(res.Rows, DefaultConfig.ScoreMax); n != 0 {
		t.Errorf("出现 %d 个满分（≥100）用户，榜单会再次退化为「一片满分」", n)
	}
	// 硬要求：榜首 = Cap(99)，不满分
	if top := res.Rows[0]; top.Total > DefaultConfig.Cap+tolerance {
		t.Errorf("榜首 = %.2f，超过 Cap(%.0f)", top.Total, DefaultConfig.Cap)
	}
	// 硬要求：分数必须真正分散（至少 5 个不同分值，且极差 ≥ 30 分）
	if distinct := distinctScores(res.Rows); distinct < 5 {
		t.Errorf("只有 %d 个不同分值，区分度不足（期望 ≥5）", distinct)
	}
	if span := res.Rows[0].Total - res.Rows[len(res.Rows)-1].Total; span < 30 {
		t.Errorf("榜首与榜尾极差 = %.2f，区分度不足（期望 ≥30）", span)
	}
	// 榜尾必须落 floor 附近（20）
	if last := res.Rows[len(res.Rows)-1]; last.Total > DefaultConfig.Floor+15 {
		t.Errorf("榜尾 = %.2f，未接近 floor(%.0f)，低分端被抬高", last.Total, DefaultConfig.Floor)
	}
}

// TestRealWorld_Paid_RankByCombinedStrength 验证付费榜排序正确。
//
// 回归背景：skyline117（2383 请求 / 990M token，两维都最强）在旧算法下
// 被排到第 7 位，榜首是 token 排第 2 的 Alistair_MioFog，
// 且 Alistair 的综合分显示为 0.0——排序与分数同时失去意义。
func TestRealWorld_Paid_RankByCombinedStrength(t *testing.T) {
	res := ScoreBoard(realPaid)

	// 榜首必须是两维都强的 skyline117
	if got := res.Rows[0].ID; got != "skyline117" {
		t.Errorf("榜首 = %s，期望 skyline117（2383 请求 / 990M token，两维都最强）", got)
	}
	// 榜首应拿 Cap（接近满分）
	if math.Abs(res.Rows[0].Total-DefaultConfig.Cap) > tolerance {
		t.Errorf("榜首 = %.2f，期望 Cap(%.0f)", res.Rows[0].Total, DefaultConfig.Cap)
	}
	// 分数必须单调不增（名次顺序 = 分数顺序）
	for i := 1; i < len(res.Rows); i++ {
		if res.Rows[i].Total > res.Rows[i-1].Total+tolerance {
			t.Errorf("第 %d 名(%.2f) 分数高于第 %d 名(%.2f)，排序错乱",
				i+1, res.Rows[i].Total, i, res.Rows[i-1].Total)
		}
	}
	// 无满分
	if n := countAtOrAbove(res.Rows, DefaultConfig.ScoreMax); n != 0 {
		t.Errorf("出现 %d 个满分用户", n)
	}
}

// TestRealWorld_DynamicOnRealData 验证真实数据上的动态性。
//
// 场景：榜首 skyline117 的 token 翻 10 倍（模拟重度用户），
// 其余所有人的分数必须被压低——这是"分数反映离榜首多远"的核心语义。
func TestRealWorld_DynamicOnRealData(t *testing.T) {
	before := ScoreBoard(realPaid)

	boosted := append([]Entry(nil), realPaid...)
	for i := range boosted {
		if boosted[i].ID == "skyline117" {
			boosted[i].Tokens *= 10
		}
	}
	after := ScoreBoard(boosted)

	// 锚点必须随之抬高
	if after.Anchors.TopTok <= before.Anchors.TopTok {
		t.Errorf("上锚未随榜首抬升：%.0f → %.0f", before.Anchors.TopTok, after.Anchors.TopTok)
	}

	// 除榜首外，**处于中段**的用户分数被压低。
	//
	// 刻意的例外：token 全局最低的 FaiaMorgana（token=0）无论榜首涨多少都恒在
	// 下锚点之下 → 恒为 floor，"被压低"无从发生——这是算法的正确行为，
	// 而非缺陷。若把它也算进断言，就等于要求 floor 分随榜首变化，与
	// "floor 表示"触底""的语义冲突。
	for _, e := range realPaid {
		if e.ID == "skyline117" {
			continue
		}
		beforeTok := byID(before.Rows, e.ID).ScoreTok
		if beforeTok <= DefaultConfig.Floor+tolerance {
			continue // 触底用户：分数恒为 floor，不参与"被压低"判定
		}
		b := byID(before.Rows, e.ID).Total
		a := byID(after.Rows, e.ID).Total
		if a >= b-tolerance {
			t.Errorf("%s 的总分应被压低：%.2f → %.2f（榜首用量涨了 10 倍）", e.ID, b, a)
		}
	}
	// 且被压低的用户至少要有一个（否则说明动态性整体失效）
	lowered := false
	for _, e := range realPaid {
		if e.ID == "skyline117" {
			continue
		}
		if byID(before.Rows, e.ID).ScoreTok <= DefaultConfig.Floor+tolerance {
			continue
		}
		if byID(after.Rows, e.ID).Total < byID(before.Rows, e.ID).Total-tolerance {
			lowered = true
			break
		}
	}
	if !lowered {
		t.Error("没有任何中段用户被压低，动态性失效")
	}
}

// TestRealWorld_BothBoardsNoFullScore 两榜同时验证：均无满分、榜首为 Cap。
func TestRealWorld_BothBoardsNoFullScore(t *testing.T) {
	for name, ds := range map[string][]Entry{"付费榜": realPaid, "免费榜": realFree} {
		res := ScoreBoard(ds)
		if n := countAtOrAbove(res.Rows, DefaultConfig.ScoreMax); n != 0 {
			t.Errorf("%s 出现 %d 个满分用户", name, n)
		}
		if top := res.Rows[0]; math.Abs(top.Total-DefaultConfig.Cap) > tolerance {
			t.Errorf("%s 榜首 = %.2f，期望 Cap(%.0f)", name, top.Total, DefaultConfig.Cap)
		}
	}
}

// countAtOrAbove 统计分数 ≥ threshold 的行数。
func countAtOrAbove(rows []Row, threshold float64) int {
	n := 0
	for _, r := range rows {
		if r.Total >= threshold-tolerance {
			n++
		}
	}
	return n
}

// distinctScores 统计不同分值的个数（四舍五入到 0.1）。
func distinctScores(rows []Row) int {
	seen := map[int]bool{}
	for _, r := range rows {
		seen[int(r.Total*10)] = true
	}
	return len(seen)
}
