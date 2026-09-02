package message

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
	"github.com/ondahq/onda/apps/worker/internal/clock"
	"github.com/ondahq/onda/apps/worker/internal/metrics"
	libqueue "github.com/ondahq/onda/packages/libqueue-go"
)

// poller.go — 폴링형 벤더(NHN·알리고)의 발송 결과 확정기.
//
// 콜백형 벤더는 공급자가 결과를 밀어주지만 폴링형은 우리가 당겨야 한다. 당기지 않으면
// message_lifecycle에 sent만 쌓이고 delivered/failed가 영영 없어, 도달률이 0으로 보인다.
//
// 필요한 테이블 (정의는 db/postgres/schema.sql · upgrades/0004_alimtalk.sql):
//
//	pending_receipts(tenant_id, app_id, connector_id, message_id, provider_message_id,
//	                 attempts, next_poll_at, created_at)
//	PK (tenant_id, message_id), INDEX (next_poll_at)
//
// 채널 컬럼은 두지 않는다 — connector_id로 벤더를 찾으면 manifest.channel이 나온다.
const (
	firstPollDelay  = 30 * time.Second // 접수 직후엔 결과가 없다. 한 템포 쉬고 조회한다.
	pollInterval    = 15 * time.Second
	pollBatch       = 200
	maxPollAttempts = 12 // 초과분은 포기하고 행을 지운다(무한 증식 방지). failed로 단정하지 않는다.
	pollBackoffBase = 30 * time.Second
	pollBackoffCap  = 30 * time.Minute
)

// pollSource — 폴링 결과의 lifecycle source. 사실을 만든 주체는 공급자이고
// 커넥터는 그것을 당겨왔을 뿐이라, 웹훅과 같은 provider_callback으로 둔다
// (엔진이 스스로 판단한 connector 이벤트와 구분된다).
const pollSource = "provider_callback"

// receipt — 결과 미확정 접수 한 건.
type receipt struct {
	TenantID          string
	AppID             string
	ConnectorID       string
	MessageID         string
	ProviderMessageID string
	Attempts          int
}

type groupKey struct {
	tenantID    string
	appID       string
	connectorID string
}

// ResultPoller — pending_receipts를 훑어 벤더에 결과를 묻고 message.lifecycle로 확정한다.
type ResultPoller struct {
	pg       *pgxpool.Pool
	res      *resolver
	reg      *alimtalk.Registry
	emit     *emitter
	clk      clock.Clock
	logger   *slog.Logger
	interval time.Duration
}

func NewResultPoller(pg *pgxpool.Pool, reg *alimtalk.Registry, producer *libqueue.Producer,
	masterKey []byte, clk clock.Clock, logger *slog.Logger) *ResultPoller {
	return &ResultPoller{
		pg:       pg,
		res:      newResolver(pg, masterKey, clk),
		reg:      reg,
		emit:     &emitter{producer: producer, logger: logger},
		clk:      clk,
		logger:   logger,
		interval: pollInterval,
	}
}

func (p *ResultPoller) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	p.logger.Info("발송 결과 폴러 시작")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := p.sweep(ctx); err != nil && ctx.Err() == nil {
				p.logger.Error("결과 폴링 실패", "err", err)
				metrics.BatchErrors.WithLabelValues("receipt_poll").Inc()
			}
		}
	}
}

func (p *ResultPoller) sweep(ctx context.Context) error {
	if p.pg == nil {
		return nil
	}
	due, err := p.selectDue(ctx, p.clk.Now(), pollBatch)
	if err != nil {
		return err
	}
	for key, rs := range groupReceipts(due) {
		p.pollGroup(ctx, key, rs)
	}
	return nil
}

// selectDue — 만기 접수를 배치로. 폴러는 전 테넌트를 훑는 스윕이므로 tenant_id 조건이 없고,
// 대신 행의 tenant_id를 그대로 lifecycle envelope에 실어 격리를 유지한다.
func (p *ResultPoller) selectDue(ctx context.Context, now time.Time, limit int) ([]receipt, error) {
	rows, err := p.pg.Query(ctx, `
		SELECT tenant_id, app_id, connector_id, message_id, provider_message_id, attempts
		  FROM pending_receipts
		 WHERE next_poll_at <= $1
		 ORDER BY next_poll_at
		 LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []receipt
	for rows.Next() {
		var r receipt
		if err := rows.Scan(&r.TenantID, &r.AppID, &r.ConnectorID, &r.MessageID,
			&r.ProviderMessageID, &r.Attempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// groupReceipts — 벤더 조회는 (앱, 커넥터) 단위 배치 호출이라 먼저 묶는다.
func groupReceipts(in []receipt) map[groupKey][]receipt {
	out := map[groupKey][]receipt{}
	for _, r := range in {
		k := groupKey{tenantID: r.TenantID, appID: r.AppID, connectorID: r.ConnectorID}
		out[k] = append(out[k], r)
	}
	return out
}

func (p *ResultPoller) pollGroup(ctx context.Context, key groupKey, rs []receipt) {
	v, err := p.reg.Get(key.connectorID)
	if err != nil {
		// 커넥터가 사라졌다(제거·매니페스트 미배치). 조회할 방법이 없으므로 행을 정리한다.
		p.logger.Warn("결과 폴링 대상 커넥터 없음 — 접수 정리", "connector_id", key.connectorID, "count", len(rs))
		p.drop(ctx, key.tenantID, messageIDs(rs))
		return
	}
	m := v.Manifest()
	b, found, note, err := p.res.resolve(ctx, key.tenantID, key.appID, m.Channel)
	if err != nil {
		p.logger.Error("폴링 크리덴셜 조회 실패", "err", err, "app_id", key.appID)
		return // 다음 tick에서 재시도 (next_poll_at 그대로)
	}
	if !found {
		p.logger.Warn("폴링 크리덴셜 없음 — 백오프", "app_id", key.appID, "note", note)
		p.backoff(ctx, key.tenantID, rs)
		return
	}

	pending := make([]alimtalk.Receipt, 0, len(rs))
	for _, r := range rs {
		pending = append(pending, alimtalk.Receipt{
			ProviderMessageID: r.ProviderMessageID, MessageID: r.MessageID,
		})
	}
	cred := alimtalk.Credential{ConnectorID: key.connectorID, JSON: b.Credential, Config: b.Config}
	events, err := v.PollResults(ctx, cred, pending)
	if err != nil {
		p.logger.Warn("PollResults 실패 — 백오프", "err", err, "connector_id", key.connectorID)
		p.backoff(ctx, key.tenantID, rs)
		return
	}

	byID := indexReceipts(rs)
	terminal := make([]string, 0, len(events))
	for _, ev := range events {
		r, known := byID[ev.MessageID]
		switch decidePollEvent(ev, known) {
		case pollIgnore:
			p.logger.Warn("해석할 수 없는 폴링 결과 — 무시",
				"message_id", ev.MessageID, "status", ev.Status, "known", known, "connector_id", key.connectorID)
		case pollDrop:
			p.logger.Warn("공급자 조회 보존 기간 초과 — 결과 미확정으로 정리",
				"message_id", r.MessageID, "connector_id", key.connectorID)
			terminal = append(terminal, r.MessageID)
		case pollEmit:
			if !m.Reports(ev.Status) {
				// 선언하지 않은 상태를 보고했다. 버리지는 않되 manifest 불일치로 남긴다.
				p.logger.Warn("manifest 미선언 상태 보고", "status", ev.Status, "connector_id", key.connectorID)
			}
			src := &libqueue.Envelope{TenantID: r.TenantID, AppID: r.AppID, TraceID: uuid.NewString()}
			p.emit.emit(ctx, src, buildPollEvent(r, ev, m.Channel), ev.OccurredAt)
			metrics.LifecycleEvents.WithLabelValues(ev.Status).Inc()
			// 종결 여부는 벤더가 정한다. 상태에서 유추하면 폴링과 콜백이 갈린다:
			// 대체발송된 sent는 종결이지만 원 채널의 sent는 아니고, 그 차이는 벤더만 안다.
			if ev.Terminal {
				terminal = append(terminal, r.MessageID)
			}
		}
	}
	p.drop(ctx, key.tenantID, terminal)
	p.backoff(ctx, key.tenantID, remaining(rs, terminal))
}

// pollAction — 폴링 결과 한 건을 어떻게 처리할지.
type pollAction int

const (
	pollIgnore pollAction = iota // 우리 접수가 아니거나 상태를 해석할 수 없다 — 그대로 둔다
	pollDrop                     // 이벤트 없이 대기 목록에서만 뺀다
	pollEmit                     // lifecycle 발행 (종결 여부는 ev.Terminal이 정한다)
)

// decidePollEvent — 판단부를 순수 함수로 뺀다. 폴링 결과 처리의 오답은
// 조용히 집계를 틀어놓기만 하고 아무 데도 티가 나지 않아서, 표로 굳혀 둔다.
func decidePollEvent(ev alimtalk.Event, known bool) pollAction {
	switch {
	case !known:
		return pollIgnore
	case ev.Expired:
		// "결과가 확정됐다"가 아니라 "더는 알아낼 수 없다"이다. 이벤트를 발행하면
		// 알 수 없는 건이 성공이나 실패로 집계된다.
		return pollDrop
	case !validLifecycleStatus[ev.Status]:
		return pollIgnore
	default:
		return pollEmit
	}
}

var validLifecycleStatus = map[string]bool{
	"accepted": true, "sent": true, "delivered": true, "opened": true,
	"clicked": true, "failed": true, "unsubscribed": true, "bounced": true,
}

// buildPollEvent — 벤더 Event → message.lifecycle.v1.
//
// ch는 원 채널이지만, 벤더 대체발송으로 실제 도달 채널이 바뀌었으면(DeliveredVia)
// 그쪽을 싣는다. 안 그러면 SMS로 나간 건이 알림톡 도달률·원가에 잡혀 집계가 조용히 틀어진다.
func buildPollEvent(r receipt, ev alimtalk.Event, ch string) lifecycleEvent {
	providerID := ev.ProviderMessageID
	if providerID == "" {
		providerID = r.ProviderMessageID
	}
	if ev.DeliveredVia != "" {
		ch = ev.DeliveredVia
	}
	out := lifecycleEvent{
		MessageID:         r.MessageID,
		Status:            ev.Status,
		OccurredAt:        ev.OccurredAt.UTC().Format(time.RFC3339Nano),
		Source:            pollSource,
		Channel:           ch,
		ConnectorID:       r.ConnectorID,
		ProviderMessageID: strPtr(providerID),
		FallbackIndex:     intPtr(0),
	}
	if ev.Status == "failed" || ev.FailureClass != "" {
		class, detail := normalizeClass(ev.FailureClass, ev.FailureDetail)
		out.FailureClass = strPtr(class)
		out.FailureDetail = strPtr(detail)
	}
	if ev.CostCurrency != "" {
		out.Cost = &struct {
			Currency string  `json:"currency"`
			Amount   float64 `json:"amount"`
		}{Currency: ev.CostCurrency, Amount: ev.CostAmount}
	}
	return out
}

func indexReceipts(rs []receipt) map[string]receipt {
	out := make(map[string]receipt, len(rs))
	for _, r := range rs {
		out[r.MessageID] = r
	}
	return out
}

func messageIDs(rs []receipt) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.MessageID)
	}
	return out
}

// remaining — 종결되지 않아 다음 조회가 필요한 접수.
func remaining(rs []receipt, terminal []string) []receipt {
	if len(terminal) == 0 {
		return rs
	}
	done := make(map[string]bool, len(terminal))
	for _, id := range terminal {
		done[id] = true
	}
	out := make([]receipt, 0, len(rs))
	for _, r := range rs {
		if !done[r.MessageID] {
			out = append(out, r)
		}
	}
	return out
}

// pollBackoff — 조회 간격. 지수적으로 늘려 결과가 늦는 접수가 매 tick마다 벤더를 두드리지 않게 한다.
func pollBackoff(attempts int) time.Duration {
	if attempts < 1 {
		return pollBackoffBase
	}
	d := pollBackoffBase << (attempts - 1)
	if d <= 0 || d > pollBackoffCap {
		return pollBackoffCap
	}
	return d
}

// splitExhausted — 상한을 넘긴 접수(포기)와 계속 조회할 접수를 가른다.
func splitExhausted(rs []receipt) (giveUp, keep []receipt) {
	for _, r := range rs {
		if r.Attempts+1 >= maxPollAttempts {
			giveUp = append(giveUp, r)
			continue
		}
		keep = append(keep, r)
	}
	return giveUp, keep
}

// backoff — 미종결 접수의 다음 조회 시각을 민다. 상한 초과분은 포기하고 지운다.
// 포기한 건을 failed로 단정하지 않는다: 조회에 실패한 것과 발송에 실패한 것은 다르고,
// 도달했을 수도 있는 발송을 실패로 기록하면 리포트가 거짓말을 하게 된다.
func (p *ResultPoller) backoff(ctx context.Context, tenantID string, rs []receipt) {
	if len(rs) == 0 || p.pg == nil {
		return
	}
	giveUp, keep := splitExhausted(rs)
	if len(giveUp) > 0 {
		p.logger.Warn("결과 조회 상한 초과 — 접수 포기", "count", len(giveUp), "tenant_id", tenantID)
		p.drop(ctx, tenantID, messageIDs(giveUp))
	}
	// 같은 배치는 attempts가 대체로 같으므로 한 번의 UPDATE로 민다.
	for _, r := range keep {
		next := p.clk.Now().Add(pollBackoff(r.Attempts + 1))
		if _, err := p.pg.Exec(ctx, `
			UPDATE pending_receipts SET attempts = attempts + 1, next_poll_at = $3
			 WHERE tenant_id = $1 AND message_id = $2`, tenantID, r.MessageID, next); err != nil {
			p.logger.Error("pending_receipts 백오프 실패", "err", err, "message_id", r.MessageID)
			return
		}
	}
}

func (p *ResultPoller) drop(ctx context.Context, tenantID string, ids []string) {
	if len(ids) == 0 || p.pg == nil {
		return
	}
	if _, err := p.pg.Exec(ctx, `
		DELETE FROM pending_receipts WHERE tenant_id = $1 AND message_id = ANY($2)`,
		tenantID, ids); err != nil {
		p.logger.Error("pending_receipts 정리 실패", "err", err, "count", len(ids))
	}
}
