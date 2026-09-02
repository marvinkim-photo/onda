// Package alimtalk — 카카오 알림톡의 벤더 중립 인터페이스.
//
// 알림톡은 카카오가 정의한 하나의 논리 채널이지만 실제 발송은 딜러사(벤더)를 거쳐야 하고,
// 벤더마다 API가 근본적으로 다르다. 이 패키지는 NHN Cloud 스펙을 기준으로 공용 계약을 정의하고,
// 각 벤더가 Vendor를 구현한 플러그인으로 들어오게 한다.
//
// 벤더 차이는 무한하지 않고 네 축으로 수렴한다(2026-09-02 공식 문서 실측):
//
//  1. 변수 치환을 누가 하는가 — NHN templateParameter(공급자 렌더) vs 알리고 message_N(완성 본문)
//     → SendRequest가 Variables와 RenderedText를 둘 다 싣고, manifest.capabilities.substitution이 고른다.
//  2. 대체발송을 누가 하는가 — NHN resendParameter / 알리고 failover vs 엔진 폴백 체인
//     → capabilities.vendor_fallback이 선언하고, 미지원 벤더만 엔진이 폴백한다.
//  3. 결과를 밀어주는가 당겨야 하는가 — 웹훅(Solapi) vs 조회 폴링(NHN·알리고)
//     → ParseCallback / PollResults 중 최소 하나를 구현하고 lifecycle_mode로 선언한다.
//  4. 식별자가 단일인가 복합인가 — 알리고 mid vs NHN requestId+recipientSeq
//     → Receipt.ProviderMessageID 문자열 하나로 정규화하고, 복합키는 벤더가 인코딩한다.
package alimtalk

import (
	"context"
	"time"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/connector"
)

// ChannelID — send.message.v1 · message.lifecycle.v1의 channel 값.
const ChannelID = "kakao_alimtalk"

// CredentialKind — PG channel_kind enum 값. 벤더별로 나누지 않는다.
// 벤더 식별은 channel_connectors.connector_id가 하며, 그래야 제3자 벤더가
// enum 마이그레이션 없이 들어올 수 있다.
const CredentialKind = "alimtalk"

// Vendor — 알림톡 딜러사 플러그인이 구현하는 인터페이스.
//
// in-process Go 구현체와 remote HTTP 어댑터가 모두 이 인터페이스를 만족한다.
// 구현체는 반드시 manifest의 contract_tests 9종을 통과해야 한다(conformance 패키지).
type Vendor interface {
	// Manifest — 자기 선언. 레지스트리가 로드한 파일과 일치해야 하며 계약 테스트가 대조한다.
	Manifest() connector.Manifest

	// Validate — 크리덴셜 실검증. 가능하면 발신프로필·잔액 조회처럼 무해한 호출로 확인한다.
	//
	// 주의: 잘못된 키는 반드시 FailureCredentialAuth로 분류해야 한다.
	// channel.Verifier는 InvalidTarget·PermanentContent를 "인증은 통과"로 읽어 verified로
	// 표시하므로(verifier.go judge), 400을 주는 공급자를 그대로 두면 잘못된 키가 검증 통과한다.
	// Resend에서 실제로 겪은 함정이다(email_resend.go classifyResend 주석).
	Validate(ctx context.Context, cred Credential) error

	// Send — 단건 발송. 반환은 공급자 "접수"이지 단말 도달이 아니다.
	// 도달·실패는 ParseCallback 또는 PollResults로 나중에 확정된다.
	Send(ctx context.Context, req SendRequest) (Receipt, error)

	// Classify — 오류를 재시도·폴백·크리덴셜 정지 판단 근거로 환원한다.
	//
	// 대부분 channel.Classify 한 줄 위임이지만 인터페이스에 남긴다. 실제 딜러사는 HTTP 상태가
	// 아니라 본문의 결과 코드로 성패를 알린다(NHN resultCode MRC01/MRC02, 알리고 code 0/-99/509).
	// 공급자 코드를 재시도 정책으로 옮기는 일은 벤더만 할 수 있다.
	Classify(err error) channel.FailureClass

	// ParseCallback — 공급자 웹훅 원문을 수명주기 이벤트로 바꾼다.
	// 폴링 전용 벤더는 (nil, ErrUnsupported)를 반환한다.
	ParseCallback(ctx context.Context, cb RawCallback) ([]Event, error)

	// PollResults — 미종결 접수들의 결과를 조회한다.
	// 콜백 전용 벤더는 (nil, ErrUnsupported)를 반환한다.
	PollResults(ctx context.Context, cred Credential, pending []Receipt) ([]Event, error)

	// ListTemplates — 승인된 템플릿 목록. 본문(Content)은 알리고처럼 완성 텍스트를 요구하는
	// 벤더를 지원하기 위한 렌더 원본이므로, 지원한다면 반드시 채워야 한다.
	ListTemplates(ctx context.Context, cred Credential, senderKey string) ([]Template, error)
}

// Credential — 복호화된 벤더 크리덴셜. 원문 JSON은 벤더가 자기 스키마로 파싱한다.
// 엔진은 절대 내용을 해석하지 않으며, 큐에는 credential_ref만 흐른다.
type Credential struct {
	ConnectorID string
	JSON        []byte
	// Config — 비밀이 아닌 앱 단위 설정(channel_connectors.config). 발신번호·기본 발신프로필 등.
	Config []byte
}

// SendRequest — 벤더 중립 발송 요청. NHN 스펙이 기준이다.
type SendRequest struct {
	// MessageID — Onda 안정 발송 ID(UUID). 재시도·재전달에도 불변이다.
	//
	// **멱등의 기준은 MessageID다.** 벤더는 공급자 멱등 헤더(NHN X-NC-API-IDEMPOTENCY-KEY 등)에
	// 이 값을 싣고, 같은 MessageID로 두 번 발송되면 같은 Receipt.ProviderMessageID를 돌려줘야 한다
	// (conformance idempotent_resend가 검증한다).
	//
	// IdempotencyKey는 엔진 내부의 중복 방지 키
	// (journey_id, version, user_id, node_index, endpoint_id — CLAUDE.md 규칙 6)로,
	// 사람이 읽는 계보 문자열이다. 길이·문자 제약이 공급자마다 달라 신뢰할 수 없으니
	// 벤더는 공급자에 넘기지 말고 로깅·추적에만 쓴다.
	MessageID      string
	IdempotencyKey string

	// Credential — 복호화된 벤더 크리덴셜. 벤더 인스턴스는 manifest만으로 만들어지는
	// 무상태 싱글턴이므로(Registry.Get), 앱별 비밀은 요청마다 실어 넘긴다.
	// Validate·PollResults가 Credential을 인자로 받는 것과 같은 이유다.
	Credential Credential

	SenderKey    string // 발신프로필 키 (NHN senderKey / Solapi pfId / 알리고 senderkey)
	TemplateCode string
	To           string // E.164

	// Variables — 치환자 키-값. substitution이 variables|both인 벤더가 쓴다.
	// 키는 카카오 원본 표기(#{name})가 아니라 변수명만 담는다(name).
	Variables map[string]string
	// RenderedText — 승인 본문에 치환을 마친 완성 텍스트. substitution이 rendered|both인 벤더가 쓴다.
	// 승인 템플릿과 정확히 일치해야 하므로 엔진이 alimtalk_templates.content로 렌더한다.
	RenderedText string

	Buttons      []Button
	QuickReplies []QuickReply

	// Fallback — 벤더 대체발송. capabilities.vendor_fallback이 true일 때만 채워진다.
	Fallback *Fallback

	// ScheduledAt — 예약 발송. capabilities.scheduled_send가 true일 때만 채워진다.
	ScheduledAt *time.Time
}

// Button — 카카오 원본 버튼. 타입 코드는 카카오 정의를 그대로 쓰고 필드명만 정규화한다.
//
//	WL 웹링크(LinkMo 필수) · AL 앱링크(LinkIOS/LinkAndroid 중 2개) · DS 배송조회
//	BK 봇키워드 · MD 메시지전달 · BC 상담톡 · BT 봇전환 · AC 채널추가(광고추가형·복합형만)
type Button struct {
	Type        string `json:"type"`
	Name        string `json:"name"` // 최대 28자
	LinkMo      string `json:"link_mo,omitempty"`
	LinkPC      string `json:"link_pc,omitempty"`
	LinkIOS     string `json:"link_ios,omitempty"`
	LinkAndroid string `json:"link_android,omitempty"`
}

type QuickReply struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	LinkMo      string `json:"link_mo,omitempty"`
	LinkPC      string `json:"link_pc,omitempty"`
	LinkIOS     string `json:"link_ios,omitempty"`
	LinkAndroid string `json:"link_android,omitempty"`
}

// Fallback — 알림톡 실패 시 문자 대체발송.
type Fallback struct {
	Type     string `json:"type"` // SMS | LMS
	Title    string `json:"title,omitempty"`
	Text     string `json:"text"`
	SenderNo string `json:"sender_no"` // 사전 등록된 발신번호
}

// Receipt — 공급자 접수 결과. 폴링형 벤더는 이 값으로 나중에 결과를 조회한다.
type Receipt struct {
	// ProviderMessageID — 공급자 식별자. 복합키(NHN requestId+recipientSeq)는 벤더가
	// 한 문자열로 인코딩하고 PollResults에서 역해석한다.
	ProviderMessageID string
	// MessageID — 이 접수에 대응하는 Onda message_id. 폴링 결과를 되돌릴 때 쓴다.
	MessageID string
	// AcceptedAt — 공급자가 접수한 시각.
	AcceptedAt time.Time
}

// Event — 수명주기 이벤트. message.lifecycle.v1로 그대로 옮겨진다.
type Event struct {
	MessageID         string
	ProviderMessageID string
	Status            string // sent | delivered | failed | ... (manifest.lifecycle.reports 내)
	OccurredAt        time.Time
	FailureClass      string
	FailureDetail     string
	// Cost — 공급자가 건당 원가를 알려주면 채운다. 채널 간 원가 비교의 유일한 출처다.
	CostCurrency string
	CostAmount   float64
	// Terminal — 더 이상 상태가 바뀌지 않는다. 폴러가 이 접수를 대기 목록에서 지운다.
	Terminal bool

	// DeliveredVia — 실제로 어느 채널로 나갔는가. 벤더 대체발송(vendor_fallback)이 동작하면
	// 알림톡이 아니라 SMS/LMS로 도달하는데, 이걸 문자열 사유에만 남기면 채널별 도달률과
	// 원가 집계가 조용히 틀어진다. 대체발송이 아니면 빈 문자열(= 원 채널).
	// 예: "sms" · "lms"
	DeliveredVia string

	// Expired — 공급자 조회 보존 기간이 지나 결과를 더 이상 알 수 없다.
	// Terminal과 구분한다: Terminal은 "결과가 확정됐다", Expired는 "결과를 못 알아냈다"이다.
	// 둘을 섞으면 알 수 없는 건이 성공/실패로 집계되거나 폴러가 영원히 물어본다.
	// 폴러는 대기 목록에서 지우되 도달 집계에는 넣지 않는다.
	Expired bool
}

// RawCallback — 공급자 웹훅 원문. API는 서명 검증만 하고 파싱은 벤더가 한다.
// 벤더 로직이 Go 한 곳에만 있게 하는 경계다.
type RawCallback struct {
	ConnectorID string
	Body        []byte
	Headers     map[string]string
	Query       map[string]string
}

// Template — 벤더에서 동기화한 승인 템플릿.
type Template struct {
	Code          string
	Name          string
	Content       string // 승인 본문 (#{변수} 포함) — RenderedText 생성의 원본
	MessageType   string // BA 기본형 | EX 부가정보형 | AD 광고추가형 | MI 복합형
	EmphasizeType string // NONE | TEXT | IMAGE | ITEM_LIST
	Buttons       []Button
	QuickReplies  []QuickReply
	Status        TemplateStatus
	VendorStatus  string // 공급자 원본 상태 코드 (그대로 보존)
	UpdatedAt     time.Time
}

type TemplateStatus string

const (
	TemplateApproved TemplateStatus = "approved"
	TemplatePending  TemplateStatus = "pending"
	TemplateRejected TemplateStatus = "rejected"
)

// IsAd — 광고성 템플릿인가. 정보성(BA·EX)은 정보통신망법상 야간 발송 제한 예외이고
// (광고) 표기·수신거부가 불필요하지만, 광고추가형(AD)·복합형(MI)은 광고성이라
// 야간 제한과 수신거부가 적용된다. 이 값이 정책 category를 결정한다.
func (t Template) IsAd() bool { return t.MessageType == "AD" || t.MessageType == "MI" }

// Category — 정책 엔진 category. 사용자가 알림톡 노드에서 임의로 바꾸지 못하게 하고
// 템플릿 유형에서 도출한다. 정보성 알림톡이 빈도 상한에 걸려 주문 알림이 안 나가면 사고다.
func (t Template) Category() string {
	if t.IsAd() {
		return "marketing"
	}
	return "transactional"
}
