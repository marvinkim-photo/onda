// Package templatesync — 알림톡 승인 템플릿 동기화 컨슈머.
//
// 계약: packages/queue-schemas/schemas/alimtalk.template.sync.v1.schema.json
// 스트림: stream:alimtalk.template.sync / cg:alimtalk.template.sync
//
// 왜 워커인가: 벤더 API 호출에는 크리덴셜 복호화가 필요하고, 복호화는 발송 워커 런타임 전용이다
// (PRD-04 3장). API는 잡만 싣고 202를 준다.
//
// 왜 필요한가: alimtalk_templates가 비어 있으면 두 가지가 동시에 죽는다.
//   - alimtalk.ValidateSend는 저장된 승인 본문에서 필수 치환자를 도출한다.
//   - 알리고처럼 완성 본문(substitution=rendered)을 요구하는 벤더는 승인 본문과
//     한 글자도 다르면 안 되는 전문을 요구한다.
//
// 즉 템플릿 동기화는 편의가 아니라 발송의 전제다.
package templatesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
	"github.com/ondahq/onda/apps/worker/internal/clock"
	"github.com/ondahq/onda/apps/worker/internal/message"
	libqueue "github.com/ondahq/onda/packages/libqueue-go"
)

const (
	fetchCount    = 16 // 동기화는 저빈도·고비용(벤더 HTTP) — 한 번에 많이 집지 않는다
	fetchBlock    = time.Second
	reclaimIdle   = 2 * time.Minute // 벤더 조회가 느릴 수 있어 lifecycle(30s)보다 길게 잡는다
	reclaimPeriod = 30 * time.Second

	maxVendorAttempts = 3
	vendorBackoffBase = 2 * time.Second
)

var connectorIDRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)

// Payload — alimtalk.template.sync v1.
type Payload struct {
	AppID       string  `json:"app_id"`
	SenderID    string  `json:"sender_id"`
	SenderKey   string  `json:"sender_key"`
	ConnectorID string  `json:"connector_id"`
	RequestedBy *string `json:"requested_by"`
	RequestedAt string  `json:"requested_at"`
}

// ParsePayload — 구조 검증까지. 실패는 재소비해도 같으므로 호출자는 ack 후 버린다.
func ParsePayload(env *libqueue.Envelope) (*Payload, error) {
	var p Payload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return nil, fmt.Errorf("payload 파싱: %w", err)
	}
	if _, err := uuid.Parse(env.TenantID); err != nil {
		return nil, fmt.Errorf("envelope tenant_id UUID 아님: %q", env.TenantID)
	}
	if _, err := uuid.Parse(p.AppID); err != nil {
		return nil, fmt.Errorf("app_id UUID 아님: %q", p.AppID)
	}
	// envelope과 payload의 app_id가 다르면 어느 쪽으로 upsert할지 정할 근거가 없다.
	// 조용히 한쪽을 고르면 남의 앱에 템플릿을 심을 수 있으므로 거절한다.
	if env.AppID != p.AppID {
		return nil, fmt.Errorf("envelope app_id(%s)와 payload app_id(%s) 불일치", env.AppID, p.AppID)
	}
	if _, err := uuid.Parse(p.SenderID); err != nil {
		return nil, fmt.Errorf("sender_id UUID 아님: %q", p.SenderID)
	}
	if p.SenderKey == "" {
		return nil, errors.New("sender_key 누락 — 벤더 조회 인자가 없다")
	}
	if !connectorIDRe.MatchString(p.ConnectorID) {
		return nil, fmt.Errorf("connector_id 형식 오류: %q", p.ConnectorID)
	}
	return &p, nil
}

// Resolver — 커넥터 배선 + 복호화된 크리덴셜. internal/message의 해석기를 그대로 쓴다.
type Resolver interface {
	Resolve(ctx context.Context, tenantID, appID, ch string) (message.Binding, bool, string, error)
}

// Vendors — connector_id → 벤더. *alimtalk.Registry가 만족한다.
type Vendors interface {
	Get(connectorID string) (alimtalk.Vendor, error)
}

// Summary — 동기화 한 건의 결과. 로그 한 줄과 테스트가 같은 값을 본다.
type Summary struct {
	SenderKey string
	Fetched   int
	Upserted  int
	Approved  int
	Missing   int
	// Skipped — 처리하지 않은 사유(설정 미비 등). 비어 있으면 정상 처리.
	Skipped string
}

// Syncer — 잡 한 건의 처리. Consumer(스트림 루프)와 분리해 테스트가 인프라 없이 돈다.
type Syncer struct {
	res    Resolver
	reg    Vendors
	store  Store
	clk    clock.Clock
	logger *slog.Logger
	// sleep — 벤더 재시도 백오프. 테스트가 즉시 반환으로 갈아끼운다.
	sleep func(context.Context, time.Duration) error
}

func NewSyncer(res Resolver, reg Vendors, store Store, clk clock.Clock, logger *slog.Logger) *Syncer {
	return &Syncer{res: res, reg: reg, store: store, clk: clk, logger: logger, sleep: sleepCtx}
}

// Sync — 잡 한 건.
//
// 반환 error는 "나중에 다시 해봐야 한다"는 뜻이고, 호출자는 ack하지 않는다(리클레임 재시도).
// nil은 종결(ack)이다 — 성공뿐 아니라 "설정이 안 돼 있다"·"payload가 불량이다"처럼
// 재소비해도 결과가 같은 경우를 포함한다.
func (s *Syncer) Sync(ctx context.Context, env *libqueue.Envelope) (Summary, error) {
	p, err := ParsePayload(env)
	if err != nil {
		s.logger.Warn("alimtalk.template.sync payload 불량 — skip", "err", err, "msg_id", env.ID)
		return Summary{Skipped: "invalid_payload"}, nil
	}

	b, found, note, err := s.res.Resolve(ctx, env.TenantID, p.AppID, alimtalk.ChannelID)
	if err != nil {
		return Summary{SenderKey: p.SenderKey}, fmt.Errorf("배선 해석: %w", err)
	}
	if !found {
		// API가 발행 전에 배선을 검사하지만, 발행과 소비 사이에 배선이 지워질 수 있다.
		s.logger.Warn("템플릿 동기화 건너뜀 — 커넥터 배선/크리덴셜 없음",
			"reason", note, "app_id", p.AppID, "sender_key", p.SenderKey)
		return Summary{SenderKey: p.SenderKey, Skipped: "unwired"}, nil
	}
	if b.ConnectorID != p.ConnectorID {
		// 배선이 진실이다 — 요청이 못박은 커넥터로 조회하면 지금 발송에 쓰이지 않는
		// 벤더의 템플릿을 캐시하게 된다. 다만 조용히 다르면 안 되므로 남긴다.
		s.logger.Warn("요청 커넥터와 현재 배선이 다르다 — 배선을 따른다",
			"requested", p.ConnectorID, "binding", b.ConnectorID, "app_id", p.AppID)
	}
	vendor, err := s.reg.Get(b.ConnectorID)
	if err != nil {
		s.logger.Warn("템플릿 동기화 건너뜀 — 커넥터 미등록", "err", err, "connector_id", b.ConnectorID)
		return Summary{SenderKey: p.SenderKey, Skipped: "unknown_connector"}, nil
	}

	cred := alimtalk.Credential{ConnectorID: b.ConnectorID, JSON: b.Credential, Config: b.Config}
	tmpls, err := s.list(ctx, vendor, cred, p.SenderKey)
	switch {
	case errors.Is(err, alimtalk.ErrUnsupported):
		s.logger.Warn("템플릿 동기화 건너뜀 — 벤더가 목록 조회를 지원하지 않는다",
			"connector_id", b.ConnectorID)
		return Summary{SenderKey: p.SenderKey, Skipped: "unsupported"}, nil
	case err != nil:
		if permanent(vendor, err) {
			// 크리덴셜 오류·권한 문제는 재시도해도 같다. 큐에 남겨 두면 리클레임이 영원히 돈다.
			s.logger.Error("템플릿 동기화 실패 — 재시도해도 같은 오류",
				"err", err, "connector_id", b.ConnectorID, "sender_key", p.SenderKey)
			return Summary{SenderKey: p.SenderKey, Skipped: "permanent_error"}, nil
		}
		return Summary{SenderKey: p.SenderKey}, fmt.Errorf("벤더 템플릿 조회: %w", err)
	}

	rows, err := BuildRows(tmpls)
	if err != nil {
		// 벤더 응답이 직렬화되지 않는다 — 재시도해도 같다.
		s.logger.Error("템플릿 변환 실패", "err", err, "connector_id", b.ConnectorID)
		return Summary{SenderKey: p.SenderKey, Fetched: len(tmpls), Skipped: "invalid_template"}, nil
	}
	scope := Scope{TenantID: env.TenantID, AppID: p.AppID, SenderID: p.SenderID}
	now := s.clk.Now()
	upserted, err := s.store.Upsert(ctx, scope, rows, now)
	if err != nil {
		return Summary{SenderKey: p.SenderKey, Fetched: len(tmpls)}, err
	}

	sum := Summary{SenderKey: p.SenderKey, Fetched: len(tmpls), Upserted: upserted, Approved: approvedCount(tmpls)}
	// 사라진 템플릿 표시는 목록이 비어 있지 않을 때만 한다. 벤더가 빈 배열을 주는 경우
	// (권한 축소·조회 조건 변경·일시적 이상)를 "전부 사라졌다"로 읽으면 멀쩡한 저니가 통째로 막힌다.
	if len(tmpls) > 0 {
		missing, err := s.store.MarkMissing(ctx, scope, codes(tmpls), now)
		if err != nil {
			return sum, err
		}
		sum.Missing = missing
	}
	s.logger.Info("알림톡 템플릿 동기화 완료",
		"sender", p.SenderKey, "fetched", sum.Fetched, "upserted", sum.Upserted,
		"approved", sum.Approved, "missing", sum.Missing, "connector_id", b.ConnectorID)
	return sum, nil
}

// list — 벤더 조회 + 일시 오류 백오프 재시도. 영구 오류는 즉시 올린다.
func (s *Syncer) list(ctx context.Context, v alimtalk.Vendor, cred alimtalk.Credential, senderKey string) ([]alimtalk.Template, error) {
	var lastErr error
	for attempt := 1; attempt <= maxVendorAttempts; attempt++ {
		tmpls, err := v.ListTemplates(ctx, cred, senderKey)
		if err == nil {
			return tmpls, nil
		}
		lastErr = err
		if errors.Is(err, alimtalk.ErrUnsupported) || permanent(v, err) || ctx.Err() != nil {
			return nil, err
		}
		if attempt == maxVendorAttempts {
			break
		}
		backoff := vendorBackoffBase * time.Duration(attempt)
		s.logger.Warn("템플릿 조회 실패 — 재시도", "err", err, "attempt", attempt, "backoff", backoff)
		if serr := s.sleep(ctx, backoff); serr != nil {
			return nil, serr
		}
	}
	return nil, lastErr
}

// permanent — 재시도가 무의미한 벤더 오류인가. 분류는 벤더만 할 수 있다(HTTP 상태가 아니라
// 본문 결과 코드로 성패를 알리는 딜러사가 있다 — alimtalk.Vendor.Classify 주석).
func permanent(v alimtalk.Vendor, err error) bool {
	switch v.Classify(err) {
	case channel.FailureCredentialAuth, channel.FailurePermanentContent, channel.FailureInvalidTarget:
		return true
	default:
		return false
	}
}

func approvedCount(tmpls []alimtalk.Template) int {
	n := 0
	for _, t := range tmpls {
		if t.Status == alimtalk.TemplateApproved {
			n++
		}
	}
	return n
}

func codes(tmpls []alimtalk.Template) []string {
	out := make([]string, 0, len(tmpls))
	for _, t := range tmpls {
		out = append(out, t.Code)
	}
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Consumer — 스트림 루프. channel 역할과 함께 기동한다.
// 리스→커밋 계약: Sync가 nil을 주면 ACK, error면 미ACK(리클레임 재시도).
type Consumer struct {
	queue       *libqueue.Consumer
	syncer      *Syncer
	clk         clock.Clock
	logger      *slog.Logger
	lastReclaim time.Time
}

func NewConsumer(queue *libqueue.Consumer, syncer *Syncer, clk clock.Clock, logger *slog.Logger) *Consumer {
	return &Consumer{queue: queue, syncer: syncer, clk: clk, logger: logger}
}

func (c *Consumer) Run(ctx context.Context) error {
	if err := c.queue.EnsureGroup(ctx); err != nil {
		return err
	}
	c.logger.Info("알림톡 템플릿 동기화 소비자 시작")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		msgs, err := c.queue.Fetch(ctx, fetchCount, fetchBlock)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.logger.Error("fetch 실패", "err", err)
			if serr := sleepCtx(ctx, time.Second); serr != nil {
				return serr
			}
			continue
		}
		if c.clk.Now().Sub(c.lastReclaim) > reclaimPeriod {
			c.lastReclaim = c.clk.Now()
			if reclaimed, rerr := c.queue.Reclaim(ctx, reclaimIdle, fetchCount); rerr == nil && len(reclaimed) > 0 {
				msgs = append(msgs, reclaimed...)
			}
		}
		for i := range msgs {
			m := &msgs[i]
			if _, serr := c.syncer.Sync(ctx, &m.Envelope); serr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// 미ACK — pending으로 남겨 리클레임이 다시 집는다.
				c.logger.Error("템플릿 동기화 실패 — 재시도 대기", "err", serr, "msg_id", m.Envelope.ID)
				continue
			}
			if aerr := c.queue.Ack(ctx, m.StreamID); aerr != nil {
				c.logger.Error("ack 실패", "err", aerr, "msg_id", m.Envelope.ID)
			}
		}
	}
}
