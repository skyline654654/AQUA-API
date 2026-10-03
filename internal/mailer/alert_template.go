// 本文件是「用量预警」邮件的模板。
//
// 意图（Why）：
//
//	预警邮件与验证码邮件的诉求完全不同：
//	  - 验证码邮件是"一次性、必须立刻用、错了就完了"；
//	  - 预警邮件是"告诉你未来的事，给你行动的时间，不用现在做任何操作"。
//	因此结构也不同：验证码把数字放到最醒目的位置，
//	预警则要给出"还能用多久 + 现在该做什么 + 去哪儿充值"三条信息。
//
//	发到哪儿：用户的【注册邮箱】。用户不需要配置额外通道，
//	也不该因为"没配预警通道"而失去账户——这是选注册邮箱而非站内信的唯一理由。
//
// 设计取舍：
//   - 纯内联样式 HTML（理由同 template.go 顶部说明）；
//   - 关键数字（剩余天数、剩余额度）单独成行并加大字号，
//     即使用户只看纯文本也能拿到核心信息；
//   - 不放外链图片，避免被邮件客户端与垃圾邮件过滤器折叠。
package mailer

import (
	"fmt"
	"html"
	"strings"
)

// AlertKind 与 model.AlertKind 同名同值，但在 mailer 包里用字符串参数表示——
// 避免 mailer 反向依赖 model（它已经是"文案层"，不该知道领域枚举）。
//
// 之所以要在两处各写一份常量字面量而不是共用 model 的：
// model 的枚举会因为业务演进而增减，而邮件文案要保持稳定
// （历史邮件要能复现，模板键的语义不能悄悄变）。两处独立反而更安全。

// QuotaAlertEmailInput 是额度预警邮件的可变内容。
type QuotaAlertEmailInput struct {
	// SiteName 是站点显示名。
	SiteName string
	// Username 是收件人用户名（让用户确认"这是给我这封"）。
	Username string
	// Kind 是预警类型：quota_low / quota_drain / usage_spike。
	Kind string
	// RemainQuota 是当前剩余额度（内部计费单位）。
	RemainQuota int64
	// DailyQuota 是近 N 日均消耗（用于让用户理解"还能用多久"是怎么算出来的）。
	DailyQuota int64
	// DaysLeft 是预计还能用多少天；0 表示"已不足一天"。
	DaysLeft int
	// TopModel 是消耗最多的模型（可为空串），给出"钱花在哪"的第一层答案。
	TopModel string
	// TopModelQuota 是该模型的消耗额度。
	TopModelQuota int64
	// PortalURL 是门户充值页地址（空串时不显示按钮）。
	PortalURL string
}

// QuotaAlertEmail 构造额度预警邮件的主题与 HTML 正文。
//
// 主题刻意包含关键数字（如"预计 3 天后耗尽"）：
// 用户在收件箱里先看到主题就能决定要不要点开——预警的价值在于"被看见"。
func QuotaAlertEmail(in QuotaAlertEmailInput) (subject, htmlBody string) {
	var title, lead, headline, headlineUnit string

	switch in.Kind {
	case "quota_drain":
		title = "余额即将耗尽"
		headline = daysLeftText(in.DaysLeft)
		headlineUnit = "按近期用量估算"
		lead = fmt.Sprintf("您的账户余额按近期的使用速度，预计 <strong>%s</strong>后耗尽。", daysLeftText(in.DaysLeft))
	case "usage_spike":
		title = "用量异常增长"
		headline = "增长"
		headlineUnit = "近期用量高于平常"
		lead = "您近期的调用量明显高于平常水平。如果这不是您的操作，请立即检查并更换访问令牌。"
	default: // quota_low
		title = "余额不足提醒"
		headline = daysLeftText(in.DaysLeft)
		headlineUnit = "按近期用量估算"
		lead = fmt.Sprintf("您的账户余额已经偏低，按近期用量预计还能使用 <strong>%s</strong>。", daysLeftText(in.DaysLeft))
	}

	subject = fmt.Sprintf("【%s】%s", in.SiteName, title)

	// 每日消耗展示为空时不占位：没有消耗数据（刚注册）时显示"暂无"比显示 0 更准确。
	dailyText := "暂无消耗数据"
	if in.DailyQuota > 0 {
		dailyText = fmt.Sprintf("%s / 天", formatQuota(in.DailyQuota))
	}

	topModelBlock := ""
	if in.TopModel != "" {
		topModelBlock = fmt.Sprintf(`
    <tr>
      <td style="padding:10px 0;border-bottom:1px solid #f1f5f9;font-size:13px;color:#64748b;">消耗最多的模型</td>
      <td style="padding:10px 0;border-bottom:1px solid #f1f5f9;font-size:13px;color:#0f172a;text-align:right;font-weight:600;">%s（%s）</td>
    </tr>`,
			html.EscapeString(in.TopModel), formatQuota(in.TopModelQuota))
	}

	actionBlock := ""
	if in.PortalURL != "" {
		actionBlock = fmt.Sprintf(`
    <p style="margin:22px 0 0;">
      <a href="%s" style="display:inline-block;background:#0891b2;color:#ffffff;text-decoration:none;
         padding:11px 26px;border-radius:10px;font-size:14px;font-weight:600;">前往充值</a>
    </p>`,
			html.EscapeString(in.PortalURL))
	}

	htmlBody = fmt.Sprintf(`
<!DOCTYPE html>
<html lang="zh-CN">
<head><meta charset="utf-8"></head>
<body style="margin:0;padding:24px 12px;background:#f8fafc;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;">
  <div style="max-width:520px;margin:0 auto;background:#ffffff;border:1px solid #e2e8f0;border-radius:14px;padding:28px;">
    <h1 style="margin:0 0 8px;font-size:18px;font-weight:600;color:#0f172a;">%s</h1>
    <p style="margin:0 0 20px;font-size:13px;color:#64748b;">%s，您好</p>

    <div style="background:#fff7ed;border:1px solid #fed7aa;border-radius:12px;padding:18px;text-align:center;">
      <div style="font-size:30px;font-weight:700;color:#ea580c;letter-spacing:1px;">%s</div>
      <div style="margin-top:6px;font-size:12px;color:#c2410c;">%s</div>
    </div>

    <p style="margin:20px 0 0;font-size:13px;color:#475569;line-height:1.75;">%s</p>

    <table style="width:100%%;border-collapse:collapse;margin-top:20px;">
      <tr>
        <td style="padding:10px 0;border-bottom:1px solid #f1f5f9;font-size:13px;color:#64748b;">当前剩余额度</td>
        <td style="padding:10px 0;border-bottom:1px solid #f1f5f9;font-size:13px;color:#0f172a;text-align:right;font-weight:600;">%s</td>
      </tr>
      <tr>
        <td style="padding:10px 0;border-bottom:1px solid #f1f5f9;font-size:13px;color:#64748b;">近期日均消耗</td>
        <td style="padding:10px 0;border-bottom:1px solid #f1f5f9;font-size:13px;color:#0f172a;text-align:right;font-weight:600;">%s</td>
      </tr>%s
    </table>
%s
    <hr style="margin:22px 0 14px;border:none;border-top:1px solid #e2e8f0;">
    <p style="margin:0;font-size:12px;color:#94a3b8;line-height:1.7;">
      本邮件由系统根据您的用量自动发送，请勿直接回复。<br>
      若这不是您的操作，请立即登录控制台更换访问令牌。
    </p>
  </div>
</body>
</html>`,
		html.EscapeString(title),
		html.EscapeString(in.Username),
		html.EscapeString(headline),
		html.EscapeString(headlineUnit),
		lead,
		formatQuota(in.RemainQuota),
		dailyText,
		topModelBlock,
		actionBlock,
	)

	return subject, htmlBody
}

// daysLeftText 把"剩余天数"渲染成一句人话。
//
// 为什么不直接显示"0 天"：那读起来像"已经没救了"，
// 而实际含义是"今天之内会用完"，提示强度完全不同。
func daysLeftText(days int) string {
	switch {
	case days <= 0:
		return "不足 1 天"
	case days == 1:
		return "约 1 天"
	default:
		return fmt.Sprintf("约 %d 天", days)
	}
}

// formatQuota 把内部额度单位渲染为可读文本。
//
// 内部额度是 1 元 = 若干单位的换算关系由站点配置决定，
// 邮件里刻意【不】换算成金额：同一个"10000"在不同站点含义不同，
// 写死换算会让邮件说错话。展示原始额度单位是诚实且不会错的做法。
func formatQuota(v int64) string {
	if v <= 0 {
		return "0"
	}
	// 千分位分隔，便于快速读出数量级。
	s := fmt.Sprintf("%d", v)
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return strings.Join(parts, ",")
}
