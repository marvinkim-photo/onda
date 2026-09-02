package channel

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/redis/go-redis/v9"

	"github.com/ondahq/onda/apps/worker/internal/clock"
	"github.com/ondahq/onda/apps/worker/internal/metrics"
	libqueue "github.com/ondahq/onda/packages/libqueue-go"
)

// sendloop — 발송 워커의 공통 골격(멱등·리스·백오프·DLQ·message_log 플러시).
//
// Worker(push)와 EmailWorker가 이 프로토콜을 250줄씩 그대로 중복하고 있다. 세 번째 복사를
// 만들지 않으려고 여기로 추출했다. 채널별 차이는 SendHandler로 받는다.
//
// 기존 두 워커는 이번에 이관하지 않는다 — 발송 신뢰성을 건드리는 위험을 피하고,
// 세 번째 구현이 참조 구현으로 자리잡은 뒤 옮긴다.

// SendOutcome — 한 건의 처리 결과. Row·OnTerminal·DLQ가 공유한다.
type SendOutcome struct {
	MessageID     string
	Status        string // sent | failed | duplicate
	FailureClass  string
	FailureDetail string
	ProviderID    string
	Attempts      int
	At            time.Time
}

// SendHandler — 채널별로 다른 부분만 구현한다.
type SendHandler[J any] interface {
	// KeyPrefix — Redis 멱등 키 네임스페이스. 채널마다 달라야 한다 (예: "send:message").
	KeyPrefix() string
	// Parse — envelope payload를 job으로. ok=false면 재처리해도 무의미하므로 ACK 후 버린다.
	Parse(env *libqueue.Envelope) (job J, idemKey, messageID string, ok bool)
	// Resolve — 발송 전 준비(크리덴셜 복호화 등). found=false면 credential_missing으로 종결한다.
	Resolve(ctx context.Context, env *libqueue.Envelope, job J) (creds Credentials, found bool, err error)
	// Send — 실제 발송. 반환 providerID는 공급자 식별자(콜백 조인 키).
	Send(ctx context.Context, env *libqueue.Envelope, job J, creds Credentials) (providerID string, err error)
	// Classify — 오류 → 재시도 정책 근거.
	Classify(err error) FailureClass
	// OnTerminal — 종결 시 부수효과. 토큰 invalid 반영, 크리덴셜 error 전환,
	// message.lifecycle 발행 등이 여기로 들어온다. duplicate 재기록에도 호출되지 않는다.
	OnTerminal(ctx context.Context, env *libqueue.Envelope, job J, out SendOutcome)
	// Row — message_log 행 16개 값. nil이면 기록하지 않는다.
	Row(env *libqueue.Envelope, job J, out SendOutcome) []any
	// DLQ — 재시도 소진 시 적재. 미구현이면 no-op이어도 된다.
	DLQ(ctx context.Context, env *libqueue.Envelope, job J, out SendOutcome)
}

// SendLoop — SendHandler를 감싸 스트림을 소비한다.
type SendLoop[J any] struct {
	name        string
	handler     SendHandler[J]
	queue       *libqueue.Consumer
	rdb         redis.Cmdable
	ch          driver.Conn
	clk         clock.Clock
	logger      *slog.Logger
	lastReclaim time.Time
}

func NewSendLoop[J any](name string, h SendHandler[J], queue *libqueue.Consumer, rdb redis.Cmdable,
	ch driver.Conn, clk clock.Clock, logger *slog.Logger) *SendLoop[J] {
	return &SendLoop[J]{name: name, handler: h, queue: queue, rdb: rdb, ch: ch, clk: clk, logger: logger}
}

func (l *SendLoop[J]) Run(ctx context.Context) error {
	if err := l.queue.EnsureGroup(ctx); err != nil {
		return err
	}
	l.logger.Info(l.name + " 워커 시작")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		msgs, err := l.queue.Fetch(ctx, sendFetch, sendBlock)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			l.logger.Error("fetch 실패", "err", err)
			time.Sleep(time.Second)
			continue
		}
		if l.clk.Now().Sub(l.lastReclaim) > reclaimPeriod {
			l.lastReclaim = l.clk.Now()
			if reclaimed, rerr := l.queue.Reclaim(ctx, sendReclaim, sendFetch); rerr == nil && len(reclaimed) > 0 {
				msgs = append(msgs, reclaimed...)
			}
		}
		if len(msgs) == 0 {
			continue
		}
		rows := make([][]any, 0, len(msgs))
		ackIDs := make([]string, 0, len(msgs))
		for i := range msgs {
			row, retry := l.handleOne(ctx, &msgs[i])
			if row != nil {
				rows = append(rows, row)
			}
			if !retry {
				ackIDs = append(ackIDs, msgs[i].StreamID)
			}
		}
		// 로그 적재 실패 시 ACK하지 않는다 — 재전달돼도 멱등 상태가 sent를 보존하므로
		// 중복 발송 없이 로그만 다시 기록된다.
		if err := l.flushLog(ctx, rows); err != nil {
			l.logger.Error("message_log 적재 실패 — 재시도", "err", err, "worker", l.name)
			time.Sleep(time.Second)
			continue
		}
		if err := l.queue.Ack(ctx, ackIDs...); err != nil {
			l.logger.Error("ack 실패", "err", err)
		}
	}
}

// handleOne — 한 건 처리. retry=true면 ACK하지 않아 reclaim이 재전달한다.
// 멱등: 임시 리스로 선점 → 전송/영구실패 시 7d 커밋(재전송 차단), 일시실패 시 리스 해제(재시도 허용).
func (l *SendLoop[J]) handleOne(ctx context.Context, m *libqueue.Message) ([]any, bool) {
	env := &m.Envelope
	job, idemK, messageID, ok := l.handler.Parse(env)
	if !ok {
		l.logger.Warn(l.name+" payload 불량 — skip", "msg_id", env.ID)
		return nil, false
	}
	now := l.clk.Now()
	prefix := l.handler.KeyPrefix()
	idemKey := fmt.Sprintf("%s:idem:%s:%s", prefix, env.TenantID, idemK)
	attemptsKey := fmt.Sprintf("%s:attempts:%s:%s", prefix, env.TenantID, idemK)
	retryAtKey := fmt.Sprintf("%s:retryat:%s:%s", prefix, env.TenantID, idemK)

	out := func(status, class, detail, providerID string, attempts int) SendOutcome {
		return SendOutcome{MessageID: messageID, Status: status, FailureClass: class,
			FailureDetail: detail, ProviderID: providerID, Attempts: attempts, At: now}
	}
	terminal := func(o SendOutcome) ([]any, bool) {
		l.handler.OnTerminal(ctx, env, job, o)
		return l.handler.Row(env, job, o), false
	}

	// 0) 백오프 대기 중이면 리스 없이 미룬다 → reclaim이 나중에 재전달.
	if ts, err := l.rdb.Get(ctx, retryAtKey).Int64(); err == nil && now.Unix() < ts {
		return nil, true
	}

	// 1) 멱등 선점 (processing 리스).
	acquired, err := l.rdb.SetNX(ctx, idemKey, statusProcessing, idemLeaseTTL).Result()
	if err != nil {
		l.logger.Error("멱등 선점 실패", "err", err)
		return nil, true
	}
	if !acquired {
		// 이미 종결됐거나 처리 중. 결과를 보존해 로그만 다시 기록한다(재전송 없음).
		val, _ := l.rdb.Get(ctx, idemKey).Result()
		switch {
		case strings.HasPrefix(val, statusSent+"|"):
			providerID := strings.TrimPrefix(val, statusSent+"|")
			metrics.ChannelSends.WithLabelValues("duplicate").Inc()
			return l.handler.Row(env, job, out("sent", "", "provider_id="+providerID, providerID, 0)), false
		case strings.HasPrefix(val, statusFailed+"|"):
			return l.handler.Row(env, job, out("failed", strings.TrimPrefix(val, statusFailed+"|"), "", "", 0)), false
		default:
			metrics.ChannelSends.WithLabelValues("duplicate").Inc()
			return l.handler.Row(env, job, out("duplicate", "", "", "", 0)), false
		}
	}

	commitFailed := func(class string) {
		l.rdb.Set(ctx, idemKey, statusFailed+"|"+class, idemCommitTTL)
		l.rdb.Del(ctx, attemptsKey, retryAtKey)
	}
	// retryFail — 상한 내면 백오프 후 재시도(리스 해제), 초과면 DLQ 적재 후 종결.
	retryFail := func(class, detail string, retryAfter time.Duration) ([]any, bool) {
		n, ierr := l.rdb.Incr(ctx, attemptsKey).Result()
		if ierr == nil {
			l.rdb.Expire(ctx, attemptsKey, idemCommitTTL)
		}
		if ierr != nil || n >= maxSendAttempts {
			o := out("failed", class+"_exhausted", detail, "", int(n))
			l.handler.DLQ(ctx, env, job, o)
			l.rdb.Set(ctx, idemKey, statusFailed+"|"+class+"_exhausted", idemCommitTTL)
			l.rdb.Del(ctx, attemptsKey, retryAtKey)
			metrics.ChannelSends.WithLabelValues("failed").Inc()
			return terminal(o)
		}
		delay := retryAfter // 429 Retry-After 우선
		if delay <= 0 {
			delay = backoff(int(n))
		}
		l.rdb.Set(ctx, retryAtKey, now.Add(delay).Unix(), idemCommitTTL)
		l.rdb.Del(ctx, idemKey) // 리스 해제
		return nil, true
	}

	// 2) 크리덴셜 등 준비
	creds, found, err := l.handler.Resolve(ctx, env, job)
	if err != nil {
		return retryFail("retryable", "크리덴셜 조회 오류: "+err.Error(), 0)
	}
	if !found {
		commitFailed("credential_missing")
		metrics.ChannelSends.WithLabelValues("failed").Inc()
		return terminal(out("failed", "credential_missing", "크리덴셜 미등록/미검증", "", 0))
	}

	// 3) 전송
	providerID, sendErr := l.handler.Send(ctx, env, job, creds)
	if sendErr == nil {
		l.rdb.Set(ctx, idemKey, statusSent+"|"+providerID, idemCommitTTL)
		l.rdb.Del(ctx, attemptsKey, retryAtKey)
		metrics.ChannelSends.WithLabelValues("sent").Inc()
		return terminal(out("sent", "", "", providerID, 0))
	}

	// 4) 실패 분류. 일시 실패만 재시도하고 나머지는 종결한다.
	// 주의(결과 불명): 요청 후 응답 전 네트워크 오류는 공급자가 이미 수신했을 수 있으나
	// retryable로 재시도한다 → at-least-once. 공급자 멱등키가 있으면 벤더가 흡수한다.
	class := l.handler.Classify(sendErr)
	if class == FailureRetryable || class == FailureRateLimited {
		return retryFail(class.String(), sendErr.Error(), RetryAfterOf(sendErr))
	}
	commitFailed(class.String())
	metrics.ChannelSends.WithLabelValues("failed").Inc()
	return terminal(out("failed", class.String(), sendErr.Error(), "", 0))
}

func (l *SendLoop[J]) flushLog(ctx context.Context, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	batch, err := l.ch.PrepareBatch(ctx, `
		INSERT INTO message_log (tenant_id, app_id, message_id, idempotency_key,
			journey_id, journey_version, node_index, campaign_ref,
			user_id, device_id, channel, status, failure_class, failure_detail, sent_at,
			provider_message_id)`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := batch.Append(r...); err != nil {
			return err
		}
	}
	return batch.Send()
}
