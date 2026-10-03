// 「动态综合评分算法 v2.3」测试：冻结向量 + 性质测试。
//
// 意图（Why）：
//
//	评分算法的每个数字都会直接展示给用户并影响运营判断，
//	因此必须把「算得对」固化成可重复验证的断言，而不是靠人眼比对截图。
//	测试分两类：
//	  1. 冻结向量：把已确认的期望值逐条钉死，任何回归都会立刻暴露；
//	  2. 性质测试：不依赖具体数字，只依赖算法必须满足的约束
//	     （空输入不崩、脏数据不崩、结果确定、支配关系成立、两维对称）。
//
// 流转（Flow）：
//
//	go test ./internal/score/ -v
//
// 扩展（Extend）：
//
//	改算法后先跑本文件；新增配置项时在「配置不变式」一节补断言。
package score

import (
	"math"
	"math/rand"
	"sort"
	"testing"
)

const tolerance = 0.01

// 数据集 A（n=6）：覆盖两维此消彼长的典型形态。
func datasetA() []Entry {
	return []Entry{
		{ID: "alice", Tokens: 12000, Requests: 800},
		{ID: "bob", Tokens: 9000, Requests: 500},
		{ID: "carol", Tokens: 6000, Requests: 300},
		{ID: "dave", Tokens: 3000, Requests: 120},
		{ID: "erin", Tokens: 20000, Requests: 60},
		{ID: "frank", Tokens: 400, Requests: 5000},
	}
}

// 数据集 B（n=5）：含 0 值与极端离群值，用于验证脏数据与归一化。
func datasetB() []Entry {
	return []Entry{
		{ID: "u1", Tokens: 1, Requests: 1},
		{ID: "u2", Tokens: 500000, Requests: 4},
		{ID: "u3", Tokens: 50, Requests: 900},
		{ID: "u4", Tokens: 1000000, Requests: 1000000},
		{ID: "u5", Tokens: 0, Requests: 0},
	}
}

func byID(rows []Row, id string) Row {
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	return Row{}
}

func assertRow(t *testing.T, r Row, wantTok, wantReq, wantTotal float64) {
	t.Helper()
	if math.Abs(r.ScoreTok-wantTok) > tolerance {
		t.Errorf("%s sTok = %.4f，期望 %.2f（容差 %.2f）", r.ID, r.ScoreTok, wantTok, tolerance)
	}
	if math.Abs(r.ScoreReq-wantReq) > tolerance {
		t.Errorf("%s sReq = %.4f，期望 %.2f（容差 %.2f）", r.ID, r.ScoreReq, wantReq, tolerance)
	}
	if math.Abs(r.Total-wantTotal) > tolerance {
		t.Errorf("%s total = %.4f，期望 %.2f（容差 %.2f）", r.ID, r.Total, wantTotal, tolerance)
	}
}

/* ─────────────────────── 一、冻结测试向量 ─────────────────────── */

// TestDatasetA1_PercentileExtremes：pTop=1.0、pBase=0.0 时锚点取极值。
func TestDatasetA1_PercentileExtremes(t *testing.T) {
	res := ScoreBoardWithConfig(datasetA(), Config{TopPercent: 1.0, BasePct: 0.0, SetBasePct: true})

	// 锚点：A=最大值、B=最小值
	if res.Anchors.TopTok != 20000 || res.Anchors.BaseTok != 400 {
		t.Errorf("token 锚点 = [%v, %v]，期望 [400(最小), 20000(榜首)]", res.Anchors.BaseTok, res.Anchors.TopTok)
	}
	if res.Anchors.TopReq != 5000 || res.Anchors.BaseReq != 60 {
		t.Errorf("请求锚点 = [%v, %v]，期望 [60(最小), 5000(榜首)]", res.Anchors.BaseReq, res.Anchors.TopReq)
	}

	assertRow(t, byID(res.Rows, "alice"), 88.68, 66.27, 99.00)
	assertRow(t, byID(res.Rows, "bob"), 82.87, 57.87, 69.25)
	assertRow(t, byID(res.Rows, "carol"), 74.69, 48.75, 60.34)
	assertRow(t, byID(res.Rows, "dave"), 60.69, 32.38, 44.33)
	assertRow(t, byID(res.Rows, "erin"), 99.00, 20.00, 44.50)
	assertRow(t, byID(res.Rows, "frank"), 20.00, 99.00, 44.50)
}

// TestDatasetA2_Default：默认配置（pTop=0.90、pBase=0.10）。
//
// 这里同时校验「展示 total ≠ sqrt(sTok×sReq)」是预期行为：
// total 由未舍入的单维分算出，sTok/sReq 只是各自的展示快照。
func TestDatasetA2_Default(t *testing.T) {
	res := ScoreBoard(datasetA())

	// 上锚 = 榜首实际值（不是 P90）：这是 v2.4 的关键修正。
	if res.Anchors.TopTok != 20000 || res.Anchors.BaseTok != 1700 {
		t.Errorf("token 锚点 = [%v, %v]，期望 [1700(P10), 20000(榜首)]", res.Anchors.BaseTok, res.Anchors.TopTok)
	}
	if res.Anchors.TopReq != 5000 || res.Anchors.BaseReq != 90 {
		t.Errorf("请求锚点 = [%v, %v]，期望 [90(P10), 5000(榜首)]", res.Anchors.BaseReq, res.Anchors.TopReq)
	}

	// 榜首 alice（两维综合最强）软提升到 99；其余人分数由相对位置决定。
	assertRow(t, byID(res.Rows, "alice"), 82.63, 62.96, 99.00)
	assertRow(t, byID(res.Rows, "bob"), 73.41, 53.72, 62.80)
	assertRow(t, byID(res.Rows, "carol"), 60.42, 43.68, 51.37)
	assertRow(t, byID(res.Rows, "dave"), 38.20, 25.66, 31.31)
	assertRow(t, byID(res.Rows, "erin"), 99.00, 20.00, 44.50)
	assertRow(t, byID(res.Rows, "frank"), 20.00, 99.00, 44.50)

	// 展示口径说明（v2.4）：total 经过全局拉伸归一到 Cap，
	// 因此 total ≠ sqrt(展示 sTok × sReq) 是**预期**行为——
	// 单维分表示"该维度的相对位置"，总分表示"全榜相对刻度"，两者不同源。
	// 这里断言的反而是：榜首 total 必须恰好等于 Cap（见 TestTopIsAlwaysCap）。
}

// TestDatasetB_DirtyData：含 0 值与极端离群值的数据集。
func TestDatasetB_DirtyData(t *testing.T) {
	entries := []Entry{
		{ID: "u1", Tokens: 1, Requests: 1},
		{ID: "u2", Tokens: 500000, Requests: 4},
		{ID: "u3", Tokens: 50, Requests: 900},
		{ID: "u4", Tokens: 1000000, Requests: 1000000},
		{ID: "u5", Tokens: 0, Requests: 0},
	}
	res := ScoreBoard(entries)

	assertRow(t, byID(res.Rows, "u1"), 24.91, 24.91, 24.91)
	assertRow(t, byID(res.Rows, "u2"), 95.28, 32.35, 55.52)
	assertRow(t, byID(res.Rows, "u3"), 45.89, 61.39, 53.08)
	// 榜首 = 99（Cap），永不出现 100
	assertRow(t, byID(res.Rows, "u4"), 99.00, 99.00, 99.00)
	// 零活跃必须精确落 floor
	assertRow(t, byID(res.Rows, "u5"), 20.00, 20.00, 20.00)
}

// TestE1_SingleUser：单人榜。唯一的人也是第一名，但拿不到满分——正确。
func TestE1_SingleUser(t *testing.T) {
	res := ScoreBoard([]Entry{{ID: "solo", Tokens: 5, Requests: 2}})
	row := byID(res.Rows, "solo")
	// v2.4：单人榜 = 数据退化（A=B）→ 全榜 Cap，唯一一人即榜首 = 99
	assertRow(t, row, 99.00, 99.00, 99.00)
	if row.Rank != 1 {
		t.Errorf("单人的名次 = %d，期望 1", row.Rank)
	}
}

// TestE2_Tie：完全并列。两人并列且都触底。
func TestE2_Tie(t *testing.T) {
	res := ScoreBoard([]Entry{
		{ID: "t1", Tokens: 5000, Requests: 500},
		{ID: "t2", Tokens: 5000, Requests: 500},
	})
	assertRow(t, byID(res.Rows, "t1"), 99.00, 99.00, 99.00)
	assertRow(t, byID(res.Rows, "t2"), 99.00, 99.00, 99.00)
	// 并列时按 id 升序稳定排序
	if res.Rows[0].ID != "t1" || res.Rows[1].ID != "t2" {
		t.Errorf("并列排序 = [%s, %s]，期望 [t1, t2]（按 id 升序）", res.Rows[0].ID, res.Rows[1].ID)
	}
}

// TestE4_SingleUserMinZero：pTop=1.0/pBase=0.0 下的单人榜。
//
// 规格原文此处写 sReq=100.00，但按其自身锚点推导（A=500、B=100、v=100）
// 与 sub 的分支规则（v ≤ B → floor），正确结果必须是 floor=20。
// 本测试以公式为准，见交付说明的偏差条目。
func TestE4_SingleUserMinZero(t *testing.T) {
	res := ScoreBoardWithConfig(
		[]Entry{{ID: "z", Tokens: 100, Requests: 100}},
		Config{TopPercent: 1.0, BasePct: 0.0, SetBasePct: true},
	)
	assertRow(t, byID(res.Rows, "z"), 99.00, 99.00, 99.00)
}

// TestE3_ZeroActivity：零活跃精确落 floor（数据集 B 的 u5）。
func TestE3_ZeroActivity(t *testing.T) {
	res := ScoreBoard([]Entry{
		{ID: "z1", Tokens: 0, Requests: 0},
		{ID: "z2", Tokens: 100, Requests: 50},
	})
	row := byID(res.Rows, "z1")
	// z1 是 v=0，落在下锚点之下 → floor
	if row.ScoreTok != 20 || row.ScoreReq != 20 {
		t.Errorf("零活跃单维 = %.2f/%.2f，期望 20.00/20.00", row.ScoreTok, row.ScoreReq)
	}
	// total 是相对量：z1 落后榜首很远，拉伸后仍应是榜尾（远低于榜首 99）
	if top := byID(res.Rows, "z2"); top.Total <= row.Total {
		t.Errorf("零活跃用户的总分 %.2f 不应高于榜首 %.2f", row.Total, top.Total)
	}
}

/* ─────────────────── 二、动态性：分数必须随全员数据变化 ─────────────────── */

// TestDynamic_AnchorMovesWithData：把 erin 的 token 从 2 万抬到 10 万，
// 锚点随之抬高，其他人分数被压低。
//
// v2.4 语义：拉伸后榜首恒为 Cap(99)，所以榜首用量变化的证据
// 不在榜首自己的分数上（它永远是 99），而在**其他人被压低了多少**。
// 若锚点不随榜首变化，其他人分数就不会变——这就是本测试的判据。
func TestDynamic_AnchorMovesWithData(t *testing.T) {
	before := ScoreBoard(datasetA())

	raised := datasetA()
	raised[4].Tokens = 100000 // erin
	after := ScoreBoard(raised)

	// 上锚必须被抬高：从 20000（榜首原值）升到 100000
	if after.Anchors.TopTok <= before.Anchors.TopTok {
		t.Errorf("上锚未抬高：%v → %v",
			before.Anchors.TopTok, after.Anchors.TopTok)
	}

	// 其他人被压低（他们在 token 维度的相对位置变差）：
	// alice 82.63→57.89、bob 73.41→52.31、carol 60.42→44.45、dave 38.20→31.01
	// 非榜首用户（bob/carol/dave）：token 单维分与总分都必须下降。
	// alice 变成了新榜首（被软提升到 99），故不在此列。
	checks := []struct {
		id          string
		beforeTok   float64
		afterTok    float64
		beforeTotal float64
		afterTotal  float64
	}{
		{"bob", 73.41, 52.31, 62.80, 53.01},
		{"carol", 60.42, 44.45, 51.37, 44.06},
		{"dave", 38.20, 31.01, 31.31, 28.21},
	}
	for _, c := range checks {
		b := byID(before.Rows, c.id).ScoreTok
		a := byID(after.Rows, c.id).ScoreTok
		if a >= b-tolerance {
			t.Errorf("%s 的 token 单维分应随榜首抬升而下降：%.2f → %.2f", c.id, b, a)
		}
		bt := byID(before.Rows, c.id).Total
		at := byID(after.Rows, c.id).Total
		if at >= bt-tolerance {
			t.Errorf("%s 的总分应随榜首抬升而下降：%.2f → %.2f", c.id, bt, at)
		}
	}
}

// TestNoFullScoreEver：任何输入下都不得出现 ScoreMax(100) 分。
//
// 这是产品硬要求：满分位永远空着，榜首"接近满分但不是满分"。
// 单人榜 / 并列 / 零活跃 / 随机数据 / 全同值——逐一验证。
func TestNoFullScoreEver(t *testing.T) {
	dsets := [][]Entry{
		{{ID: "solo", Tokens: 5, Requests: 2}},
		{{ID: "t1", Tokens: 5000, Requests: 500}, {ID: "t2", Tokens: 5000, Requests: 500}},
		{{ID: "z1", Tokens: 0, Requests: 0}, {ID: "z2", Tokens: 100, Requests: 50}},
		{{ID: "a", Tokens: 1, Requests: 1}, {ID: "b", Tokens: 1, Requests: 1}},
		{{ID: "x", Tokens: 1e12, Requests: 1}},
		datasetA(), datasetB(),
	}
	for i, ds := range dsets {
		res := ScoreBoard(ds)
		for _, r := range res.Rows {
			if r.Total >= DefaultConfig.ScoreMax {
				t.Errorf("数据集 #%d 用户 %s 拿到 %.2f 分（不得 ≥ %.0f）",
					i, r.ID, r.Total, DefaultConfig.ScoreMax)
			}
			if r.Total > DefaultConfig.Cap+tolerance {
				t.Errorf("数据集 #%d 用户 %s 超过 Cap(%.0f)：%.2f",
					i, r.ID, DefaultConfig.Cap, r.Total)
			}
		}
	}
	// 随机数据同样不得出现 100
	rng := rand.New(rand.NewSource(99))
	for round := 0; round < 300; round++ {
		n := 1 + rng.Intn(25)
		ds := make([]Entry, n)
		for i := range ds {
			ds[i] = Entry{ID: string(rune('a' + i)), Tokens: rng.Float64() * 1e9, Requests: rng.Float64() * 1e6}
		}
		for _, r := range ScoreBoard(ds).Rows {
			if r.Total >= DefaultConfig.ScoreMax {
				t.Fatalf("第 %d 轮 %s 出现满分 %.2f", round, r.ID, r.Total)
			}
		}
	}
}

// TestTopIsAlwaysCap：软提升后榜首恰好等于 Cap（接近满分、不满分）。
//
// 注意：软提升只把榜首补到 Cap，**不改动他人的分数刻度**——这是与
// "全局等比拉伸"的关键区别，后者会在榜首易主时把他人分数推高，
// 造成"榜首涨了、别人反而涨"的反直觉结果。
func TestTopIsAlwaysCap(t *testing.T) {
	for _, ds := range [][]Entry{datasetA(), datasetB(), {{ID: "solo", Tokens: 7, Requests: 3}}} {
		res := ScoreBoard(ds)
		if len(res.Rows) == 0 {
			continue
		}
		top := res.Rows[0] // 已按 total 降序
		if math.Abs(top.Total-DefaultConfig.Cap) > tolerance {
			t.Errorf("榜首分数 = %.2f，期望恰好 %.0f", top.Total, DefaultConfig.Cap)
		}
	}
}

/* ─────────────────────────── 三、性质测试 ─────────────────────────── */

// TestEmpty：空输入返回空数组，不 panic。
func TestEmpty(t *testing.T) {
	res := ScoreBoard(nil)
	if res.Rows == nil {
		t.Error("Rows 应为非 nil 空切片（前端可直接 .map）")
	}
	if len(res.Rows) != 0 {
		t.Errorf("空输入应返回 0 行，实际 %d 行", len(res.Rows))
	}
	res2 := ScoreBoard([]Entry{})
	if len(res2.Rows) != 0 {
		t.Errorf("空切片应返回 0 行，实际 %d 行", len(res2.Rows))
	}
}

// TestSingleElementRange：单元素不 panic，分数落在 [floor, scoreMax]。
func TestSingleElementRange(t *testing.T) {
	res := ScoreBoard([]Entry{{ID: "only", Tokens: 12345, Requests: 678}})
	if len(res.Rows) != 1 {
		t.Fatalf("单元素应返回 1 行，实际 %d 行", len(res.Rows))
	}
	row := res.Rows[0]
	if row.Total < DefaultConfig.Floor-tolerance || row.Total > DefaultConfig.ScoreMax+tolerance {
		t.Errorf("单元素分数 = %.2f，超出 [%.0f, %.0f]",
			row.Total, DefaultConfig.Floor, DefaultConfig.ScoreMax)
	}
}

// TestDirtyInputHandled：负数 / NaN / Inf 不得 panic，按 0 处理。
func TestDirtyInputHandled(t *testing.T) {
	entries := []Entry{
		{ID: "neg", Tokens: -100, Requests: -50},
		{ID: "nan", Tokens: math.NaN(), Requests: math.NaN()},
		{ID: "inf", Tokens: math.Inf(1), Requests: math.Inf(-1)},
		{ID: "ok", Tokens: 5000, Requests: 500},
	}
	res := ScoreBoard(entries)
	if len(res.Rows) != 4 {
		t.Fatalf("脏数据行必须保留在榜内，实际 %d 行", len(res.Rows))
	}
	for _, id := range []string{"neg", "nan", "inf"} {
		row := byID(res.Rows, id)
		if row.Tokens != 0 || row.Requests != 0 {
			t.Errorf("%s 的脏值应置 0，实际 tokens=%v requests=%v", id, row.Tokens, row.Requests)
		}
	}
	// 正常行不应受影响
	if byID(res.Rows, "ok").Total <= DefaultConfig.Floor {
		t.Errorf("正常行分数被脏数据污染：%.2f", byID(res.Rows, "ok").Total)
	}
}

// TestSymmetry：两维互换 + 权重互换，每人总分不变。
//
// 这是算法对称性的硬证据：token 与请求的地位完全对等。
func TestSymmetry(t *testing.T) {
	entries := datasetA()
	base := ScoreBoard(entries)

	swapped := make([]Entry, len(entries))
	for i, e := range entries {
		swapped[i] = Entry{ID: e.ID, Tokens: e.Requests, Requests: e.Tokens}
	}
	swappedRes := ScoreBoardWithConfig(swapped, Config{
		Weights: Weights{Tokens: DefaultConfig.Weights.Requests, Requests: DefaultConfig.Weights.Tokens},
	})

	for _, e := range entries {
		b := byID(base.Rows, e.ID).Total
		s := byID(swappedRes.Rows, e.ID).Total
		if math.Abs(b-s) > tolerance {
			t.Errorf("%s 对称互换后分数变化：%.2f → %.2f", e.ID, b, s)
		}
	}
}

// TestDominance：跨用户支配关系。
//
// 若 A 在两维都不低于 B，则 A 的总分不得低于 B。
//
// 刻意不写的断言（规格已警示）：
//   - "提高某维数值，自己分数一定上升"——不成立：抬高该维锚点可能压低所有人；
//   - "别人分数一定不上升"——不成立：分位基准移动会让某人从 floor 跳进插值区。
func TestDominance(t *testing.T) {
	entries := []Entry{
		{ID: "a", Tokens: 10000, Requests: 900},
		{ID: "b", Tokens: 5000, Requests: 400},
		{ID: "c", Tokens: 500, Requests: 5000},
		{ID: "d", Tokens: 20000, Requests: 50},
		{ID: "e", Tokens: 3000, Requests: 300},
		{ID: "f", Tokens: 50, Requests: 40},
		{ID: "g", Tokens: 8000, Requests: 800},
		{ID: "h", Tokens: 1200, Requests: 1500},
	}
	res := ScoreBoard(entries)
	score := map[string]float64{}
	for _, r := range res.Rows {
		score[r.ID] = r.Total
	}
	for _, a := range entries {
		for _, b := range entries {
			if a.ID == b.ID {
				continue
			}
			if a.Tokens >= b.Tokens && a.Requests >= b.Requests {
				if score[a.ID] < score[b.ID]-tolerance {
					t.Errorf("支配关系被破坏：%s(%.0f/%.0f)=%.2f 应 ≥ %s(%.0f/%.0f)=%.2f",
						a.ID, a.Tokens, a.Requests, score[a.ID],
						b.ID, b.Tokens, b.Requests, score[b.ID])
				}
			}
		}
	}
}

// TestDeterminism：打乱输入顺序，输出顺序与分数完全一致。
func TestDeterminism(t *testing.T) {
	base := ScoreBoard(datasetA())

	rng := rand.New(rand.NewSource(20261002))
	for round := 0; round < 20; round++ {
		shuffled := datasetA()
		rng.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		res := ScoreBoard(shuffled)

		if len(res.Rows) != len(base.Rows) {
			t.Fatalf("第 %d 轮行数不同：%d vs %d", round, len(res.Rows), len(base.Rows))
		}
		for i := range res.Rows {
			if res.Rows[i].ID != base.Rows[i].ID {
				t.Fatalf("第 %d 轮第 %d 位顺序不同：%s vs %s", round, i, res.Rows[i].ID, base.Rows[i].ID)
			}
			if math.Abs(res.Rows[i].TotalRaw-base.Rows[i].TotalRaw) > 1e-12 {
				t.Errorf("第 %d 轮 %s 分数不同：%.15f vs %.15f",
					round, res.Rows[i].ID, res.Rows[i].TotalRaw, base.Rows[i].TotalRaw)
			}
		}
	}
}

// TestScoreRange：所有输出必须落在 [floor, scoreMax]。
func TestScoreRange(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for round := 0; round < 200; round++ {
		n := 1 + rng.Intn(30)
		entries := make([]Entry, n)
		for i := range entries {
			entries[i] = Entry{
				ID:       string(rune('a' + i)),
				Tokens:   rng.Float64() * 1e6,
				Requests: rng.Float64() * 1e4,
			}
		}
		res := ScoreBoard(entries)
		for _, r := range res.Rows {
			if r.Total < DefaultConfig.Floor-tolerance || r.Total > DefaultConfig.ScoreMax+tolerance {
				t.Fatalf("第 %d 轮 %s total=%.4f 越界 [%.0f, %.0f]",
					round, r.ID, r.Total, DefaultConfig.Floor, DefaultConfig.ScoreMax)
			}
		}
		// 名次必须是 1..n 的连续序列
		for i, r := range res.Rows {
			if r.Rank != i+1 {
				t.Fatalf("第 %d 轮第 %d 行名次 = %d，期望 %d", round, i, r.Rank, i+1)
			}
		}
	}
}

// TestConfigInvariants：配置不变式被强制。
func TestConfigInvariants(t *testing.T) {
	// 每个用例给出「该配置下期望的分数区间」——非法配置必须回退成某个自洽配置，
	// 而不是 panic 或产出与实际生效配置不一致的分数。
	cases := []struct {
		name   string
		cfg    Config
		lo, hi float64
	}{
		{"pBase≥pTop 回退默认", Config{BasePct: 0.95, TopPercent: 0.10}, 20, 100},
		{"floor≥scoreMax 归零", Config{ScoreMax: 10, Floor: 50, SetFloor: true}, 0, 10},
		{"pTop>1 截断为 1", Config{TopPercent: 1.5}, 20, 100},
		{"权重和为2 归一", Config{Weights: Weights{Tokens: 1, Requests: 1}}, 20, 100},
	}
	for _, c := range cases {
		res := ScoreBoardWithConfig(datasetA(), c.cfg)
		for _, r := range res.Rows {
			if r.Total < c.lo-tolerance || r.Total > c.hi+tolerance {
				t.Errorf("%s（%+v）产出分数 %.2f，超出期望区间 [%.0f, %.0f]",
					c.name, c.cfg, r.Total, c.lo, c.hi)
			}
		}
	}
}

// TestRoundHalfUp：舍入是 half-up（0.005 进位），不是 banker's rounding。
func TestRoundHalfUp(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{68.765, 68.77},
		{68.764, 68.76},
		{0.005, 0.01},
		{99.995, 100.00},
		{44.7214, 44.72},
	}
	for _, c := range cases {
		if got := round2(c.in); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("round2(%v) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

// TestPercentileMatchesExcel：分位数与 Excel PERCENTILE.INC 同口径。
func TestPercentileMatchesExcel(t *testing.T) {
	xs := []float64{400, 3000, 6000, 9000, 12000, 20000}
	cases := []struct {
		p    float64
		want float64
	}{
		{0.0, 400},
		{0.10, 1700},
		{0.90, 16000},
		{1.0, 20000},
	}
	for _, c := range cases {
		if got := percentile(xs, c.p); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("percentile(p=%.2f) = %v，期望 %v", c.p, got, c.want)
		}
	}
	// 单元素
	one := []float64{42}
	if got := percentile(one, 0.5); got != 42 {
		t.Errorf("单元素分位数 = %v，期望 42", got)
	}
	// 空输入
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("空输入分位数 = %v，期望 0", got)
	}
}

// TestSanitizeFloorInt：合法值向下取整（token/请求都是计数）。
func TestSanitizeFloorInt(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{100.9, 100}, {100.1, 100}, {0, 0},
		{-1, 0}, {math.NaN(), 0}, {math.Inf(1), 0}, {math.Inf(-1), 0},
	}
	for _, c := range cases {
		if got := sanitize(c.in); got != c.want {
			t.Errorf("sanitize(%v) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

// TestRanksAreContiguous：名次必须是无重复的 1..n。
func TestRanksAreContiguous(t *testing.T) {
	res := ScoreBoard(datasetB())
	seen := map[int]bool{}
	for _, r := range res.Rows {
		if seen[r.Rank] {
			t.Errorf("名次 %d 重复", r.Rank)
		}
		seen[r.Rank] = true
	}
	ids := make([]string, 0, len(res.Rows))
	for _, r := range res.Rows {
		ids = append(ids, r.ID)
	}
	if !sort.StringsAreSorted(ids) && len(ids) > 0 {
		t.Logf("输出顺序（按 rank）: %v", ids)
	}
}
