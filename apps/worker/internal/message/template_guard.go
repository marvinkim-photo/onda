package message

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
)

// template_guard.go — 발송 직전 승인 템플릿 검증.
//
// 알림톡 본문의 단일 출처는 카카오 승인 원본이고, 우리는 그 사본을 alimtalk_templates에
// 캐시한다(internal/templatesync). 이 사본이 있어야 두 가지를 할 수 있다.
//   - 필수 치환자를 승인 본문에서 도출해 빠진 값을 벤더에 가기 전에 잡는다.
//   - 승인되지 않은(심사중·반려·벤더에서 사라진) 템플릿으로 나가는 발송을 끊는다.
//
// 사본이 없을 때 전량 실패시키지 않는 이유: 동기화는 테넌트가 언제 돌리는지 알 수 없고,
// 아직 한 번도 동기화하지 않은 테넌트의 정상 발송(주문 알림 등)을 검증 캐시가 없다는
// 이유로 막으면 그게 더 큰 사고다. 그래서 미동기화는 경고 후 진행, 사본이 있는데
// 승인 상태가 아니면 실패로 나눈다.

// storedTemplateSQL — 발송 대상 템플릿 한 건. 같은 template_code가 발신프로필마다
// 따로 존재할 수 있으므로 sender_key까지 맞춰야 "이 발송의 템플릿"이 된다.
const storedTemplateSQL = `
	SELECT t.content, t.message_type, t.status, t.vendor_status
	  FROM alimtalk_templates t
	  JOIN alimtalk_senders s ON s.id = t.sender_id AND s.tenant_id = t.tenant_id
	 WHERE t.tenant_id = $1 AND t.app_id = $2 AND t.template_code = $3 AND s.sender_key = $4
	 LIMIT 1`

// loadStoredTemplate — 캐시된 승인 템플릿. 없으면 (nil, nil).
//
// 조회 실패(테이블 미생성·PG 일시 장애)도 nil로 돌린다. 검증은 발송을 돕는 가드이지
// 발송 자체가 아니므로, 가드용 조회가 흔들린다고 전 발송을 세우지 않는다 — 대신 경고를 남긴다.
func (w *Worker) loadStoredTemplate(ctx context.Context, tenantID, appID, senderKey, code string) *alimtalk.Template {
	if w.pg == nil || senderKey == "" || code == "" {
		return nil
	}
	var t alimtalk.Template
	var status string
	err := w.pg.QueryRow(ctx, storedTemplateSQL, tenantID, appID, code, senderKey).
		Scan(&t.Content, &t.MessageType, &status, &t.VendorStatus)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			w.logger.Warn("승인 템플릿 조회 실패 — 검증 없이 진행",
				"err", err, "template_code", code, "app_id", appID)
		}
		return nil
	}
	t.Code = code
	t.Status = alimtalk.TemplateStatus(status)
	return &t
}

// guardTemplate — 캐시본 대조. 미동기화는 경고 후 통과, 미승인·치환자 누락은 permanent_content.
func (w *Worker) guardTemplate(job *Job, req alimtalk.SendRequest) error {
	if job.Template == nil {
		w.logger.Warn("승인 템플릿 미동기화 — 발송 전 검증을 건너뛴다",
			"template_code", req.TemplateCode, "sender_key", req.SenderKey,
			"connector_id", job.ConnectorID)
		return nil
	}
	if err := alimtalk.ValidateSend(*job.Template, req, job.Manifest.SubstitutionMode()); err != nil {
		return channel.NewSendError(channel.FailurePermanentContent,
			"승인 템플릿 검증 실패(%s): %s", req.TemplateCode, err.Error())
	}
	return nil
}

// senderKeyFor — 이 발송의 발신프로필 키. 발송이 지정한 값이 우선이고, 없으면 커넥터 배선 설정.
// buildRequest와 템플릿 조회가 같은 답을 봐야 하므로 한 곳에 둔다.
func senderKeyFor(job *Job) string {
	if job.P != nil && job.P.Content.Template != nil && job.P.Content.Template.SenderKey != "" {
		return job.P.Content.Template.SenderKey
	}
	return configString(job.Config, "sender_key")
}
