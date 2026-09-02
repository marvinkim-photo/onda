package message

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	libqueue "github.com/ondahq/onda/packages/libqueue-go"
)

// lifecycle.go — message.lifecycle.v1 발행.
//
// 지금까지 Go 워커 중 수명주기를 발행하는 곳은 없었다(SDK와 Resend 웹훅만 넣었다).
// 커넥터가 자기 발송 결과를 스스로 발행해야 message_lifecycle이 채널 간 성과 비교의
// 단일 기준이 된다 — 그렇지 않으면 알림톡은 message_log에만 있고 리포트에서 사라진다.

const lifecycleEventType = "message.lifecycle"

// lifecycleClass — 워커 내부 실패 분류 → message.lifecycle.v1 failure_class enum.
//
// SendLoop은 재시도 소진 시 "retryable_exhausted"처럼 접미사를 붙이고, 크리덴셜 미해석은
// "credential_missing"을 쓴다. 둘 다 스키마 enum에 없으므로 여기서 정규화한다.
// (enum 밖 값을 그대로 실으면 lifecycle 소비자가 payload 불량으로 통째로 버린다.)
var lifecycleClass = map[string]string{
	"retryable":                 "retryable",
	"rate_limited":              "rate_limited",
	"permanent_content":         "permanent_content",
	"invalid_target":            "invalid_target",
	"credential_auth":           "credential_auth",
	"unsupported":               "unsupported",
	"retry_exhausted":           "retry_exhausted",
	"credential_missing":        "credential_auth",
	"retryable_exhausted":       "retry_exhausted",
	"rate_limited_exhausted":    "retry_exhausted",
	"invalid_target_exhausted":  "retry_exhausted",
	"credential_auth_exhausted": "retry_exhausted",
}

// normalizeClass — enum으로 환원하고, 원래 값이 달랐으면 detail 앞에 원문을 남긴다.
func normalizeClass(class, detail string) (string, string) {
	if class == "" {
		return "", detail
	}
	mapped, ok := lifecycleClass[class]
	if !ok {
		mapped = "unsupported"
	}
	if mapped != class {
		if detail == "" {
			return mapped, class
		}
		return mapped, class + ": " + detail
	}
	return mapped, detail
}

// lifecycleEvent — 발행 대상 한 건. 스키마 필드명을 그대로 쓴다.
type lifecycleEvent struct {
	MessageID         string  `json:"message_id"`
	IdempotencyKey    string  `json:"idempotency_key,omitempty"`
	Status            string  `json:"status"`
	OccurredAt        string  `json:"occurred_at"`
	Source            string  `json:"source"`
	Channel           string  `json:"channel"`
	ConnectorID       string  `json:"connector_id"`
	ProviderMessageID *string `json:"provider_message_id"`
	UserID            *string `json:"user_id"`
	EndpointID        *string `json:"endpoint_id"`
	FailureClass      *string `json:"failure_class"`
	FailureDetail     *string `json:"failure_detail"`
	FallbackIndex     *int    `json:"fallback_index"`
	Attempt           *int    `json:"attempt"`
	Cost              *struct {
		Currency string  `json:"currency"`
		Amount   float64 `json:"amount"`
	} `json:"cost,omitempty"`
}

// maxDetail — 스키마 failure_detail maxLength.
const maxDetail = 1024

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	if len(s) > maxDetail {
		s = s[:maxDetail]
	}
	return &s
}

func intPtr(n int) *int { return &n }

// emitter — lifecycle 발행기. producer가 nil이면 조용히 no-op(단위 테스트·미배선 환경).
type emitter struct {
	producer *libqueue.Producer
	logger   *slog.Logger
	// sent — 테스트가 들여다보는 훅. 프로덕션에서는 nil.
	sent func(*libqueue.Envelope, lifecycleEvent)
}

// emit — envelope을 만들어 stream:message.lifecycle에 싣는다.
// 발행 실패는 로그만 남긴다: 수명주기 유실보다 발송 자체를 막는 쪽이 더 나쁘다.
func (e *emitter) emit(ctx context.Context, src *libqueue.Envelope, ev lifecycleEvent, at time.Time) {
	if e.sent != nil {
		e.sent(src, ev)
	}
	if e.producer == nil {
		return
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		e.logger.Error("lifecycle 직렬화 실패", "err", err, "message_id", ev.MessageID)
		return
	}
	traceID := src.TraceID
	if traceID == "" {
		traceID = uuid.NewString()
	}
	out := &libqueue.Envelope{
		ID:         uuid.NewString(),
		Type:       lifecycleEventType,
		SchemaVer:  1,
		TenantID:   src.TenantID,
		AppID:      src.AppID,
		OccurredAt: at,
		TraceID:    traceID,
		Payload:    raw,
	}
	if _, err := e.producer.Publish(ctx, libqueue.StreamLifecycle, out); err != nil {
		e.logger.Error("lifecycle 발행 실패", "err", err, "message_id", ev.MessageID, "status", ev.Status)
	}
}
