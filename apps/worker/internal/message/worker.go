package message

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
	"github.com/ondahq/onda/apps/worker/internal/clock"
	"github.com/ondahq/onda/apps/worker/internal/connector"
	libqueue "github.com/ondahq/onda/packages/libqueue-go"
)

const zeroUUID = "00000000-0000-0000-0000-000000000000"

// KeyPrefix — Redis 멱등 키 네임스페이스. push("send")·email("send:email")과 겹치면 안 된다.
const keyPrefix = "send:message"

// Job — 한 건의 처리 상태. SendLoop이 Parse→Resolve→Send로 같은 포인터를 넘기므로
// Resolve가 해석한 커넥터·벤더를 Send가 이어받는 통로이기도 하다
// (SendHandler에는 Resolve→Send로 값을 넘길 다른 자리가 없다 — Credentials는 비밀 전용).
type Job struct {
	P *Payload

	// --- Resolve가 채운다 ---
	ConnectorID string
	Config      []byte
	Vendor      alimtalk.Vendor
	Manifest    connector.Manifest
	// Template — 캐시된 승인 템플릿(alimtalk_templates). nil이면 아직 동기화되지 않았다는 뜻이며,
	// 그때는 발송 전 검증을 건너뛴다 (template_guard.go).
	Template *alimtalk.Template
	// Note — 해석 실패·불일치 사유. credential_missing 종결 시 로그·lifecycle detail로 흐른다.
	Note string
}

// Worker — send.message.v1 소비 핸들러. channel.SendLoop이 멱등·백오프·DLQ·message_log를 맡고,
// 이 타입은 채널별 차이(커넥터 해석·벤더 발송·수명주기 발행)만 담당한다.
type Worker struct {
	res    *resolver
	reg    *alimtalk.Registry
	pg     *pgxpool.Pool
	emit   *emitter
	clk    clock.Clock
	logger *slog.Logger
}

func NewWorker(pg *pgxpool.Pool, reg *alimtalk.Registry, producer *libqueue.Producer,
	masterKey []byte, clk clock.Clock, logger *slog.Logger) *Worker {
	return &Worker{
		res:    newResolver(pg, masterKey, clk),
		reg:    reg,
		pg:     pg,
		emit:   &emitter{producer: producer, logger: logger},
		clk:    clk,
		logger: logger,
	}
}

var _ channel.SendHandler[*Job] = (*Worker)(nil)

func (w *Worker) KeyPrefix() string { return keyPrefix }

func (w *Worker) Parse(env *libqueue.Envelope) (*Job, string, string, bool) {
	p, err := Parse(env.Payload)
	if err != nil {
		w.logger.Warn("send.message payload 불량 — skip", "err", err, "msg_id", env.ID)
		return nil, "", "", false
	}
	return &Job{P: p}, p.IdempotencyKey, p.MessageID, true
}

// Resolve — 채널 배선(channel_connectors) + 크리덴셜(credentials) + 벤더(registry).
// found=false는 "설정이 안 됐다"이지 "장애"가 아니므로 재시도하지 않고 종결한다.
func (w *Worker) Resolve(ctx context.Context, env *libqueue.Envelope, job *Job) (channel.Credentials, bool, error) {
	b, found, note, err := w.res.resolve(ctx, env.TenantID, env.AppID, job.P.Channel)
	if err != nil {
		return channel.Credentials{}, false, err
	}
	if !found {
		job.Note = note
		return channel.Credentials{}, false, nil
	}
	// 커넥터 선택: 저니가 발행 시점에 못박은 payload의 connector.id가 우선이고,
	// 비어 있거나 그 커넥터가 사라졌을 때만 앱 배선으로 되돌아간다.
	//
	// 주의: config와 크리덴셜은 (app, channel) 배선 행 하나뿐이라 커넥터가 어긋나도
	// 그 행의 것을 쓴다. 즉 배선을 바꾼 뒤 구(舊) 커넥터를 못박은 인플라이트 발송은
	// 새 벤더의 설정·비밀로 나간다. 그래서 어긋나면 경고를 남긴다.
	connectorID := b.ConnectorID
	if pinned := job.P.Connector.ID; pinned != "" && pinned != b.ConnectorID {
		if _, perr := w.reg.Get(pinned); perr == nil {
			w.logger.Warn("커넥터 불일치 — 발송이 못박은 커넥터를 쓴다",
				"pinned", pinned, "binding", b.ConnectorID, "app_id", env.AppID)
			connectorID = pinned
		} else {
			w.logger.Warn("발송이 못박은 커넥터가 없어 배선으로 되돌린다",
				"pinned", pinned, "binding", b.ConnectorID, "app_id", env.AppID)
		}
	}
	v, verr := w.reg.Get(connectorID)
	if verr != nil {
		job.Note = verr.Error()
		return channel.Credentials{}, false, nil
	}
	job.ConnectorID = connectorID
	job.Config = b.Config
	job.Vendor = v
	job.Manifest = v.Manifest()
	// 승인 템플릿 캐시본을 여기서 실어 둔다 — Send는 벤더 호출 직전에 이걸로 검증한다.
	if tmpl := job.P.Content.Template; tmpl != nil {
		job.Template = w.loadStoredTemplate(ctx, env.TenantID, env.AppID, senderKeyFor(job), tmpl.Code)
	}
	return channel.Credentials{Kind: credentialKind(job.P.Channel), JSON: b.Credential}, true, nil
}

// Send — 벤더 발송. 벤더의 Classify를 여기서 오류에 실어 둔다:
// SendHandler.Classify(err)에는 job이 없어 벤더에 닿을 수 없기 때문이다.
func (w *Worker) Send(ctx context.Context, _ *libqueue.Envelope, job *Job, creds channel.Credentials) (string, error) {
	if job.P.Channel != alimtalk.ChannelID {
		return "", channel.NewSendError(channel.FailurePermanentContent,
			"채널 %s는 아직 message 워커가 지원하지 않는다", job.P.Channel)
	}
	req, err := w.buildRequest(job)
	if err != nil {
		return "", err
	}
	// 승인 본문 대조는 벤더에 가기 전에. 공급자 거절과 과금을 아끼고, 무엇보다
	// 미승인 템플릿이 실제로 발송되는 일을 막는다.
	if err := w.guardTemplate(job, req); err != nil {
		return "", err
	}
	req.Credential = vendorCredential(job, creds)
	rc, sendErr := job.Vendor.Send(ctx, req)
	if sendErr != nil {
		return "", &channel.SendError{
			Class:      job.Vendor.Classify(sendErr),
			Detail:     sendErr.Error(),
			RetryAfter: channel.RetryAfterOf(sendErr),
		}
	}
	return rc.ProviderMessageID, nil
}

// buildRequest — payload → 벤더 중립 SendRequest. manifest 선언에 따라 무엇을 채울지 고른다.
func (w *Worker) buildRequest(job *Job) (alimtalk.SendRequest, error) {
	p := job.P
	tmpl := p.Content.Template
	if tmpl == nil {
		return alimtalk.SendRequest{}, channel.NewSendError(channel.FailurePermanentContent,
			"알림톡은 content.template이 필수인데 없다")
	}
	if p.Target.Value == "" {
		return alimtalk.SendRequest{}, channel.NewSendError(channel.FailureInvalidTarget, "수신 번호 없음")
	}
	senderKey := senderKeyFor(job)
	if senderKey == "" {
		return alimtalk.SendRequest{}, channel.NewSendError(channel.FailurePermanentContent,
			"발신프로필 키 없음 (content.template.sender_key 또는 커넥터 config.sender_key)")
	}
	buttons, err := mapButtons(p.Content.Buttons)
	if err != nil {
		return alimtalk.SendRequest{}, channel.NewSendError(channel.FailurePermanentContent, "%s", err.Error())
	}
	// 광고성 여부는 카테고리에서 온다(정책 엔진이 템플릿 유형으로 이미 정했다).
	if err := alimtalk.ValidateButtons(buttons, p.Category == "marketing"); err != nil {
		return alimtalk.SendRequest{}, channel.NewSendError(channel.FailurePermanentContent, "%s", err.Error())
	}

	req := alimtalk.SendRequest{
		MessageID:      p.MessageID,
		IdempotencyKey: p.IdempotencyKey,
		SenderKey:      senderKey,
		TemplateCode:   tmpl.Code,
		To:             p.Target.Value,
		Buttons:        buttons,
	}
	if req.TemplateCode == "" {
		return alimtalk.SendRequest{}, channel.NewSendError(channel.FailurePermanentContent, "템플릿 코드 없음")
	}

	// 치환을 누가 하는가 — 벤더 변이 첫 번째 축. manifest 선언대로만 채운다.
	mode := job.Manifest.SubstitutionMode()
	if mode == connector.SubstitutionVariables || mode == connector.SubstitutionBoth {
		req.Variables = tmpl.Variables
	}
	if mode == connector.SubstitutionRendered || mode == connector.SubstitutionBoth {
		if tmpl.RenderedPreview == "" {
			// 완성 본문을 요구하는 벤더인데 엔진이 렌더를 안 보냈다. 빈 본문을 보내면
			// 승인 템플릿과 불일치라 공급자가 거절하거나 이상한 문구가 나간다 — 여기서 끊는다.
			return alimtalk.SendRequest{}, channel.NewSendError(channel.FailurePermanentContent,
				"커넥터 %s는 완성 본문(substitution=%s)이 필요한데 content.template.rendered_preview가 비어 있다",
				job.ConnectorID, mode)
		}
		if n := utf8.RuneCountInString(tmpl.RenderedPreview); n > alimtalk.MaxBodyRunes {
			return alimtalk.SendRequest{}, channel.NewSendError(channel.FailurePermanentContent,
				"본문이 %d자로 상한(%d자)을 넘는다", n, alimtalk.MaxBodyRunes)
		}
		req.RenderedText = tmpl.RenderedPreview
	}

	// 대체발송을 누가 하는가 — 두 번째 축. 선언한 벤더에만 넘긴다.
	// 미선언 벤더에 넘기면 벤더가 무시해 폴백이 통째로 사라지고, 그 사실을 아무도 모른다.
	if job.Manifest.Capabilities.VendorFallback {
		req.Fallback = vendorFallback(p.Fallback)
	}
	if job.Manifest.Capabilities.ScheduledSend && p.Options.ScheduledFor != nil && *p.Options.ScheduledFor != "" {
		if t, perr := time.Parse(time.RFC3339, *p.Options.ScheduledFor); perr == nil {
			req.ScheduledAt = &t
		}
	}
	return req, nil
}

// vendorCredential — 벤더에 넘길 크리덴셜. 발송 시점에 Job에서 재구성한다.
func vendorCredential(job *Job, creds channel.Credentials) alimtalk.Credential {
	return alimtalk.Credential{ConnectorID: job.ConnectorID, JSON: creds.JSON, Config: job.Config}
}

// buttonType — send.message.v1 정규화 이름 → 카카오 원본 코드.
var buttonType = map[string]string{
	"web_link":         "WL",
	"app_link":         "AL",
	"bot_keyword":      "BK",
	"message_delivery": "MD",
	"add_channel":      "AC",
}

func mapButtons(in []Button) ([]alimtalk.Button, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]alimtalk.Button, 0, len(in))
	for i, b := range in {
		code, ok := buttonType[b.Type]
		if !ok {
			return nil, fmt.Errorf("버튼 %d: 알 수 없는 타입 %q", i+1, b.Type)
		}
		out = append(out, alimtalk.Button{
			Type: code, Name: b.Name,
			LinkMo: b.URLMobile, LinkPC: b.URLPC,
			LinkIOS: b.SchemeIOS, LinkAndroid: b.SchemeAndroid,
		})
	}
	return out, nil
}

// vendorFallback — fallback 체인에서 문자 대체발송 한 건을 뽑는다.
// 벤더 대체발송은 SMS/LMS 한 단계만 지원하므로 첫 번째 문자 단계만 넘긴다.
func vendorFallback(steps []Step) *alimtalk.Fallback {
	for _, s := range steps {
		if s.Content.Text == nil || s.Content.Text.Body == "" {
			continue
		}
		switch s.Channel {
		case "sms", "lms":
			t := "SMS"
			if s.Channel == "lms" {
				t = "LMS"
			}
			return &alimtalk.Fallback{
				Type:     t,
				Title:    s.Content.Text.Title,
				Text:     s.Content.Text.Body,
				SenderNo: s.Content.Text.Sender,
			}
		}
	}
	return nil
}

// configString — channel_connectors.config에서 문자열 하나를 꺼낸다(없으면 "").
func configString(cfg []byte, key string) string {
	if len(cfg) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &m); err != nil {
		return ""
	}
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// Classify — Send가 이미 벤더 분류를 SendError에 실어 두었다.
func (w *Worker) Classify(err error) channel.FailureClass { return channel.Classify(err) }

// OnTerminal — 종결 부수효과. 여기서 message.lifecycle을 발행한다.
func (w *Worker) OnTerminal(ctx context.Context, env *libqueue.Envelope, job *Job, out channel.SendOutcome) {
	p := job.P
	connectorID := job.ConnectorID
	if connectorID == "" {
		connectorID = p.Connector.ID // 해석 실패 시에도 리포트가 어느 커넥터인지 알아야 한다
	}
	attempt := out.Attempts
	if attempt < 1 {
		attempt = 1
	}
	ev := lifecycleEvent{
		MessageID:      p.MessageID,
		IdempotencyKey: p.IdempotencyKey,
		OccurredAt:     out.At.UTC().Format(time.RFC3339Nano),
		Source:         "connector",
		Channel:        p.Channel,
		ConnectorID:    connectorID,
		UserID:         strPtr(p.UserID),
		EndpointID:     strPtr(p.Target.EndpointID),
		FallbackIndex:  intPtr(0), // 원 발송. 폴백 시도는 엔진이 별도 message_id로 발행한다.
		Attempt:        intPtr(attempt),
	}

	switch out.Status {
	case "sent":
		ev.Status = "sent"
		ev.ProviderMessageID = strPtr(out.ProviderID)
		w.trackReceipt(ctx, env, job, out)
	case "failed":
		ev.Status = "failed"
		detail := out.FailureDetail
		if job.Note != "" {
			detail = job.Note // 해석 실패는 SendLoop의 고정 문구보다 사유가 훨씬 유용하다
		}
		class, detail := normalizeClass(out.FailureClass, detail)
		ev.FailureClass = strPtr(class)
		ev.FailureDetail = strPtr(detail)
		if out.FailureClass == "credential_auth" {
			w.markCredentialError(ctx, env.TenantID, env.AppID, p.Channel, out.FailureDetail)
		}
	default:
		return // duplicate 등은 새 사실이 아니다
	}
	w.emit.emit(ctx, env, ev, out.At)
}

// trackReceipt — 폴링형 벤더의 접수를 pending_receipts에 적재한다.
// 이 행이 없으면 결과 폴러가 조회할 대상을 모르고, 도달·실패가 영영 확정되지 않는다.
func (w *Worker) trackReceipt(ctx context.Context, env *libqueue.Envelope, job *Job, out channel.SendOutcome) {
	if w.pg == nil || out.ProviderID == "" || !job.Manifest.NeedsPolling() {
		return
	}
	if _, err := w.pg.Exec(ctx, `
		INSERT INTO pending_receipts (tenant_id, app_id, connector_id, message_id,
			provider_message_id, attempts, next_poll_at)
		VALUES ($1, $2, $3, $4, $5, 0, $6)
		ON CONFLICT (tenant_id, message_id) DO UPDATE SET
			provider_message_id = EXCLUDED.provider_message_id,
			attempts = 0, next_poll_at = EXCLUDED.next_poll_at`,
		env.TenantID, env.AppID, job.ConnectorID, job.P.MessageID,
		out.ProviderID, out.At.Add(firstPollDelay)); err != nil {
		w.logger.Error("pending_receipts 적재 실패", "err", err, "message_id", job.P.MessageID)
	}
}

// markCredentialError — 401/403은 앱 전량 실패로 번지므로 즉시 콘솔에 드러낸다 (C-8).
func (w *Worker) markCredentialError(ctx context.Context, tenantID, appID, ch, detail string) {
	if w.pg == nil {
		return
	}
	kind := credentialKind(ch)
	if _, err := w.pg.Exec(ctx, `
		UPDATE credentials SET status = 'error', status_detail = $3, updated_at = now()
		 WHERE app_id = $1 AND kind = $2`, appID, kind, detail); err != nil {
		w.logger.Error("크리덴셜 error 전환 실패", "err", err, "kind", kind)
	}
	w.res.invalidate(tenantID, appID, ch)
}

// Row — message_log 16개 값. 컬럼 순서는 sendloop.go flushLog의 INSERT와 1:1이다.
func (w *Worker) Row(env *libqueue.Envelope, job *Job, out channel.SendOutcome) []any {
	p := job.P
	journeyID := zeroUUID
	if p.JourneyID != nil && *p.JourneyID != "" {
		journeyID = *p.JourneyID
	}
	var version uint32
	var node uint16
	if p.JourneyVersion != nil && *p.JourneyVersion > 0 {
		version = uint32(*p.JourneyVersion)
	}
	if p.NodeIndex != nil && *p.NodeIndex > 0 {
		node = uint16(*p.NodeIndex)
	}
	campaignRef := ""
	if p.CampaignRef != nil {
		campaignRef = *p.CampaignRef
	}
	detail := out.FailureDetail
	if out.Status == "failed" && job.Note != "" {
		detail = job.Note
	}
	return []any{
		env.TenantID, env.AppID, p.MessageID, p.IdempotencyKey,
		journeyID, version, node, campaignRef,
		uuidOrZero(p.UserID), uuidOrZero(p.Target.EndpointID), p.Channel,
		out.Status, out.FailureClass, detail, out.At, out.ProviderID,
	}
}

// uuidOrZero — message_log의 user_id·device_id는 UUID 컬럼이다. 비UUID 식별자가 오면
// 행을 통째로 잃느니 zero UUID로 적재한다(원문은 실패 detail·lifecycle에 남는다).
func uuidOrZero(s string) string {
	if s == "" {
		return zeroUUID
	}
	if _, err := uuid.Parse(s); err != nil {
		return zeroUUID
	}
	return s
}

// DLQ — 재시도 소진분 적재. cmd/dlq가 원본 envelope으로 재처리한다.
func (w *Worker) DLQ(ctx context.Context, env *libqueue.Envelope, job *Job, out channel.SendOutcome) {
	if w.pg == nil {
		return
	}
	envJSON, _ := json.Marshal(env)
	var mid any
	if _, err := uuid.Parse(job.P.MessageID); err == nil {
		mid = job.P.MessageID
	}
	if _, err := w.pg.Exec(ctx, `
		INSERT INTO send_dlq (tenant_id, app_id, idempotency_key, message_id,
			failure_class, failure_detail, attempts, envelope)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, idempotency_key) DO UPDATE SET
			failure_class = EXCLUDED.failure_class, failure_detail = EXCLUDED.failure_detail,
			attempts = EXCLUDED.attempts, envelope = EXCLUDED.envelope,
			created_at = now(), replayed_at = NULL`,
		env.TenantID, env.AppID, job.P.IdempotencyKey, mid,
		out.FailureClass, out.FailureDetail, out.Attempts, envJSON); err != nil {
		w.logger.Error("DLQ 적재 실패", "err", err, "idem", job.P.IdempotencyKey)
	}
}
