package journey

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
	"github.com/ondahq/onda/apps/worker/internal/policy"
	libqueue "github.com/ondahq/onda/packages/libqueue-go"
)

// 알림톡 노드 → send.message.v1 outbox.
//
// 푸시·이메일은 채널 전용 스트림(send.push / send.email)을 쓰지만 알림톡은 채널 중립 계약인
// send.message.v1로 나간다. 벤더 플러그인 구조가 그 계약 위에 서 있기 때문이다.

// phoneDigits — 숫자만 남긴다.
var phoneDigits = regexp.MustCompile(`\D`)

// telNamespace — 전화번호에서 결정적 endpoint_id를 만들 때 쓰는 네임스페이스.
// 전화번호는 디바이스처럼 레지스트리가 없으므로 번호에서 UUID를 유도해
// 멱등 키의 마지막 요소(엔드포인트 식별자)를 안정적으로 만든다.
var telNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("onda:endpoint:tel"))

// normalizeKoreanPhone — 국내 번호를 E.164로. 알림톡은 국내 전용이라 KR을 기본으로 본다.
// 정규화할 수 없으면 빈 문자열.
func normalizeKoreanPhone(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "+") {
		d := phoneDigits.ReplaceAllString(v, "")
		if len(d) < 8 || len(d) > 15 {
			return ""
		}
		return "+" + d
	}
	d := phoneDigits.ReplaceAllString(v, "")
	switch {
	case strings.HasPrefix(d, "82"):
		d = strings.TrimPrefix(d, "82")
		d = strings.TrimPrefix(d, "0")
	case strings.HasPrefix(d, "0"):
		d = strings.TrimPrefix(d, "0")
	default:
		return "" // 국가·국내 접두가 없으면 판단하지 않는다(잘못된 대상 발송 방지)
	}
	if len(d) < 8 || len(d) > 11 {
		return ""
	}
	return "+82" + d
}

func phoneEndpointID(e164 string) string {
	return uuid.NewSHA1(telNamespace, []byte(e164)).String()
}

// alimtalkBinding — 발송에 필요한 앱 설정 묶음. 한 번의 조인으로 읽는다.
type alimtalkBinding struct {
	connectorID   string
	senderKey     string
	templateCode  string
	content       string
	messageType   string
	templateState string
	buttons       []byte
}

// loadAlimtalkBinding — 발신프로필·승인 템플릿·채널 배선을 한 번에 읽는다.
// 커넥터 배선이 없으면 connectorID가 빈 문자열로 오고 호출자가 설정 누락으로 처리한다.
func (s *Scheduler) loadAlimtalkBinding(ctx context.Context, tx pgx.Tx, tenantID, appID, senderID, templateCode string) (*alimtalkBinding, error) {
	var b alimtalkBinding
	var connectorID, buttons *string
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(cc.connector_id, ''), s.sender_key, t.template_code, t.content,
		       COALESCE(t.message_type, 'BA'), COALESCE(t.status, ''), COALESCE(t.buttons::text, '')
		  FROM alimtalk_senders s
		  JOIN alimtalk_templates t
		    ON t.app_id = s.app_id AND t.sender_id = s.id AND t.template_code = $4
		  LEFT JOIN channel_connectors cc
		    ON cc.app_id = s.app_id AND cc.channel = $5 AND cc.enabled
		 WHERE s.tenant_id = $1 AND s.app_id = $2 AND s.id = $3`,
		tenantID, appID, senderID, templateCode, alimtalk.ChannelID).
		Scan(&connectorID, &b.senderKey, &b.templateCode, &b.content, &b.messageType, &b.templateState, &buttons)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if connectorID != nil {
		b.connectorID = *connectorID
	}
	if buttons != nil && *buttons != "" {
		b.buttons = []byte(*buttons)
	}
	return &b, nil
}

// enqueueAlimtalk — 알림톡 노드를 send.message.v1 outbox로 적재한다.
//
// 정책 주의: 알림톡 정보성(BA·EX)은 정보통신망법상 야간 발송 제한 예외이고 빈도 상한 대상도 아니다.
// 광고추가형·복합형(AD·MI)은 광고성이라 야간 제한과 수신거부가 적용된다.
// 그래서 category를 저니 설정이 아니라 **템플릿 유형에서 도출**한다. 사용자가 임의로 못 바꾼다.
// 저니가 transactional이라 상위에서 quiet hours를 통과했더라도 템플릿이 광고성이면 여기서 다시 막는다
// (조이는 방향으로만 재판정한다 — 야간 광고 발송은 위법이다).
func (s *Scheduler) enqueueAlimtalk(ctx context.Context, tx pgx.Tx, c *claimedState, def *Definition,
	node Node, pol *appPolicy, sub map[string]string, attrs map[string]string) (string, error) {
	at := node.Alimtalk
	phone := normalizeKoreanPhone(attrs["phone"])
	if phone == "" {
		s.logSkipReason(ctx, c, def, "skipped_unreachable", alimtalk.ChannelID, "전화번호 없음/형식 오류")
		return "skipped_unreachable", nil
	}

	b, err := s.loadAlimtalkBinding(ctx, tx, c.tenantID, c.appID, at.SenderID, at.TemplateCode)
	if err != nil {
		return "", fmt.Errorf("알림톡 설정 조회: %w", err)
	}
	if b == nil {
		s.logSkipReason(ctx, c, def, "skipped_unreachable", alimtalk.ChannelID, "발신프로필·템플릿을 찾을 수 없음")
		return "skipped_unreachable", nil
	}
	if b.connectorID == "" {
		s.logSkipReason(ctx, c, def, "skipped_unreachable", alimtalk.ChannelID, "알림톡 발송기(벤더) 미설정")
		return "skipped_unreachable", nil
	}
	if b.templateState != string(alimtalk.TemplateApproved) {
		s.logSkipReason(ctx, c, def, "skipped_unreachable", alimtalk.ChannelID,
			"승인되지 않은 템플릿("+b.templateState+")")
		return "skipped_unreachable", nil
	}

	tpl := alimtalk.Template{Content: b.content, MessageType: b.messageType}
	cat := policy.Category(tpl.Category())
	marketing := cat != policy.Transactional

	if marketing {
		// 광고성 알림톡: 수신거부 존중 + 야간 제한 재판정.
		if sub[alimtalk.ChannelID] == "unsubscribed" || sub["alimtalk"] == "unsubscribed" {
			s.logSkipReason(ctx, c, def, "skipped_unreachable", alimtalk.ChannelID, "수신거부")
			return "skipped_unreachable", nil
		}
		qd, qerr := policy.EvaluateQuietHours(cat, pol.quietHours, pol.tz, s.clk.Now())
		if qerr != nil {
			return "", qerr
		}
		if qd.Action != policy.ActionSend {
			s.logSkipReason(ctx, c, def, "skipped_quiet_hours", alimtalk.ChannelID, "광고성 템플릿 야간 제한")
			return "skipped_quiet_hours", nil
		}
	}

	allowed, err := s.freqCap.Allow(ctx, cat, pol.freqCap, c.appID, c.userID)
	if err != nil {
		return "", err
	}
	if !allowed {
		s.logSkipReason(ctx, c, def, "skipped_cap", alimtalk.ChannelID, "")
		return "skipped_cap", nil
	}

	// 치환자: 노드가 매핑한 값에 {{ }} 개인화를 적용한다.
	vars := make(map[string]string, len(at.Variables))
	for k, v := range at.Variables {
		vars[k] = Render(v, attrs)
	}
	// 완성 본문: 알리고처럼 승인 본문과 정확히 일치하는 텍스트를 요구하는 벤더용.
	// 여기서 실패하면 공급자에 보내기 전에 걸러 과금과 거절을 아낀다.
	rendered, err := alimtalk.Render(b.content, vars)
	if err != nil {
		s.logSkipReason(ctx, c, def, "skipped_unreachable", alimtalk.ChannelID, err.Error())
		return "skipped_unreachable", nil
	}

	endpointID := phoneEndpointID(phone)
	idemKey := sendKey(c, def, endpointID)
	basis := "transactional"
	if marketing {
		basis = "opt_in"
	}
	template := map[string]any{
		"code":             b.templateCode,
		"variables":        vars,
		"sender_key":       b.senderKey,
		"rendered_preview": rendered,
	}
	content := map[string]any{"template": template}
	if len(b.buttons) > 0 {
		var buttons []any
		if json.Unmarshal(b.buttons, &buttons) == nil && len(buttons) > 0 {
			content["buttons"] = buttons
		}
	}
	payload := map[string]any{
		"idempotency_key": idemKey,
		"message_id":      uuidString(),
		"channel":         alimtalk.ChannelID,
		"connector":       map[string]any{"id": b.connectorID, "version": "0.0.0"},
		"user_id":         c.userID,
		"target": map[string]any{
			"type":        "phone",
			"endpoint_id": endpointID,
			"value":       phone,
			"country":     "KR",
		},
		"content":  content,
		"category": string(cat),
		"consent":  map[string]any{"basis": basis},
		"policy": map[string]any{
			"quiet_hours":       quietDecisionLabel(marketing),
			"frequency_cap":     capLabel(marketing),
			"ad_label_required": marketing,
		},
		"locale":          "ko-KR",
		"journey_id":      c.journeyID,
		"journey_version": c.version,
		"node_index":      c.currentNode,
	}
	if at.Fallback != nil {
		payload["fallback"] = []any{map[string]any{
			"channel":   "sms",
			"connector": map[string]any{"id": b.connectorID, "version": "0.0.0"},
			"target":    map[string]any{"type": "phone", "endpoint_id": endpointID, "value": phone, "country": "KR"},
			"content": map[string]any{"text": map[string]any{
				"title": at.Fallback.Title,
				"body":  Render(at.Fallback.Text, attrs),
			}},
			"on": []string{"invalid_target", "permanent_content", "retry_exhausted", "unsupported"},
		}}
	}

	payloadJSON, _ := json.Marshal(payload)
	if _, err := tx.Exec(ctx, `
		INSERT INTO journey_outbox (tenant_id, app_id, stream, idempotency_key, payload)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
		c.tenantID, c.appID, libqueue.StreamSendMessage, idemKey, payloadJSON); err != nil {
		return "", err
	}
	return "queued", nil
}

// quietDecisionLabel — send.message.v1 policy.quiet_hours 값. 정보성은 규제 예외로 우회했음을 남긴다.
func quietDecisionLabel(marketing bool) string {
	if marketing {
		return "allowed"
	}
	return "bypassed_transactional"
}

func capLabel(marketing bool) string {
	if marketing {
		return "allowed"
	}
	return "bypassed_transactional"
}
