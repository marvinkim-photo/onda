package templatesync

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
)

// VendorStatusMissing — 벤더 목록에서 사라진 템플릿에 남기는 표식.
//
// 사라진 템플릿을 지우지 않는 이유: 저니가 template_code로 참조하고 있을 수 있고,
// 행이 사라지면 발송 시 "템플릿 미동기화"로 보여 ValidateSend가 통과해 버린다
// (worker.go — 미동기화는 경고 후 진행). 즉 삭제는 검증을 조용히 끄는 결과가 된다.
//
// 그래서 행은 남기되 status='rejected' + 이 표식으로 못 쓰는 상태임을 명시한다.
// 승인 본문·변수는 그대로 두므로 콘솔은 "무엇이 사라졌는지"를 보여줄 수 있고,
// 발송은 벤더에 다녀오기 전에 permanent_content로 끊긴다.
const VendorStatusMissing = "ONDA_MISSING_IN_VENDOR"

// Scope — upsert 대상 범위. 템플릿은 발신프로필 단위로만 의미가 있다.
type Scope struct {
	TenantID string
	AppID    string
	SenderID string
}

// Row — alimtalk_templates 한 행. 벤더 Template에서의 파생 규칙(치환자 추출·상태 정규화·
// jsonb 직렬화)을 PG 앞에서 끝내 두면, 그 규칙이 DB 없이 테스트된다.
type Row struct {
	Code          string
	Name          string
	Content       string
	MessageType   string
	EmphasizeType string
	// Variables — 승인 본문에서 뽑은 치환자 이름. 콘솔의 변수 매핑 UI가 읽는 컬럼이다.
	Variables []string
	// Buttons·QuickReplies — jsonb 컬럼에 그대로 들어가는 JSON 문자열.
	Buttons      string
	QuickReplies string
	Status       string
	VendorStatus string
}

// BuildRow — 벤더 템플릿 → 저장 행.
func BuildRow(t alimtalk.Template) (Row, error) {
	buttons, err := marshalJSON(t.Buttons)
	if err != nil {
		return Row{}, fmt.Errorf("템플릿 %s 버튼 직렬화: %w", t.Code, err)
	}
	quick, err := marshalJSON(t.QuickReplies)
	if err != nil {
		return Row{}, fmt.Errorf("템플릿 %s 바로연결 직렬화: %w", t.Code, err)
	}
	return Row{
		Code: t.Code, Name: t.Name, Content: t.Content,
		MessageType: t.MessageType, EmphasizeType: t.EmphasizeType,
		// 치환자는 벤더 응답이 아니라 승인 본문에서 뽑는다 — 본문이 유일한 진실이고,
		// 벤더마다 변수 목록을 주기도/안 주기도 한다.
		Variables:    alimtalk.Variables(t.Content),
		Buttons:      buttons,
		QuickReplies: quick,
		Status:       normalizeStatus(t.Status),
		VendorStatus: t.VendorStatus,
	}, nil
}

// BuildRows — 목록 변환. 한 건이라도 직렬화에 실패하면 전체를 멈춘다(부분 반영 금지).
func BuildRows(tmpls []alimtalk.Template) ([]Row, error) {
	rows := make([]Row, 0, len(tmpls))
	for _, t := range tmpls {
		r, err := BuildRow(t)
		if err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// Store — 동기화 결과의 영속화. 테스트가 가짜 구현을 넣을 수 있게 인터페이스로 둔다.
type Store interface {
	// Upsert — (app_id, sender_id, template_code) 기준 upsert. 기록한 행 수를 돌려준다.
	Upsert(ctx context.Context, scope Scope, rows []Row, now time.Time) (int, error)
	// MarkMissing — presentCodes에 없는 기존 행을 사라진 것으로 표시. 표시한 행 수를 돌려준다.
	MarkMissing(ctx context.Context, scope Scope, presentCodes []string, now time.Time) (int, error)
}

// pgStore — PostgreSQL 구현.
type pgStore struct{ pg *pgxpool.Pool }

// NewStore — alimtalk_templates에 쓰는 Store.
func NewStore(pg *pgxpool.Pool) Store { return &pgStore{pg: pg} }

// upsertSQL — ON CONFLICT 대상은 테이블의 UNIQUE (app_id, sender_id, template_code)다.
// 유니크 키에 tenant_id가 없으므로 DO UPDATE에 tenant 조건을 붙여, 만에 하나 app_id가
// 다른 테넌트 것으로 넘어오더라도 남의 행을 덮어쓰지 못하게 한다.
const upsertSQL = `
	INSERT INTO alimtalk_templates (tenant_id, app_id, sender_id, template_code, name, content,
		message_type, emphasize_type, variables, buttons, quick_replies, status, vendor_status,
		synced_at, updated_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)
	ON CONFLICT (app_id, sender_id, template_code) DO UPDATE SET
		name = EXCLUDED.name, content = EXCLUDED.content,
		message_type = EXCLUDED.message_type, emphasize_type = EXCLUDED.emphasize_type,
		variables = EXCLUDED.variables, buttons = EXCLUDED.buttons,
		quick_replies = EXCLUDED.quick_replies, status = EXCLUDED.status,
		vendor_status = EXCLUDED.vendor_status, synced_at = EXCLUDED.synced_at,
		updated_at = EXCLUDED.updated_at
	WHERE alimtalk_templates.tenant_id = EXCLUDED.tenant_id`

// markMissingSQL — 이번 조회에 없던 행만. vendor_status가 이미 표식이면 건드리지 않는다
// (updated_at이 매 동기화마다 흔들리면 "언제 사라졌는지"를 잃는다).
const markMissingSQL = `
	UPDATE alimtalk_templates
	   SET status = 'rejected', vendor_status = $4, updated_at = $5
	 WHERE tenant_id = $1 AND app_id = $2 AND sender_id = $3
	   AND NOT (template_code = ANY($6))
	   AND vendor_status <> $4`

func (s *pgStore) Upsert(ctx context.Context, scope Scope, rows []Row, now time.Time) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	return s.inTx(ctx, func(tx pgx.Tx) (int, error) {
		n := 0
		for _, r := range rows {
			tag, err := tx.Exec(ctx, upsertSQL,
				scope.TenantID, scope.AppID, scope.SenderID, r.Code, r.Name, r.Content,
				r.MessageType, r.EmphasizeType, r.Variables, r.Buttons, r.QuickReplies,
				r.Status, r.VendorStatus, now.UTC())
			if err != nil {
				return n, fmt.Errorf("템플릿 %s upsert: %w", r.Code, err)
			}
			n += int(tag.RowsAffected())
		}
		return n, nil
	})
}

func (s *pgStore) MarkMissing(ctx context.Context, scope Scope, presentCodes []string, now time.Time) (int, error) {
	if len(presentCodes) == 0 {
		// 빈 목록으로 전량을 사라진 것으로 만들지 않는다 — 호출자도 막지만 여기서도 막는다.
		return 0, nil
	}
	return s.inTx(ctx, func(tx pgx.Tx) (int, error) {
		tag, err := tx.Exec(ctx, markMissingSQL,
			scope.TenantID, scope.AppID, scope.SenderID, VendorStatusMissing, now.UTC(), presentCodes)
		if err != nil {
			return 0, fmt.Errorf("사라진 템플릿 표시: %w", err)
		}
		return int(tag.RowsAffected()), nil
	})
}

func (s *pgStore) inTx(ctx context.Context, fn func(pgx.Tx) (int, error)) (int, error) {
	if s.pg == nil {
		return 0, fmt.Errorf("templatesync: pg 미주입")
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("트랜잭션 시작: %w", err)
	}
	n, err := fn(tx)
	if err != nil {
		_ = tx.Rollback(ctx)
		return n, err
	}
	if err := tx.Commit(ctx); err != nil {
		return n, fmt.Errorf("커밋: %w", err)
	}
	return n, nil
}

// normalizeStatus — 벤더가 상태를 못 주면 컬럼 기본값과 같은 'unknown'.
// 빈 문자열을 그대로 넣으면 ValidateSend가 "승인되지 않은 템플릿(status=)"이라는
// 읽을 수 없는 메시지를 내고, 콘솔 필터도 빈 값으로 갈라진다.
func normalizeStatus(s alimtalk.TemplateStatus) string {
	if s == "" {
		return "unknown"
	}
	return string(s)
}

func marshalJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	if string(b) == "null" {
		return "[]", nil // jsonb 컬럼 기본값과 같은 빈 배열
	}
	return string(b), nil
}
