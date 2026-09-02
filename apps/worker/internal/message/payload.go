// Package message — send.message.v1(채널 중립 발송 계약) 소비 워커.
//
// 기존 send.push / send.email 워커는 채널마다 payload 형태가 달랐다. send.message.v1은
// 모든 채널이 하나의 형태로 커넥터에 도달하게 하며, 이 패키지가 그 첫 소비자다.
// 알림톡이 첫 채널이지만 이 워커 자체는 채널 중립이다 — 채널별 지식은 Send/Row 두 곳에만 있다.
//
// 계약: packages/queue-schemas/schemas/send.message.v1.schema.json
package message

import (
	"encoding/json"
	"fmt"

	"github.com/ondahq/onda/apps/worker/internal/connector"
)

// Payload — send.message.v1의 Go 투영. 워커가 실제로 쓰는 필드만 담고,
// 모르는 필드는 무시한다(스키마가 앞서가도 워커가 죽지 않게).
type Payload struct {
	IdempotencyKey string    `json:"idempotency_key"`
	MessageID      string    `json:"message_id"`
	Channel        string    `json:"channel"`
	Connector      Connector `json:"connector"`
	UserID         string    `json:"user_id"`
	Target         Target    `json:"target"`
	Content        Content   `json:"content"`
	Category       string    `json:"category"`
	Consent        Consent   `json:"consent"`
	Policy         Policy    `json:"policy"`
	Locale         string    `json:"locale"`
	Options        Options   `json:"options"`
	Fallback       []Step    `json:"fallback"`

	JourneyID      *string           `json:"journey_id"`
	JourneyVersion *int              `json:"journey_version"`
	NodeIndex      *int              `json:"node_index"`
	CampaignRef    *string           `json:"campaign_ref"`
	Metadata       map[string]string `json:"metadata"`
}

type Connector struct {
	ID            string `json:"id"`
	Version       string `json:"version"`
	CredentialRef string `json:"credential_ref"`
}

// Target — 수신 엔드포인트. EndpointID가 멱등 키의 마지막 요소이자 message_log의 device_id다
// (CLAUDE.md 규칙 6의 채널 중립 확장 — 누락되면 다중 엔드포인트 미발송 버그가 난다).
type Target struct {
	Type       string `json:"type"`
	EndpointID string `json:"endpoint_id"`
	Value      string `json:"value"`
	Platform   string `json:"platform"`
	Country    string `json:"country"`
}

// Content — 채널 capability별 렌더 완료 본문. 최소 하나가 있어야 한다.
// 이 워커가 해석하지 않는 종류(push·email·webhook·in_app)는 원문 그대로 보존만 한다.
type Content struct {
	Push     json.RawMessage `json:"push,omitempty"`
	Email    json.RawMessage `json:"email,omitempty"`
	Webhook  json.RawMessage `json:"webhook,omitempty"`
	InApp    json.RawMessage `json:"in_app,omitempty"`
	Text     *TextContent    `json:"text,omitempty"`
	Template *Template       `json:"template,omitempty"`
	Buttons  []Button        `json:"buttons,omitempty"`
}

// IsEmpty — content minProperties:1 위반 여부.
func (c Content) IsEmpty() bool {
	return len(c.Push) == 0 && len(c.Email) == 0 && len(c.Webhook) == 0 && len(c.InApp) == 0 &&
		c.Text == nil && c.Template == nil && len(c.Buttons) == 0
}

type TextContent struct {
	Title    string `json:"title,omitempty"`
	Body     string `json:"body"`
	Sender   string `json:"sender,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// Template — 사전 승인 템플릿 채널(알림톡 등).
// RenderedPreview는 엔진이 승인 본문에 치환을 마친 완성 텍스트다. substitution=rendered인
// 벤더(알리고 등)에게는 이것이 발송 본문 그 자체라 "미리보기"가 아니라 필수 입력이다.
type Template struct {
	Code            string            `json:"code"`
	Variables       map[string]string `json:"variables"`
	SenderKey       string            `json:"sender_key,omitempty"`
	RenderedPreview string            `json:"rendered_preview,omitempty"`
}

// Button — send.message.v1의 정규화 버튼. 카카오 원본 코드(WL/AL/…)가 아니라
// 채널 중립 이름을 쓰므로 알림톡 경계에서 되돌린다(worker.go mapButtons).
type Button struct {
	Type          string `json:"type"`
	Name          string `json:"name"`
	URLMobile     string `json:"url_mobile,omitempty"`
	URLPC         string `json:"url_pc,omitempty"`
	SchemeIOS     string `json:"scheme_ios,omitempty"`
	SchemeAndroid string `json:"scheme_android,omitempty"`
}

// Consent — 발송 근거. 정보통신망법 증빙이라 커넥터가 아니라 엔진이 채운다.
type Consent struct {
	Basis     string  `json:"basis"`
	OptedInAt *string `json:"opted_in_at"`
	Source    *string `json:"source"`
}

// Policy — 엔진이 이미 판정한 결과. 커넥터는 신뢰하고 재판정하지 않는다.
type Policy struct {
	QuietHours      string  `json:"quiet_hours,omitempty"`
	FrequencyCap    string  `json:"frequency_cap,omitempty"`
	AdLabelRequired bool    `json:"ad_label_required,omitempty"`
	UnsubscribeRef  *string `json:"unsubscribe_ref"`
}

type Options struct {
	TTLSeconds   int     `json:"ttl_seconds,omitempty"`
	CollapseKey  string  `json:"collapse_key,omitempty"`
	Priority     string  `json:"priority,omitempty"`
	ScheduledFor *string `json:"scheduled_for"`
}

// Step — fallback 체인 한 단계. 알림톡→SMS 대체발송이 첫 용례다.
type Step struct {
	Channel   string    `json:"channel"`
	Connector Connector `json:"connector"`
	Target    Target    `json:"target"`
	Content   Content   `json:"content"`
	On        []string  `json:"on"`
}

var validBasis = map[string]bool{
	"opt_in": true, "transactional": true, "legitimate_interest": true, "test": true,
}

// Validate — 스키마가 명시한 불변식 중 워커가 의존하는 것만 확인한다.
// 여기서 걸린 payload는 재처리해도 결과가 같으므로 SendLoop이 ACK 후 버린다.
func (p *Payload) Validate() error {
	switch {
	case p.IdempotencyKey == "":
		return fmt.Errorf("idempotency_key 누락")
	case p.MessageID == "":
		return fmt.Errorf("message_id 누락")
	case p.Channel == "":
		return fmt.Errorf("channel 누락")
	case !connector.IDPattern.MatchString(p.Connector.ID):
		return fmt.Errorf("connector.id 형식 오류: %q", p.Connector.ID)
	case p.UserID == "":
		return fmt.Errorf("user_id 누락")
	case p.Target.EndpointID == "":
		// 멱등 키의 마지막 요소. 없으면 같은 사용자의 두 번째 엔드포인트가 통째로 누락된다.
		return fmt.Errorf("target.endpoint_id 누락")
	case p.Consent.Basis == "":
		return fmt.Errorf("consent.basis 누락")
	case !validBasis[p.Consent.Basis]:
		return fmt.Errorf("consent.basis 불명: %q", p.Consent.Basis)
	case p.Content.IsEmpty():
		return fmt.Errorf("content 비어 있음")
	}
	return nil
}

// Parse — envelope payload → Payload. 알 수 없는 필드는 허용한다.
func Parse(raw []byte) (*Payload, error) {
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("send.message payload 파싱: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}
