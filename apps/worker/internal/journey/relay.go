package journey

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	libqueue "github.com/ondahq/onda/packages/libqueue-go"
)

// RunRelay는 outbox 릴레이다. 미발행 outbox 행을 send.push로 발행하고 published 마킹한다.
// 전이 트랜잭션이 커밋된 행만 보이므로, 크래시 지점과 무관하게 멱등 키로 수렴한다 (4.3).
func (s *Scheduler) RunRelay(ctx context.Context) error {
	s.logger.Info("outbox 릴레이 시작")
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.relayOnce(ctx); err != nil && ctx.Err() == nil {
				s.logger.Error("릴레이 실패", "err", err)
			}
		}
	}
}

func (s *Scheduler) relayOnce(ctx context.Context) error {
	rows, err := s.pg.Query(ctx, `
		SELECT id, tenant_id, app_id, stream, payload FROM journey_outbox
		 WHERE published_at IS NULL ORDER BY id LIMIT $1`, relayBatch)
	if err != nil {
		return err
	}
	type row struct {
		id                      int64
		tenantID, appID, stream string
		payload                 []byte
	}
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.tenantID, &r.appID, &r.stream, &r.payload); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, r := range pending {
		kind, err := outboxType(r.stream)
		if err != nil {
			return err
		}
		env := &libqueue.Envelope{
			ID:         uuidString(),
			Type:       kind,
			SchemaVer:  1,
			TenantID:   r.tenantID,
			AppID:      r.appID,
			OccurredAt: s.clk.Now(),
			TraceID:    uuidString(),
			Payload:    json.RawMessage(r.payload),
		}
		if _, err := s.producer.Publish(ctx, r.stream, env); err != nil {
			// 발행 실패 — published 마킹 안 함, 다음 틱 재시도 (send.push 멱등 방어)
			return err
		}
		if _, err := s.pg.Exec(ctx,
			`UPDATE journey_outbox SET published_at = $2 WHERE id = $1`, r.id, s.clk.Now()); err != nil {
			return err
		}
	}
	return nil
}

func outboxType(stream string) (string, error) {
	switch stream {
	case libqueue.StreamIngest:
		return "ingest.batch", nil
	case libqueue.StreamEvents:
		return "event.normalized", nil
	case libqueue.StreamJourneyEntry:
		return "journey.enter", nil
	case libqueue.StreamSendPush:
		return "send.push", nil
	case libqueue.StreamSendEmail:
		return "send.email", nil
	case libqueue.StreamSendMessage:
		return "send.message", nil
	default:
		return "", fmt.Errorf("unsupported outbox stream %q", stream)
	}
}

// RunReaper는 claimed 상태가 claimReap을 넘기면 회수한다 (죽은 워커 복구 — DEV-sub-03).
func (s *Scheduler) RunReaper(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			n, err := s.reapOnce(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				s.logger.Error("리퍼 실패", "err", err)
				continue
			}
			if n > 0 {
				s.logger.Info("claimed 상태 회수", "count", n)
			}
		}
	}
}

func (s *Scheduler) reapOnce(ctx context.Context) (int64, error) {
	cutoff := s.clk.Now().Add(-claimReap)
	tag, err := s.pg.Exec(ctx, `UPDATE journey_states SET status='waiting',claimed_by=NULL,
		claimed_at=NULL,claim_token=NULL,updated_at=$2 WHERE status='claimed' AND claimed_at<$1`, cutoff, s.clk.Now())
	return tag.RowsAffected(), err
}

func uuidString() string {
	return uuid.NewString()
}
