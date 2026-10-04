// 本文件实现对弈的 HTTP 接口（创建对局、推进一手、读取与删除）。
//
// 意图（Why）：
//
//	把"对局"这个领域对象暴露成一组可被前端驱动的操作。刻意做成
//	【一步一个接口】而不是"一次跑完整局"：
//	  · 一次 AI 走子要数秒到数十秒，整局跑完会远超任何合理的请求超时；
//	  · 逐步推进让前端能实时看到每一手，这正是演示的看点；
//	  · 中途关页面就自然暂停，不会在服务端留下一个跑很久的后台任务。
//
// 一条必须守住的纪律（额度）：
//
//	对弈的模型调用走的是转发链路但【不经过鉴权中间件】，
//	因此没有"事前额度墙"（见 game_caller.go 的说明）。
//	如果这里不补，零额度用户就能靠开棋局无限调用模型——
//	一个绕过计费的漏洞。因此每次走子前都调用 ensureGameQuota，
//	判定口径与鉴权中间件【完全一致】（总额度 − 已用 − 在途预留），
//	否则两处口径不同会出现"正常调用被拦、下棋却能过"的反常现象。
//
// 流转（Flow）：
//
//	POST /api/user/games                 创建对局（不调用模型）
//	POST /api/user/games/:id/step        推进一手（人类落子 或 AI 走子）
//	GET  /api/user/games/:id             读取对局 + 棋谱
//	GET  /api/user/games/:id/board.png   取当前局面的图片（即模型看到的那张）
//	GET  /api/user/games                 我的对局列表
//	DELETE /api/user/games/:id           删除对局
//
// 扩展（Extend）：
//
//	想加"后台自动跑完"：在 step 之外加一个受控的循环接口，
//	但必须先解决超时与并发（同一对局被两个请求同时推进），
//	当前 step 的乐观并发（靠 (match_id, seq) 唯一索引）正是为此预留的。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xiaosu4610/aqua-api/internal/game"
	"github.com/xiaosu4610/aqua-api/internal/model"
	"github.com/xiaosu4610/aqua-api/internal/oai"
	"github.com/xiaosu4610/aqua-api/internal/reqctx"
	"github.com/xiaosu4610/aqua-api/internal/store"
)

// 对局相关的限制。
const (
	// gameMaxMoves 是单局手数上限（防止模型互下不休止地消耗额度）。
	//
	// 取 200：围棋一盘正常在 150~300 手，但那是人类对局；
	// 模型对局常常在中盘就因无法给出合法着法而中止，
	// 200 手足以覆盖绝大多数情况，同时给额度消耗一个硬上限。
	gameMaxMoves = 200
	// gameListLimit 是"我的对局"返回条数上限。
	gameListLimit = 50
)

// createGameRequest 是创建对局的请求体。
type createGameRequest struct {
	Kind string `json:"kind"` // gomoku / go / xiangqi
	Mode string `json:"mode"` // ai_vs_ai / human_vs_ai
	// BlackModel / WhiteModel 是各席位模型名；人类席位留空。
	BlackModel string `json:"black_model"`
	WhiteModel string `json:"white_model"`
	// HumanColor 是人机模式下人类执的一方：1=黑 2=白。
	HumanColor uint8 `json:"human_color"`
}

// stepGameRequest 是推进一手的请求体。
type stepGameRequest struct {
	// Move 是人类落子（人机模式且轮到人类时必填），如 "H8" 或 "H1H4"。
	Move string `json:"move"`
}

// gameMatchDTO 是对局的对前端形态。
type gameMatchDTO struct {
	ID         uint64 `json:"id"`
	Kind       string `json:"kind"`
	KindLabel  string `json:"kind_label"`
	Mode       string `json:"mode"`
	ModeLabel  string `json:"mode_label"`
	BlackModel string `json:"black_model"`
	WhiteModel string `json:"white_model"`
	// HumanColor 0=无人类 1=黑 2=白
	HumanColor uint8  `json:"human_color"`
	Status     string `json:"status"`
	StatusText string `json:"status_text"`
	Winner     string `json:"winner"`
	MoveCount  int    `json:"move_count"`
	// ToMove 是下一步该谁走（0 表示对局已结束）。
	ToMove uint8 `json:"to_move"`
	// AwaitingHuman 表示当前是否在等人落子（前端据此启用输入）。
	AwaitingHuman bool   `json:"awaiting_human"`
	ErrorText     string `json:"error_text,omitempty"`
	// Position 是当前局面（前端据此绘制棋盘）。
	Position  *game.Position `json:"position"`
	CreatedAt int64          `json:"created_at"`
	UpdatedAt int64          `json:"updated_at"`
}

// gameMoveDTO 是一手棋的前端形态。
type gameMoveDTO struct {
	Seq       int    `json:"seq"`
	Color     uint8  `json:"color"`
	ColorText string `json:"color_text"`
	Notation  string `json:"notation"`
	// Attempts > 1 表示这一手模型试了多次才给出合法着法，
	// 它是"模型看图能力"最直接的量化指标，值得展示给用户。
	Attempts int    `json:"attempts"`
	Raw      string `json:"raw,omitempty"`
	Error    string `json:"error_text,omitempty"`
}

// gameDetailDTO 是对局详情（含棋谱）。
type gameDetailDTO struct {
	Match gameMatchDTO  `json:"match"`
	Moves []gameMoveDTO `json:"moves"`
}

// handleCreateGame 创建一局对局。
//
// POST /api/user/games
func (s *Server) handleCreateGame(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}
	var req createGameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		s.gameBadRequest(c, "请求格式不正确")
		return
	}

	kind, err := game.ParseKind(req.Kind)
	if err != nil {
		s.gameBadRequest(c, "不支持的棋种："+req.Kind)
		return
	}
	mode := strings.TrimSpace(req.Mode)
	if mode != model.GameModeAIVsAI && mode != model.GameModeHumanVsAI {
		s.gameBadRequest(c, "对局模式只能是 ai_vs_ai 或 human_vs_ai")
		return
	}

	blackModel := strings.TrimSpace(req.BlackModel)
	whiteModel := strings.TrimSpace(req.WhiteModel)

	switch mode {
	case model.GameModeAIVsAI:
		if blackModel == "" || whiteModel == "" {
			s.gameBadRequest(c, "AI 对 AI 必须为双方各选择一个模型")
			return
		}
		if req.HumanColor != 0 {
			s.gameBadRequest(c, "AI 对 AI 模式下不应指定人类执子方")
			return
		}
	case model.GameModeHumanVsAI:
		if req.HumanColor != 1 && req.HumanColor != 2 {
			s.gameBadRequest(c, "人机对弈必须指定人类执黑(1)还是执白(2)")
			return
		}
		// 人类那一席必须没有模型名，另一席必须有——
		// 否则会创建出"人类执白但白方也配了模型"这种自相矛盾的对局。
		if req.HumanColor == 1 {
			if blackModel != "" {
				s.gameBadRequest(c, "人类执黑时，黑方不应再指定模型")
				return
			}
			if whiteModel == "" {
				s.gameBadRequest(c, "请为白方（AI）选择一个模型")
				return
			}
		} else {
			if whiteModel != "" {
				s.gameBadRequest(c, "人类执白时，白方不应再指定模型")
				return
			}
			if blackModel == "" {
				s.gameBadRequest(c, "请为黑方（AI）选择一个模型")
				return
			}
		}
	}

	// 创建前先验额度：让用户在"开一局棋"这一步就知道额度不够，
	// 而不是下到一半才失败。同时这也挡住了"零额度开局刷模型"。
	if err := s.ensureGameQuota(c.Request.Context(), user.ID); err != nil {
		s.respondQuotaBlocked(c, err)
		return
	}

	pos, err := game.NewPosition(kind)
	if err != nil {
		s.gameBadRequest(c, err.Error())
		return
	}
	stateJSON, err := json.Marshal(pos)
	if err != nil {
		s.respondInternalError(c, "序列化局面失败")
		return
	}

	match := &model.GameMatch{
		UserID:     user.ID,
		Kind:       string(kind),
		Mode:       mode,
		BlackModel: blackModel,
		WhiteModel: whiteModel,
		HumanColor: req.HumanColor,
		Status:     model.GameStatusPlaying,
		StateJSON:  string(stateJSON),
	}
	if err := s.deps.GameMatches.Create(c.Request.Context(), match); err != nil {
		s.respondInternalError(c, "创建对局失败")
		return
	}

	dto, err := s.gameMatchDTO(match)
	if err != nil {
		s.respondInternalError(c, "读取对局失败")
		return
	}
	c.JSON(http.StatusCreated, dto)
}

// handleStepGame 推进一手。
//
// POST /api/user/games/:id/step
//
// 语义：无论是人类还是 AI 的回合，本接口都只推进【一手】。
// 前端据此循环调用即可看到逐步对局。
func (s *Server) handleStepGame(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}
	matchID, err := parseUintParam(c, "id")
	if err != nil {
		s.gameBadRequest(c, "对局 ID 无效")
		return
	}
	var req stepGameRequest
	// 人类回合需要 body；AI 回合可以不带 body（前端只发一个空对象或什么都不发）。
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			s.gameBadRequest(c, "请求格式不正确")
			return
		}
	}

	ctx := c.Request.Context()
	match, err := s.deps.GameMatches.GetByID(ctx, user.ID, matchID)
	if err != nil {
		if errors.Is(err, model.ErrGameMatchNotFound) {
			s.gameNotFound(c, "对局不存在")
			return
		}
		s.respondInternalError(c, "读取对局失败")
		return
	}
	if match.IsOver() {
		s.gameBadRequest(c, "对局已结束")
		return
	}
	if match.MoveCount >= gameMaxMoves {
		// 触到上限：如实标记为中止并说明原因，而不是静默不再推进
		// （静默会让前端一直轮询而永远看不到结束）。
		s.abortMatch(ctx, match, fmt.Sprintf("已达单局手数上限（%d 手）", gameMaxMoves), nil)
		s.gameBadRequest(c, fmt.Sprintf("已达单局手数上限（%d 手）", gameMaxMoves))
		return
	}

	pos, err := decodeGamePosition(match)
	if err != nil {
		s.respondInternalError(c, "局面数据损坏")
		return
	}

	// 额度闸门：见文件头说明——这是补回"事前额度墙"的关键一步。
	if err := s.ensureGameQuota(ctx, user.ID); err != nil {
		s.respondQuotaBlocked(c, err)
		return
	}

	// 判定本手由谁走，并区分"人类"与"AI"两条路径。
	if match.IsHumanTurn(uint8(pos.ToMove)) {
		if strings.TrimSpace(req.Move) == "" {
			s.gameBadRequest(c, "轮到你落子，请提供 move 字段")
			return
		}
		s.stepHuman(c, match, pos, req.Move)
		return
	}

	// AI 走子：这是唯一会调用模型的路径。
	s.stepAI(c, match, pos)
}

// stepHuman 处理人类落子。
//
// 人类的输入同样走【与模型完全相同】的解析与校验：
// 这样"人写 H8 还是 h8、写不写 MOVE: 前缀"都能识别，
// 而合法性判据也只有一份（规则引擎），不会出现"人机两套规则"。
func (s *Server) stepHuman(c *gin.Context, match *model.GameMatch, pos *game.Position, raw string) {
	rules, err := game.Lookup(pos.Kind)
	if err != nil {
		s.respondInternalError(c, "棋种规则不可用")
		return
	}
	res, parseErr := game.ParseMove(pos.Kind, raw)
	if parseErr != nil {
		s.gameBadRequest(c, "无法识别你输入的着法，请使用例如 H8 或 H1H4 的格式")
		return
	}

	mv, err := buildHumanMove(pos, res)
	if err != nil {
		s.gameBadRequest(c, err.Error())
		return
	}
	if err := rules.Validate(pos, mv); err != nil {
		// 人类与模型收到的是同一套规则错误文案，因此提示同样具体
		// （"该位置已有棋子"而不是"非法着法"）。
		s.gameBadRequest(c, "该着法不合法："+err.Error())
		return
	}

	next := rules.Apply(pos, mv)
	status := rules.Result(next)
	s.commitMove(c, match, next, status, &model.GameMoveRecord{
		Seq:       mv.Seq,
		Color:     uint8(mv.Color),
		Notation:  mv.Notation(),
		RawOutput: truncateText(raw, 300),
		Attempts:  1,
	})
}

// buildHumanMove 把解析结果转成一手着法（含人类不可能触发的校验）。
func buildHumanMove(pos *game.Position, res game.ParseResult) (game.Move, error) {
	if res.IsResign {
		return game.Move{Color: pos.ToMove, Seq: pos.LastMoveSeq() + 1, Resign: true}, nil
	}
	if res.IsPass {
		// 与 AI 路径保持同一判据：只有围棋允许停一手。
		if pos.Kind != game.KindGo {
			return game.Move{}, fmt.Errorf("该棋种不允许停一手")
		}
		return game.Move{Color: pos.ToMove, Seq: pos.LastMoveSeq() + 1, Pass: true}, nil
	}
	mv := res.Move
	mv.Color = pos.ToMove
	mv.Seq = pos.LastMoveSeq() + 1
	return mv, nil
}

// stepAI 处理 AI 走子（本文件唯一会调用模型的路径）。
func (s *Server) stepAI(c *gin.Context, match *model.GameMatch, pos *game.Position) {
	modelName := match.ModelForColor(uint8(pos.ToMove))
	if modelName == "" {
		s.respondInternalError(c, "该席位没有可用模型")
		return
	}

	ctx := c.Request.Context()
	// 身份写进 context：模型调用会经转发链路计费与记日志，
	// 缺了它这些调用会记成"匿名"，用户看不到自己的用量。
	identity := reqctx.Identity{
		UserID:  match.UserID,
		TokenID: 0,
	}
	if cur, ok := reqctx.IdentityFrom(ctx); ok {
		identity.TokenID = cur.TokenID
	}
	callCtx := reqctx.WithIdentity(ctx, identity)

	recent := s.recentMoveNotations(ctx, match.ID, 6)
	outcome, next, status, err := s.gameArena().PlayTurn(callCtx, pos, modelName, recent)
	if err != nil {
		s.respondInternalError(c, "推进对局失败："+err.Error())
		return
	}

	// 调用层面失败（网络/额度/无可用渠道）：中止对局并如实说明，
	// 绝不写成"模型认输"——那是与事实相反的结论。
	if outcome.CallFailed {
		s.abortMatch(ctx, match, "模型调用失败："+outcome.Error, nil)
		c.JSON(http.StatusOK, gin.H{
			"match":       s.mustMatchDTO(match),
			"moves":       []gameMoveDTO{},
			"aborted":     true,
			"error":       outcome.Error,
			"call_failed": true,
		})
		return
	}

	// 重试用尽：把这一手与失败原因都落库，让用户能看到"模型卡在哪"。
	if outcome.AttemptsExhausted() {
		reason := outcome.Error
		if len(outcome.AttemptErrors) > 0 {
			reason = outcome.Error + "；" + strings.Join(outcome.AttemptErrors, " | ")
		}
		rec := &model.GameMoveRecord{
			Seq:       pos.LastMoveSeq() + 1,
			Color:     uint8(pos.ToMove),
			Notation:  "（未能给出合法着法）",
			RawOutput: truncateText(outcome.Raw, 300),
			Attempts:  outcome.Attempts,
			ErrorText: truncateText(reason, 500),
		}
		s.abortMatch(ctx, match, reason, rec)
		c.JSON(http.StatusOK, gin.H{
			"match":   s.mustMatchDTO(match),
			"moves":   []gameMoveDTO{},
			"aborted": true,
			"error":   reason,
		})
		return
	}

	mv := *outcome.Move
	s.commitMove(c, match, next, status, &model.GameMoveRecord{
		Seq:       mv.Seq,
		Color:     uint8(mv.Color),
		Notation:  mv.Notation(),
		RawOutput: truncateText(outcome.Raw, 300),
		Attempts:  outcome.Attempts,
	})
}

// commitMove 落库一手并返回对局的新状态。
//
// 所有成功路径（人类/AI）都汇聚到这里，保证"棋谱 + 快照 + 状态"
// 三者的更新只有一份实现，不会出现两条路径记法不一致。
func (s *Server) commitMove(c *gin.Context, match *model.GameMatch, next *game.Position, status game.Status, rec *model.GameMoveRecord) {
	ctx := c.Request.Context()
	stateJSON, err := json.Marshal(next)
	if err != nil {
		s.respondInternalError(c, "序列化局面失败")
		return
	}
	match.StateJSON = string(stateJSON)
	match.MoveCount = rec.Seq
	match.Status = string(status)
	match.Winner = winnerOf(status)
	rec.MatchID = match.ID

	if err := s.deps.GameMatches.AdvanceWithMove(ctx, match, rec); err != nil {
		if errors.Is(err, store.ErrGameMoveConflict) {
			// 并发推进：如实告诉前端刷新，而不是 500（这不是服务端故障）。
			s.gameBadRequest(c, "该对局已被推进，请刷新后重试")
			return
		}
		s.respondInternalError(c, "保存对局失败")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"match":  s.mustMatchDTO(match),
		"moves":  []gameMoveDTO{dtoOfMove(rec)},
		"status": match.Status,
		"over":   match.IsOver(),
	})
}

// abortMatch 中止对局并落一条说明性记录。
//
// rec 可为 nil（如触到手数上限时没有具体一手可记）。
func (s *Server) abortMatch(ctx context.Context, match *model.GameMatch, reason string, rec *model.GameMoveRecord) {
	match.Status = model.GameStatusAborted
	match.Winner = ""
	match.ErrorText = truncateText(reason, 500)

	if rec == nil {
		if err := s.deps.GameMatches.Update(ctx, match); err != nil {
			slog.Warn("中止对局失败", "error", err, "match_id", match.ID)
		}
		return
	}
	rec.MatchID = match.ID
	match.MoveCount = rec.Seq
	if err := s.deps.GameMatches.AdvanceWithMove(ctx, match, rec); err != nil {
		// 落库失败不能让状态停留在 playing：至少要更新状态。
		if uerr := s.deps.GameMatches.Update(ctx, match); uerr != nil {
			slog.Warn("中止对局失败（且棋谱落库也失败）",
				"error", uerr, "match_id", match.ID)
		}
	}
}

// handleGetGame 读取对局详情（含棋谱）。
//
// GET /api/user/games/:id
func (s *Server) handleGetGame(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}
	matchID, err := parseUintParam(c, "id")
	if err != nil {
		s.gameBadRequest(c, "对局 ID 无效")
		return
	}
	ctx := c.Request.Context()
	match, err := s.deps.GameMatches.GetByID(ctx, user.ID, matchID)
	if err != nil {
		if errors.Is(err, model.ErrGameMatchNotFound) {
			s.gameNotFound(c, "对局不存在")
			return
		}
		s.respondInternalError(c, "读取对局失败")
		return
	}
	moves, err := s.deps.GameMatches.ListMoves(ctx, match.ID)
	if err != nil {
		s.respondInternalError(c, "读取棋谱失败")
		return
	}

	dto, err := s.gameMatchDTO(match)
	if err != nil {
		s.respondInternalError(c, "局面数据损坏")
		return
	}
	list := make([]gameMoveDTO, 0, len(moves))
	for _, m := range moves {
		list = append(list, dtoOfMove(m))
	}
	c.JSON(http.StatusOK, gameDetailDTO{Match: dto, Moves: list})
}

// handleListGames 列出我的对局。
//
// GET /api/user/games
func (s *Server) handleListGames(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	matches, err := s.deps.GameMatches.ListByUser(ctx, user.ID, gameListLimit)
	if err != nil {
		s.respondInternalError(c, "读取对局列表失败")
		return
	}
	out := make([]gameMatchDTO, 0, len(matches))
	for _, m := range matches {
		dto, err := s.gameMatchDTO(m)
		if err != nil {
			// 单条局面损坏不应让整个列表失败：跳过它并继续，
			// 否则一个坏对局会让用户再也打不开列表。
			continue
		}
		out = append(out, dto)
	}
	c.JSON(http.StatusOK, gin.H{"matches": out})
}

// handleDeleteGame 删除对局。
//
// DELETE /api/user/games/:id
func (s *Server) handleDeleteGame(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}
	matchID, err := parseUintParam(c, "id")
	if err != nil {
		s.gameBadRequest(c, "对局 ID 无效")
		return
	}
	if err := s.deps.GameMatches.Delete(c.Request.Context(), user.ID, matchID); err != nil {
		s.respondInternalError(c, "删除对局失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": 1})
}

// handleGameBoard 返回当前局面的 PNG 图片。
//
// GET /api/user/games/:id/board.png
//
// 用途：让前端能展示"模型实际看到的那张图"。这比前端自绘更有说服力——
// 用户能亲眼确认"模型看到的就是这个局面"，而不是怀疑我们喂错了数据。
func (s *Server) handleGameBoard(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}
	matchID, err := parseUintParam(c, "id")
	if err != nil {
		s.gameBadRequest(c, "对局 ID 无效")
		return
	}
	match, err := s.deps.GameMatches.GetByID(c.Request.Context(), user.ID, matchID)
	if err != nil {
		if errors.Is(err, model.ErrGameMatchNotFound) {
			s.gameNotFound(c, "对局不存在")
			return
		}
		s.respondInternalError(c, "读取对局失败")
		return
	}
	pos, err := decodeGamePosition(match)
	if err != nil {
		s.respondInternalError(c, "局面数据损坏")
		return
	}
	// 不缓存：对局每推进一手图就变了，缓存会让前端看到上一手的局面。
	c.Header("Cache-Control", "no-store")
	png, err := game.RenderBoardPNG(pos)
	if err != nil {
		s.respondInternalError(c, "渲染棋盘失败")
		return
	}
	c.Data(http.StatusOK, "image/png", png)
}

// ── 辅助函数 ────────────────────────────────────────────────

// gameArena 返回对局驱动器（每次新建，开销可忽略：它只持有调用器与重试次数）。
//
// 调用器优先用注入的实现（测试或特殊部署），否则退化为基于转发引擎的默认实现。
func (s *Server) gameArena() *game.Arena {
	caller := s.deps.GameCaller
	if caller == nil {
		caller = NewRelayModelCaller(s.deps.Relay)
	}
	return game.NewArena(caller, s.deps.GameArenaAttempts)
}

// decodeGamePosition 从对局快照恢复局面。
func decodeGamePosition(match *model.GameMatch) (*game.Position, error) {
	if strings.TrimSpace(match.StateJSON) == "" {
		// 快照缺失时退回"按棋种新建一局"，而不是报错：
		// 这样即使历史数据不完整，用户至少还能看到棋盘与棋谱。
		kind, err := game.ParseKind(match.Kind)
		if err != nil {
			return nil, err
		}
		return game.NewPosition(kind)
	}
	var pos game.Position
	if err := json.Unmarshal([]byte(match.StateJSON), &pos); err != nil {
		return nil, fmt.Errorf("store: 解析局面快照失败: %w", err)
	}
	return &pos, nil
}

// gameMatchDTO 把实体转成对前端形态。
func (s *Server) gameMatchDTO(match *model.GameMatch) (gameMatchDTO, error) {
	pos, err := decodeGamePosition(match)
	if err != nil {
		return gameMatchDTO{}, err
	}
	toMove := pos.ToMove
	if match.IsOver() {
		toMove = 0
	}
	return gameMatchDTO{
		ID:            match.ID,
		Kind:          match.Kind,
		KindLabel:     kindLabel(match.Kind),
		Mode:          match.Mode,
		ModeLabel:     modeLabel(match.Mode),
		BlackModel:    match.BlackModel,
		WhiteModel:    match.WhiteModel,
		HumanColor:    match.HumanColor,
		Status:        match.Status,
		StatusText:    gameStatusText(match),
		Winner:        match.Winner,
		MoveCount:     match.MoveCount,
		ToMove:        uint8(toMove),
		AwaitingHuman: !match.IsOver() && match.IsHumanTurn(uint8(pos.ToMove)),
		ErrorText:     match.ErrorText,
		Position:      pos,
		CreatedAt:     match.CreatedAt.Unix(),
		UpdatedAt:     match.UpdatedAt.Unix(),
	}, nil
}

// mustMatchDTO 是"读不到局面也不该中断响应"的版本。
//
// 用于中止对局等"已经确定要返回状态"的场景：此时若因局面损坏而整体失败，
// 用户会拿不到任何信息（连"对局已中止"都看不到），比返回一个降级结果更糟。
func (s *Server) mustMatchDTO(match *model.GameMatch) gameMatchDTO {
	dto, err := s.gameMatchDTO(match)
	if err == nil {
		return dto
	}
	return gameMatchDTO{
		ID: match.ID, Kind: match.Kind, KindLabel: kindLabel(match.Kind),
		Mode: match.Mode, ModeLabel: modeLabel(match.Mode),
		BlackModel: match.BlackModel, WhiteModel: match.WhiteModel,
		HumanColor: match.HumanColor, Status: match.Status,
		StatusText: gameStatusText(match), Winner: match.Winner,
		MoveCount: match.MoveCount, ErrorText: match.ErrorText,
		CreatedAt: match.CreatedAt.Unix(), UpdatedAt: match.UpdatedAt.Unix(),
	}
}

// recentMoveNotations 取最近若干手的记法（喂给模型的上下文）。
//
// 只取最近几手：更早的局面已经体现在图片里，重复列出既费 token，
// 又可能让模型"以文字为准而忽略图片"——那恰恰破坏了多模态演示的意义。
func (s *Server) recentMoveNotations(ctx context.Context, matchID uint64, limit int) []string {
	moves, err := s.deps.GameMatches.ListMoves(ctx, matchID)
	if err != nil || len(moves) == 0 {
		return nil
	}
	start := 0
	if len(moves) > limit {
		start = len(moves) - limit
	}
	out := make([]string, 0, len(moves)-start)
	for _, m := range moves[start:] {
		out = append(out, m.Notation)
	}
	return out
}

// ensureGameQuota 校验用户额度是否还够发起一次调用。
//
// 判定口径与鉴权中间件【逐字一致】：可用额度 = 总额度 − 已用 − 在途预留，
// 且不限额度的账号直接放行、判定用 <= 0 而不是 == 0。
// 口径若与中间件不同，会出现"正常调用被拦、下棋却能过"（或反之）
// 这种自相矛盾的现象，用户与站长都无法理解。
func (s *Server) ensureGameQuota(ctx context.Context, userID uint64) error {
	owner, err := s.deps.Users.GetByID(ctx, userID)
	if err != nil || owner == nil {
		return errNoQuotaInfo
	}
	if owner.Quota == model.QuotaUnlimited {
		return nil
	}
	pending := int64(0)
	if s.deps.Billing != nil {
		if p, perr := s.deps.Billing.PendingReserved(ctx, userID); perr == nil {
			pending = p
		}
	}
	if owner.AvailableQuota(pending) <= 0 {
		return errQuotaExhausted
	}
	return nil
}

var (
	// errQuotaExhausted 额度已耗尽。
	errQuotaExhausted = errors.New("额度不足，无法继续对局")
	// errNoQuotaInfo 读不到额度信息（降级为"放行"，由计费链路兜底）。
	errNoQuotaInfo = errors.New("无法读取额度信息")
)

// gameBadRequest 回应"用户能自己改正"的请求问题（400）。
//
// 复用仓库既有的 writeUserError，而不是新造一套响应格式——
// 前端已按那一套解析错误，另起格式会让对弈页的报错显示成空白。
func (s *Server) gameBadRequest(c *gin.Context, message string) {
	writeUserError(c, http.StatusBadRequest, message, oai.TypeInvalidRequest, "invalid_request")
}

// gameNotFound 回应资源不存在（404）。
func (s *Server) gameNotFound(c *gin.Context, message string) {
	writeUserError(c, http.StatusNotFound, message, oai.TypeInvalidRequest, "not_found")
}

// respondQuotaBlocked 回应额度不足。
func (s *Server) respondQuotaBlocked(c *gin.Context, err error) {
	if errors.Is(err, errNoQuotaInfo) {
		// 读不到额度不该拦人：转发链路的计费仍会正常扣费，
		// 拦下来只会让"用户仓库暂时不可读"变成"所有人都不能下棋"。
		return
	}
	c.JSON(http.StatusTooManyRequests, gin.H{
		"error": gin.H{
			"message": "额度不足，无法继续对局。对弈每一步都会真实调用模型并消耗额度。",
			"type":    "rate_limit_error",
			"code":    "insufficient_quota",
		},
	})
}

// dtoOfMove 把棋谱记录转成前端形态。
func dtoOfMove(rec *model.GameMoveRecord) gameMoveDTO {
	return gameMoveDTO{
		Seq:       rec.Seq,
		Color:     rec.Color,
		ColorText: colorText(rec.Color),
		Notation:  rec.Notation,
		Attempts:  rec.Attempts,
		Raw:       rec.RawOutput,
		Error:     rec.ErrorText,
	}
}

// winnerOf 由终局状态推导胜者标识。
func winnerOf(status game.Status) string {
	switch status {
	case game.StatusBlackWin:
		return model.GameWinnerBlack
	case game.StatusWhiteWin:
		return model.GameWinnerWhite
	case game.StatusDraw:
		return model.GameWinnerDraw
	}
	// aborted / playing 时没有胜者：留空而不是填 draw，
	// 让前端与统计口径都能区分"没下完"与"和棋"。
	return ""
}

// gameStatusText 生成对局状态的中文说明。
func gameStatusText(match *model.GameMatch) string {
	switch match.Status {
	case model.GameStatusBlackWin:
		return "黑方胜"
	case model.GameStatusWhiteWin:
		return "白方胜"
	case model.GameStatusDraw:
		return "和棋"
	case model.GameStatusAborted:
		return "已中止（未下完）"
	}
	return "进行中"
}

// kindLabel 棋种中文名。
func kindLabel(kind string) string {
	switch kind {
	case "gomoku":
		return "五子棋"
	case "go":
		return "围棋"
	case "xiangqi":
		return "象棋"
	}
	return kind
}

// modeLabel 模式中文名。
func modeLabel(mode string) string {
	switch mode {
	case model.GameModeAIVsAI:
		return "AI 对 AI"
	case model.GameModeHumanVsAI:
		return "人机对弈"
	}
	return mode
}

// colorText 颜色中文名（象棋习惯叫红黑，这里统一用红/黑以兼顾三棋）。
func colorText(color uint8) string {
	switch color {
	case 1:
		return "黑"
	case 2:
		return "白"
	}
	return ""
}

// parseUintParam 解析路径上的无符号整数参数。
func parseUintParam(c *gin.Context, name string) (uint64, error) {
	return strconv.ParseUint(c.Param(name), 10, 64)
}
