package journey

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ondahq/onda/apps/worker/internal/policy"
)

// RunTick은 상태머신 틱 루프다. next_wake_at 도래한 상태를 클레임해 노드를 실행한다.
func (s *Scheduler) RunTick(ctx context.Context) error {
	s.logger.Info("scheduler 틱 시작")
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.tickOnce(ctx); err != nil && ctx.Err() == nil {
				s.logger.Error("틱 처리 실패", "err", err)
			}
		}
	}
}

// claimedState — 클레임한 상태 스냅샷
type claimedState struct {
	id          string
	tenantID    string
	appID       string
	journeyID   string
	version     int
	userID      string
	currentNode int
	claimToken  string
	entrySeq    *int64
	nextWake    *time.Time // 예정 기상 시각 — 재개 후 오래된 발송 skip 판정에 사용 (R-06)
}

// staleSendThreshold — 예정보다 이만큼 넘게 늦은 marketing 발송은 skip (재개 등, PRD-03 9장 Q1).
const staleSendThreshold = 24 * time.Hour

func (s *Scheduler) tickOnce(ctx context.Context) error {
	claimed, err := s.claimDue(ctx)
	if err != nil {
		return err
	}
	for _, c := range claimed {
		if err := s.executeNode(ctx, &c); err != nil {
			s.logger.Error("노드 실행 실패", "state", c.id, "err", err)
			s.failClaim(ctx, &c, err)
		}
	}
	return nil
}

func (s *Scheduler) claimDue(ctx context.Context) ([]claimedState, error) {
	// 배치 클레임: waiting/active 중 기상 도래분을 claimed로 선점 (FOR UPDATE SKIP LOCKED)
	now := s.clk.Now()
	rows, err := s.pg.Query(ctx, `
		UPDATE journey_states SET status = 'claimed', claimed_by = $1, claimed_at = $2,
		       claim_token = gen_random_uuid(), updated_at = $2
		 WHERE id IN (
		   SELECT id FROM journey_states
		    WHERE status IN ('active', 'waiting')
		      AND (next_wake_at IS NULL OR next_wake_at <= $2)
		      -- 부모 저니가 active일 때만 진행 — paused/archived면 이미 진입한 고객도 동결 (재검증 E)
		      AND journey_id IN (SELECT id FROM journeys WHERE status = 'active')
		    ORDER BY next_wake_at NULLS FIRST
		    LIMIT $3
		    FOR UPDATE SKIP LOCKED
		 )
		 RETURNING id, tenant_id, app_id, journey_id, journey_version, user_id, current_node, claim_token::text, entry_seq, next_wake_at`,
		s.consumer, now, claimBatch)
	if err != nil {
		return nil, fmt.Errorf("클레임: %w", err)
	}
	var claimed []claimedState
	for rows.Next() {
		var c claimedState
		if err := rows.Scan(&c.id, &c.tenantID, &c.appID, &c.journeyID, &c.version, &c.userID, &c.currentNode, &c.claimToken, &c.entrySeq, &c.nextWake); err != nil {
			rows.Close()
			return nil, err
		}
		claimed = append(claimed, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return claimed, nil
}

// executeNode는 한 상태의 현재 노드를 실행하고 전이를 커밋한다 (전이+outbox 원자성).
func (s *Scheduler) executeNode(ctx context.Context, c *claimedState) error {
	def, err := s.loadDefinition(ctx, c.journeyID, c.version)
	if err != nil {
		return err
	}
	tx, sequence, now, err := s.lockClaim(ctx, c, def)
	if err != nil {
		return err
	}
	if tx == nil {
		return nil
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if c.currentNode < 0 {
		return fmt.Errorf("negative current_node")
	}
	if exited, err := s.applyPendingConversion(ctx, tx, c, def, now); err != nil || exited {
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if def.SchemaVersion == 2 {
		return s.executeDAG(ctx, tx, c, def, sequence, now)
	}
	if c.currentNode >= len(def.Nodes) {
		if err := s.moveState(ctx, tx, c, c.currentNode, "completed", nil, now); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	node := def.Nodes[c.currentNode]

	switch node.Type {
	case "delay":
		// 대기: 다음 노드로 전이 + 기상 시각 설정
		wake := s.clk.Now().Add(time.Duration(node.DurationSeconds) * time.Second)
		if err := s.moveState(ctx, tx, c, c.currentNode+1, "waiting", &wake, now); err != nil {
			return err
		}

	case "message":
		// 정책 검사: quiet hours가 delay면 전이하지 않고 다음 open까지 대기 (노드 유지)
		pol, err := s.loadAppPolicy(ctx, tx, c.tenantID, c.appID)
		if err != nil {
			return err
		}
		cat := policy.Category(def.Settings.Category)
		// R-06 재개 정책: 예정보다 24h+ 늦은 marketing 발송은 skip(오래된 알림 방지, PRD-03 9장 Q1).
		// pause 중 경과한 delay가 재개 시 몰려 발송되는 것을 차단. transactional은 우회.
		if cat != policy.Transactional && c.nextWake != nil && s.clk.Now().Sub(*c.nextWake) > staleSendThreshold {
			s.logSkip(ctx, c, def, "skipped_stale")
			nextStatus := "active"
			if c.currentNode+1 >= len(def.Nodes) {
				nextStatus = "completed"
			}
			if err := s.moveState(ctx, tx, c, c.currentNode+1, nextStatus, nil, now); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		qd, err := policy.EvaluateQuietHours(cat, pol.quietHours, pol.tz, s.clk.Now())
		if err != nil {
			return err
		}
		if qd.Action == policy.ActionDelay {
			// 발송 보류 — 노드 유지, 다음 허용 시각에 재실행 (PRD-03 6.1 delay_until_open)
			if err := s.moveState(ctx, tx, c, c.currentNode, "waiting", &qd.DelayUntil, now); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		// send 또는 skip(quiet_hours skip 정책) — 어느 쪽이든 노드는 전진
		if qd.Action == policy.ActionSend {
			if _, err := s.enqueueSends(ctx, tx, c, def, node, pol); err != nil {
				return err
			}
		} else {
			s.logSkip(ctx, c, def, "skipped_quiet_hours")
		}
		nextStatus := "active"
		if c.currentNode+1 >= len(def.Nodes) {
			nextStatus = "completed"
		}
		if err := s.moveState(ctx, tx, c, c.currentNode+1, nextStatus, nil, now); err != nil {
			return err
		}

	default:
		return fmt.Errorf("알 수 없는 노드 타입: %s", node.Type)
	}

	return tx.Commit(ctx)
}

// enqueueSends는 유저의 도달 가능 디바이스마다 send.push outbox 행을 기록한다.
// 도달성·정책 검사(카테고리 반영)는 메시지 노드 실행 시점 (PRD-03 3.1, 6장).
func (s *Scheduler) enqueueSends(ctx context.Context, tx pgx.Tx, c *claimedState, def *Definition, node Node, pol *appPolicy) (string, error) {
	if node.Push == nil && node.Email == nil && node.Alimtalk == nil {
		return "", fmt.Errorf("message node has no push/email/alimtalk content")
	}
	cat := policy.Category(def.Settings.Category)
	marketing := cat != policy.Transactional

	// 렌더용 속성 + 구독 상태 조회
	var stdAttrs, customAttrs []byte
	var subscriptions []byte
	err := tx.QueryRow(ctx,
		`SELECT std_attrs, custom_attrs, subscriptions FROM users WHERE tenant_id=$1 AND app_id=$2 AND id=$3 AND status='active'`, c.tenantID, c.appID, c.userID).
		Scan(&stdAttrs, &customAttrs, &subscriptions)
	if err != nil {
		return "", fmt.Errorf("유저 조회: %w", err)
	}
	var sub map[string]string
	_ = json.Unmarshal(subscriptions, &sub)

	// 이메일 노드는 별도 경로 (디바이스 무관, std_attrs.email 대상)
	if node.Email != nil {
		return s.enqueueEmail(ctx, tx, c, def, node, pol, cat, marketing, sub, mergeAttrs(stdAttrs, customAttrs))
	}
	// 알림톡도 디바이스 무관(std_attrs.phone 대상). category는 저니 설정이 아니라 템플릿 유형이 정하므로
	// cat/marketing을 넘기지 않고 enqueueAlimtalk이 직접 도출한다.
	if node.Alimtalk != nil {
		return s.enqueueAlimtalk(ctx, tx, c, def, node, pol, sub, mergeAttrs(stdAttrs, customAttrs))
	}

	// marketing이면 push opt-in 필수 (transactional은 우회)
	if marketing {
		if sub["push"] != "opted_in" {
			s.logSkip(ctx, c, def, "skipped_unreachable") // opt-out
			return "skipped_unreachable", nil
		}
	}
	// frequency cap (유저당 24h N건, transactional 우회) — 원자 검사+증가
	allowed, err := s.freqCap.Allow(ctx, cat, pol.freqCap, c.appID, c.userID)
	if err != nil {
		return "", err
	}
	if !allowed {
		s.logSkip(ctx, c, def, "skipped_cap")
		return "skipped_cap", nil
	}
	attrs := mergeAttrs(stdAttrs, customAttrs)
	title := Render(node.Push.Title, attrs)
	body := Render(node.Push.Body, attrs)
	deepLink := Render(node.Push.DeepLink, attrs) // 공통 계약(R-01): deep_link를 발송에 연결

	rows, err := tx.Query(ctx, `
		SELECT id, push_token, platform FROM devices
		 WHERE tenant_id=$1 AND app_id=$2 AND user_id=$3 AND push_token IS NOT NULL
		   AND token_status = 'active' AND os_permission = 'granted'`, c.tenantID, c.appID, c.userID)
	if err != nil {
		return "", err
	}
	type dev struct{ id, token, platform string }
	var devices []dev
	for rows.Next() {
		var d dev
		if err := rows.Scan(&d.id, &d.token, &d.platform); err != nil {
			rows.Close()
			return "", err
		}
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()

	category := "marketing"
	if !marketing {
		category = "transactional"
	}
	for _, d := range devices {
		idemKey := sendKey(c, def, d.id)
		payload := map[string]any{
			"idempotency_key": idemKey,
			"message_id":      uuidString(), // 안정 발송 ID — message_log·SDK 도달/오픈 연결 (재검증 F)
			"user_id":         c.userID,
			"device_id":       d.id,
			"push_token":      d.token,
			"platform":        d.platform,
			"content":         map[string]any{"push": pushContent(title, body, deepLink)},
			"category":        category,
			"journey_id":      c.journeyID,
			"journey_version": c.version,
			"node_index":      c.currentNode,
		}
		payloadJSON, _ := json.Marshal(payload)
		if _, err := tx.Exec(ctx, `
			INSERT INTO journey_outbox (tenant_id, app_id, stream, idempotency_key, payload)
			VALUES ($1, $2, 'stream:send.push', $3, $4) ON CONFLICT DO NOTHING`,
			c.tenantID, c.appID, idemKey, payloadJSON); err != nil {
			return "", err
		}
	}
	if len(devices) == 0 {
		return "skipped_unreachable", nil
	}
	return "queued", nil
}

// enqueueEmail은 이메일 노드를 처리한다: std_attrs.email 대상, {{ }} 렌더 후 send.email outbox 발행.
// 발송기(provider)는 노드가 지정하면 그 발송기, 미지정이면 활성 발송기(워커 폴백).
func (s *Scheduler) enqueueEmail(ctx context.Context, tx pgx.Tx, c *claimedState, def *Definition, node Node, pol *appPolicy, cat policy.Category, marketing bool, sub map[string]string, attrs map[string]string) (string, error) {
	email := strings.TrimSpace(attrs["email"])
	if email == "" || !strings.Contains(email, "@") {
		s.logSkipChannel(ctx, c, def, "skipped_unreachable", "email") // 이메일 주소 없음
		return "skipped_unreachable", nil
	}
	// marketing은 이메일 수신거부(unsubscribed) 존중 (transactional 우회). 기본 수신 허용.
	if marketing && sub["email"] == "unsubscribed" {
		s.logSkipChannel(ctx, c, def, "skipped_unreachable", "email")
		return "skipped_unreachable", nil
	}
	allowed, err := s.freqCap.Allow(ctx, cat, pol.freqCap, c.appID, c.userID)
	if err != nil {
		return "", err
	}
	if !allowed {
		s.logSkipChannel(ctx, c, def, "skipped_cap", "email")
		return "skipped_cap", nil
	}
	category := "marketing"
	if !marketing {
		category = "transactional"
	}
	idemKey := sendKey(c, def, "email")
	payload := map[string]any{
		"idempotency_key": idemKey,
		"message_id":      uuidString(),
		"user_id":         c.userID,
		"email":           email,
		"content":         map[string]any{"email": map[string]any{"subject": Render(node.Email.Subject, attrs), "html": Render(node.Email.HTML, attrs)}},
		"category":        category,
		"journey_id":      c.journeyID,
		"journey_version": c.version,
		"node_index":      c.currentNode,
	}
	if node.Email.Provider != "" {
		payload["provider"] = node.Email.Provider
	}
	payloadJSON, _ := json.Marshal(payload)
	if _, err := tx.Exec(ctx, `
		INSERT INTO journey_outbox (tenant_id, app_id, stream, idempotency_key, payload)
		VALUES ($1, $2, 'stream:send.email', $3, $4) ON CONFLICT DO NOTHING`,
		c.tenantID, c.appID, idemKey, payloadJSON); err != nil {
		return "", err
	}
	return "queued", nil
}

// pushContent는 send.push의 content.push 맵을 만든다. deep_link는 있을 때만 포함(공통 계약 R-01).
func pushContent(title, body, deepLink string) map[string]any {
	push := map[string]any{"title": title, "body": body}
	if strings.TrimSpace(deepLink) != "" {
		push["deep_link"] = deepLink
	}
	return push
}

// logSkip은 발송 생략 사유를 message_log에 기록한다 (PRD-04 5장 — "왜 안 갔는지").
// 디바이스 단위가 아닌 유저 단위 skip이므로 device_id는 0.
func (s *Scheduler) logSkip(ctx context.Context, c *claimedState, def *Definition, status string) {
	s.logSkipChannel(ctx, c, def, status, "push")
}

func (s *Scheduler) logSkipChannel(ctx context.Context, c *claimedState, def *Definition, status, channel string) {
	s.logSkipReason(ctx, c, def, status, channel, "")
}

// logSkipReason은 생략 사유(failure_detail)까지 남긴다. "왜 안 갔는지"가 설정 실수인 경우
// (템플릿 미승인·발송기 미설정 등) 사유 없이는 콘솔에서 원인을 알 수 없다.
func (s *Scheduler) logSkipReason(ctx context.Context, c *claimedState, def *Definition, status, channel, detail string) {
	if s.ch == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	idemKey := sendKey(c, def, "skip")
	err := s.ch.Exec(ctx, `
		INSERT INTO message_log (tenant_id, app_id, message_id, idempotency_key,
			journey_id, journey_version, node_index, campaign_ref,
			user_id, device_id, channel, status, failure_class, failure_detail, sent_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, '00000000-0000-0000-0000-000000000000',
		        ?, ?, '', ?, ?)`,
		c.tenantID, c.appID, uuidString(), idemKey,
		c.journeyID, uint32(c.version), uint16(c.currentNode),
		c.userID, channel, status, detail, s.clk.Now())
	if err != nil {
		s.logger.Error("skip 로그 기록 실패", "err", err, "state", c.id, "status", status)
	}
}

func sendKey(c *claimedState, def *Definition, deviceID string) string {
	legacy := fmt.Sprintf("%s:%d:%s:%d:%s", c.journeyID, c.version, c.userID, c.currentNode, deviceID)
	if def.SchemaVersion == 2 {
		return "v2:" + legacy + ":" + c.id
	}
	return legacy
}

func (s *Scheduler) loadDefinition(ctx context.Context, journeyID string, version int) (*Definition, error) {
	key := fmt.Sprintf("%s/%d", journeyID, version)
	s.defMu.Lock()
	if d, ok := s.defs[key]; ok {
		s.defMu.Unlock()
		return d, nil
	}
	s.defMu.Unlock()

	var raw []byte
	err := s.pg.QueryRow(ctx,
		`SELECT definition FROM journey_versions WHERE journey_id = $1 AND version = $2`,
		journeyID, version).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("저니 버전 없음: %s/%d", journeyID, version)
		}
		return nil, err
	}
	def, err := ParseDefinition(raw)
	if err != nil {
		return nil, err
	}
	s.defMu.Lock()
	s.defs[key] = def // 불변 버전이므로 영구 캐시 안전
	s.defMu.Unlock()
	return def, nil
}

func mergeAttrs(stdRaw, customRaw []byte) map[string]string {
	out := map[string]string{}
	for _, raw := range [][]byte{stdRaw, customRaw} {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			for k, v := range m {
				switch t := v.(type) {
				case string:
					out[k] = t
				default:
					b, _ := json.Marshal(t)
					out[k] = string(b)
				}
			}
		}
	}
	return out
}
