// Package libqueue는 Redis Streams 큐 접근의 유일한 경로다 (ADR-1, CLAUDE.md 규칙 2).
// TS 측 쌍둥이는 packages/libqueue-ts. 스트림 키·그룹 이름·envelope 구조는
// packages/queue-schemas가 단일 출처이며, 양쪽이 동일해야 한다 (계약 테스트 대상).
// Kafka 이관 시 이 패키지만 교체한다.
package libqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// 스트림 키 — packages/queue-schemas/src/index.ts의 STREAMS와 동일해야 한다.
const (
	StreamIngest       = "stream:ingest"
	StreamEvents       = "stream:events" // 정규화 이벤트 (user_id 해석 후)
	StreamJourneyEntry = "stream:journey.entry"
	StreamJourneyWake  = "stream:journey.wake"
	StreamDispatch     = "stream:dispatch"
	StreamSendPush     = "stream:send.push"
	StreamSendEmail    = "stream:send.email"
	StreamSendMessage  = "stream:send.message"      // 채널 중립 발송 (send.message.v1)
	StreamLifecycle    = "stream:message.lifecycle" // 발송 수명주기 (message.lifecycle.v1)
	StreamFeedback     = "stream:feedback"
	// 알림톡 승인 템플릿 동기화 잡 — API가 발행, channel 역할 워커가 소비.
	StreamAlimtalkTemplateSync = "stream:alimtalk.template.sync"
)

// Consumer group 이름 — queue-schemas의 CONSUMER_GROUPS와 동일해야 한다.
const (
	GroupIngest               = "cg:ingest"
	GroupTriggerMatcher       = "cg:trigger-matcher"
	GroupScheduler            = "cg:scheduler"
	GroupFanout               = "cg:fanout"
	GroupChannel              = "cg:channel"
	GroupChannelEmail         = "cg:channel.email"
	GroupChannelMessage       = "cg:channel.message"
	GroupLifecycle            = "cg:lifecycle"
	GroupFeedback             = "cg:feedback"
	GroupAlimtalkTemplateSync = "cg:alimtalk.template.sync"
)

// envelopeField는 XADD field-value 쌍의 field 이름 (libqueue-ts와 동일).
const envelopeField = "envelope"

// Envelope은 모든 큐 메시지의 공통 봉투다 (DEV-MAIN §5).
type Envelope struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	SchemaVer  int             `json:"schema_ver"`
	TenantID   string          `json:"tenant_id"`
	AppID      string          `json:"app_id"`
	OccurredAt time.Time       `json:"occurred_at"`
	TraceID    string          `json:"trace_id"`
	Payload    json.RawMessage `json:"payload"`
}

// Validate는 envelope 필수 필드를 구조적으로 검사한다.
// TODO(S2): queue-schemas JSON Schema 임베드 기반 완전 검증으로 교체.
func (e *Envelope) Validate() error {
	switch {
	case e.ID == "":
		return errors.New("envelope: id 누락")
	case e.Type == "":
		return errors.New("envelope: type 누락")
	case e.SchemaVer < 1:
		return errors.New("envelope: schema_ver는 1 이상")
	case e.TenantID == "":
		return errors.New("envelope: tenant_id 누락")
	case e.AppID == "":
		return errors.New("envelope: app_id 누락")
	case e.OccurredAt.IsZero():
		return errors.New("envelope: occurred_at 누락")
	case e.TraceID == "":
		return errors.New("envelope: trace_id 누락")
	case len(e.Payload) == 0:
		return errors.New("envelope: payload 누락")
	}
	return nil
}

// Message는 스트림에서 읽은 한 건이다. Ack에는 StreamID를 사용한다.
type Message struct {
	StreamID string // Redis 스트림 엔트리 ID ("1692950400000-0")
	Envelope Envelope
}

// Producer는 스트림에 envelope을 발행한다.
type Producer struct {
	rdb    redis.Cmdable
	maxLen int64
}

// NewProducer를 만든다. maxLen<=0이면 기본 1,000,000 (MAXLEN ~).
func NewProducer(rdb redis.Cmdable, maxLen int64) *Producer {
	if maxLen <= 0 {
		maxLen = 1_000_000
	}
	return &Producer{rdb: rdb, maxLen: maxLen}
}

// Publish는 envelope을 검증 후 XADD한다.
func (p *Producer) Publish(ctx context.Context, stream string, env *Envelope) (string, error) {
	if err := env.Validate(); err != nil {
		return "", err
	}
	body, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("envelope 직렬화: %w", err)
	}
	id, err := p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		MaxLen: p.maxLen,
		Approx: true,
		Values: map[string]any{envelopeField: string(body)},
	}).Result()
	if err != nil {
		return "", fmt.Errorf("xadd %s: %w", stream, err)
	}
	return id, nil
}

// Consumer는 consumer group 기반 소비자다 (at-least-once).
type Consumer struct {
	rdb      redis.Cmdable
	stream   string
	group    string
	consumer string
}

func NewConsumer(rdb redis.Cmdable, stream, group, consumerName string) *Consumer {
	return &Consumer{rdb: rdb, stream: stream, group: group, consumer: consumerName}
}

// EnsureGroup은 그룹을 생성한다(스트림 없으면 MKSTREAM). 이미 있으면 무시.
func (c *Consumer) EnsureGroup(ctx context.Context) error {
	err := c.rdb.XGroupCreateMkStream(ctx, c.stream, c.group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("xgroup create %s/%s: %w", c.stream, c.group, err)
	}
	return nil
}

// Fetch는 신규 메시지를 최대 count건, block 시간만큼 대기하며 읽는다.
// block <= 0 이면 대기 없이 즉시 반환한다 (무한 블로킹 금지 — BLOCK 0 함정 방지).
// 파싱 불가 엔트리는 즉시 Ack 후 건너뛴다(포이즌 필 방지) — 원본은 raw_ingestions에 있다.
func (c *Consumer) Fetch(ctx context.Context, count int64, block time.Duration) ([]Message, error) {
	if block <= 0 {
		block = -1 // go-redis: 음수 = BLOCK 옵션 미전송(논블로킹), 0 = 무한 대기
	}
	res, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    c.group,
		Consumer: c.consumer,
		Streams:  []string{c.stream, ">"},
		Count:    count,
		Block:    block,
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) { // block 타임아웃 — 메시지 없음
			return nil, nil
		}
		return nil, fmt.Errorf("xreadgroup %s: %w", c.stream, err)
	}
	return c.parseStreams(ctx, res), nil
}

// Reclaim은 idle이 minIdle을 넘긴 pending 메시지를 회수한다 (크래시 소비자 복구).
// DEV-sub-01: XAUTOCLAIM, idle 30s.
func (c *Consumer) Reclaim(ctx context.Context, minIdle time.Duration, count int64) ([]Message, error) {
	entries, _, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   c.stream,
		Group:    c.group,
		Consumer: c.consumer,
		MinIdle:  minIdle,
		Start:    "0-0",
		Count:    count,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("xautoclaim %s: %w", c.stream, err)
	}
	return c.parseEntries(ctx, entries), nil
}

// Ack는 처리 완료를 보고한다.
func (c *Consumer) Ack(ctx context.Context, streamIDs ...string) error {
	if len(streamIDs) == 0 {
		return nil
	}
	if err := c.rdb.XAck(ctx, c.stream, c.group, streamIDs...).Err(); err != nil {
		return fmt.Errorf("xack %s: %w", c.stream, err)
	}
	return nil
}

func (c *Consumer) parseStreams(ctx context.Context, res []redis.XStream) []Message {
	var msgs []Message
	for _, s := range res {
		msgs = append(msgs, c.parseEntries(ctx, s.Messages)...)
	}
	return msgs
}

func (c *Consumer) parseEntries(ctx context.Context, entries []redis.XMessage) []Message {
	msgs := make([]Message, 0, len(entries))
	for _, m := range entries {
		raw, ok := m.Values[envelopeField].(string)
		if !ok {
			_ = c.Ack(ctx, m.ID) // 포이즌 필: 형식 불명 엔트리는 버린다
			continue
		}
		var env Envelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			_ = c.Ack(ctx, m.ID)
			continue
		}
		msgs = append(msgs, Message{StreamID: m.ID, Envelope: env})
	}
	return msgs
}
