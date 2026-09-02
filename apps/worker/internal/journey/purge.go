package journey

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const purgeInterval = 30 * time.Second

// FK 의존성 역순(자식→부모) — 모두 tenant_id로 삭제. journey_versions는 tenant_id 컬럼이 없어 서브쿼리.
// journey_node_executions·event_receipts·event_customer_cursors는 각각 journey_states·users를
// 참조하므로 반드시 그 부모보다 먼저 삭제한다 (재검증 R-11 FK 누락 수정).
var pgPurgeStmts = []string{
	`DELETE FROM member_backup_codes WHERE tenant_id = $1`,
	`DELETE FROM sessions WHERE tenant_id = $1`,
	`DELETE FROM audit_logs WHERE tenant_id = $1`,
	`DELETE FROM journey_node_executions WHERE tenant_id = $1`,
	`DELETE FROM journey_outbox WHERE tenant_id = $1`,
	`DELETE FROM journey_states WHERE tenant_id = $1`,
	`DELETE FROM journey_versions WHERE journey_id IN (SELECT id FROM journeys WHERE tenant_id = $1)`,
	`DELETE FROM event_receipts WHERE tenant_id = $1`,
	`DELETE FROM event_customer_cursors WHERE tenant_id = $1`,
	`DELETE FROM user_merges WHERE tenant_id = $1`,
	`DELETE FROM devices WHERE tenant_id = $1`,
	`DELETE FROM segments WHERE tenant_id = $1`,
	`DELETE FROM attribute_registry WHERE tenant_id = $1`,
	// 알림톡 설정. channel_connectors·alimtalk_senders·alimtalk_templates는 tenants·apps에 FK를 걸므로
	// 여기서 지우지 않으면 아래 apps/tenants 삭제가 FK 위반으로 실패한다(파기 자체가 깨진다).
	// pending_receipts는 FK가 없지만 tenant_id를 담은 폴러 임시 행이라 함께 지운다.
	`DELETE FROM alimtalk_templates WHERE tenant_id = $1`,
	`DELETE FROM alimtalk_senders WHERE tenant_id = $1`,
	`DELETE FROM channel_connectors WHERE tenant_id = $1`,
	`DELETE FROM pending_receipts WHERE tenant_id = $1`,
	`DELETE FROM credentials WHERE tenant_id = $1`,
	`DELETE FROM journeys WHERE tenant_id = $1`,
	`DELETE FROM users WHERE tenant_id = $1`,
	`DELETE FROM api_keys WHERE tenant_id = $1`,
	`DELETE FROM apps WHERE tenant_id = $1`,
	`DELETE FROM members WHERE tenant_id = $1`,
	`DELETE FROM tenants WHERE id = $1`,
}

var chPurgeTables = []string{
	"events", "message_log", "attr_changes", "ingestion_errors",
	"raw_ingestions", "profiles_mirror", "campaign_audiences",
	"usage_sends_daily", "usage_active_users_daily",
}

// RunMaintenance는 유예 만료 테넌트 파기 + CH 정리 재시도 루프다 (T-10).
func (s *Scheduler) RunMaintenance(ctx context.Context) error {
	s.logger.Info("유지보수(테넌트 파기) 루프 시작")
	ticker := time.NewTicker(purgeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.purgeExpiredTenants(ctx); err != nil && ctx.Err() == nil {
				s.logger.Error("테넌트 파기 스캔 실패", "err", err)
			}
			// PG 파기됐으나 CH 정리가 남은(장애 등) 테넌트 재시도 — 완료 추적.
			if err := s.retryPendingCHPurges(ctx); err != nil && ctx.Err() == nil {
				s.logger.Error("CH 파기 재시도 스캔 실패", "err", err)
			}
		}
	}
}

// purgeExpiredTenants는 purge_after가 지난 후보를 찾아 하나씩 파기한다(행 잠금으로 복구 경합 방지).
func (s *Scheduler) purgeExpiredTenants(ctx context.Context) error {
	rows, err := s.pg.Query(ctx,
		`SELECT id FROM tenants WHERE purge_after IS NOT NULL AND purge_after <= $1`, s.clk.Now())
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()

	for _, id := range ids {
		purged, err := s.purgeTenant(ctx, id)
		if err != nil {
			s.logger.Error("테넌트 파기 실패 — 다음 틱 재시도", "tenant", id, "err", err)
			continue
		}
		if purged {
			s.logger.Warn("테넌트 파기 완료", "tenant", id)
		}
	}
	return nil
}

// purgeTenant는 한 테넌트를 파기한다. 반환 purged=false면 복구/유예 등으로 파기하지 않음.
// 복구 경합 방지: 행을 FOR UPDATE로 잠그고 purge_after를 재확인 — restore로 NULL/미래가 됐으면 스킵.
func (s *Scheduler) purgeTenant(ctx context.Context, tenantID string) (bool, error) {
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var purgeAfter *time.Time
	err = tx.QueryRow(ctx,
		`SELECT purge_after FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).Scan(&purgeAfter)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil // 이미 파기됨
		}
		return false, err
	}
	// restore가 먼저 커밋됐거나 아직 유예 중이면 파기하지 않는다 (복구 우선).
	if purgeAfter == nil || purgeAfter.After(s.clk.Now()) {
		return false, nil
	}

	for _, q := range pgPurgeStmts {
		if _, err := tx.Exec(ctx, q, tenantID); err != nil {
			return false, fmt.Errorf("purge %q: %w", q, err)
		}
	}
	// PG 파기와 원자적으로 추적 레코드 기록 → CH 정리 재시도의 근거.
	if _, err := tx.Exec(ctx,
		`INSERT INTO tenant_purges (tenant_id) VALUES ($1) ON CONFLICT (tenant_id) DO NOTHING`,
		tenantID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}

	// CH 정리 시도 — 실패해도 PG 파기는 확정. tenant_purges로 재시도 추적.
	s.cleanupCH(ctx, tenantID)
	return true, nil
}

// retryPendingCHPurges는 PG 파기됐으나 CH 정리가 미완인 테넌트를 재시도한다.
func (s *Scheduler) retryPendingCHPurges(ctx context.Context) error {
	rows, err := s.pg.Query(ctx,
		`SELECT tenant_id FROM tenant_purges WHERE ch_purged = false ORDER BY pg_purged_at LIMIT 100`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		s.cleanupCH(ctx, id)
	}
	return nil
}

// cleanupCH는 CH의 테넌트 데이터를 ALTER DELETE로 제거(발행)하고 완료 여부를 tenant_purges에 추적한다.
// 전 테이블 발행 성공 시 ch_purged=true. 하나라도 실패면 시도 카운트·오류 기록 후 다음 틱 재시도.
func (s *Scheduler) cleanupCH(ctx context.Context, tenantID string) {
	var firstErr error
	for _, t := range chPurgeTables {
		q := "ALTER TABLE onda." + t + " DELETE WHERE tenant_id = ?"
		if err := s.ch.Exec(ctx, q, tenantID); err != nil {
			s.logger.Error("CH 파기 mutation 실패", "table", t, "tenant", tenantID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr == nil {
		if _, err := s.pg.Exec(ctx,
			`UPDATE tenant_purges SET ch_purged = true, ch_purged_at = now(),
			        ch_attempts = ch_attempts + 1, ch_last_error = NULL WHERE tenant_id = $1`,
			tenantID); err != nil {
			s.logger.Error("tenant_purges 완료 기록 실패", "tenant", tenantID, "err", err)
		}
		return
	}
	if _, err := s.pg.Exec(ctx,
		`UPDATE tenant_purges SET ch_attempts = ch_attempts + 1, ch_last_error = $2 WHERE tenant_id = $1`,
		tenantID, firstErr.Error()); err != nil {
		s.logger.Error("tenant_purges 실패 기록 실패", "tenant", tenantID, "err", err)
	}
}
