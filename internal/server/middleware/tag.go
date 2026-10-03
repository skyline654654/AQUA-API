// 本文件实现「调用场景标签」的提取中间件。
//
// 意图（Why）：
//
//	站长最需要的不是"用户花了多少"，而是"钱花在哪"——
// 是 Playground 试玩、是某个插件、还是某个脚本在刷。
// 令牌维度能回答"哪把 Key 花的"，但同一把 Key 往往被多个场景复用
// （开发调试 + 线上服务），按令牌切分仍回答不了"哪个场景"。
// 标签是对令牌维度的正交补充：它由客户端声明，用来把用量切回业务语义。
//
// 流转（Flow）：
//
//	v1.Use(middleware.CaptureTag())
//	  └─ X-Aqua-Tag 头 → model.NormalizeTag 归一 → reqctx.WithTag 写入请求 context
//	       └─ relay 落usage_logs 时 reqctx.Tag(ctx) 取出并写入 tag 列
//	         └─ 成本归因按 (user_id, tag) 聚合，回答"钱花在哪"
//
// 安全边界（重要）：
//
//	客户端可以【任意声明】自己的标签——这不影响任何计费，只影响统计口径。
//	但服务端自己发起的调用（站内游乐场）必须由服务端强制注入，
//	不能让客户端把游乐场的流量伪装成别的场景（否则归因数据失去意义）。
//	因此该中间件只在 /v1 外部入口挂载；站内调用由 Playground 处理器
// 在自己的context 上再覆盖一次（见handler 中的 WithTag）。
//
// 扩展（Extend）：
//
//	需要可信标签（如"由官方 SDK 发出"）时，可在中间件里对已知 User-Agent
//	做白名单映射，只有命中才采纳客户端标签；当前版本刻意保持简单——
//	标签是运营统计维度，不是安全维度。
package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/xiaosu4610/aqua-api/internal/model"
	"github.com/xiaosu4610/aqua-api/internal/reqctx"
)

// TagHeader 是客户端声明调用场景的 HTTP 请求头。
const TagHeader = "X-Aqua-Tag"

// CaptureTag 提取 X-Aqua-Tag 请求头并写入请求 context 的中间件。
//
// 归一规则统一走 model.NormalizeTag（去空白 / 截断 32 字节 / 空值归 untagged），
// 与写入侧共用同一套规则——若两边各写一份，必然出现"一个截断一个不截断"
// 导致同一场景被聚合成多行的漂移。
func CaptureTag() gin.HandlerFunc {
	return func(c *gin.Context) {
		tag := model.NormalizeTag(c.GetHeader(TagHeader))
		ctx := reqctx.WithTag(c.Request.Context(), tag)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
