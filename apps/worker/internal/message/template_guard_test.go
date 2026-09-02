package message

import (
	"context"
	"testing"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
	"github.com/ondahq/onda/apps/worker/internal/connector"
)

// 발송 직전 승인 템플릿 대조 (template_guard.go).
//
// 캐시본이 없을 때까지 실패로 만들면 아직 동기화하지 않은 테넌트의 정상 발송이 전량 멈춘다.
// 반대로 캐시본이 있는데 승인 상태가 아니면 반드시 막아야 한다 — 미승인 템플릿 발송은
// 공급자 거절로 끝나지 않고 카카오 채널 제재로 이어진다.

func guardWorker(t *testing.T) (*Worker, *alimtalk.Registry) {
	t.Helper()
	reg := testRegistry(t, manifest("t_variables", func(m *connector.Manifest) {
		m.Capabilities.Substitution = connector.SubstitutionVariables
	}))
	w, _ := newTestWorker(t, reg)
	return w, reg
}

func storedTemplate(status alimtalk.TemplateStatus) *alimtalk.Template {
	return &alimtalk.Template{
		Code: "TPL_1", Content: "#{name}님 주문이 접수됐습니다.", MessageType: "BA",
		Status: status, VendorStatus: "APR",
	}
}

func TestGuardApprovedTemplatePasses(t *testing.T) {
	w, reg := guardWorker(t)
	p := validPayload()
	p.Content.Template.Code = "TPL_1"
	job := jobFor(t, reg, "t_variables", p)
	job.Template = storedTemplate(alimtalk.TemplateApproved)

	if _, err := w.Send(context.Background(), testEnv(), job, channel.Credentials{}); err != nil {
		t.Fatalf("승인 템플릿은 통과해야 한다: %v", err)
	}
	if built["t_variables"].sends != 1 {
		t.Fatalf("벤더 발송 1회 기대, got %d", built["t_variables"].sends)
	}
}

func TestGuardUnapprovedTemplateFailsPermanent(t *testing.T) {
	for _, status := range []alimtalk.TemplateStatus{alimtalk.TemplatePending, alimtalk.TemplateRejected, "unknown"} {
		w, reg := guardWorker(t)
		before := built["t_variables"].sends
		p := validPayload()
		p.Content.Template.Code = "TPL_1"
		job := jobFor(t, reg, "t_variables", p)
		job.Template = storedTemplate(status)

		_, err := w.Send(context.Background(), testEnv(), job, channel.Credentials{})
		if err == nil {
			t.Fatalf("status=%s는 발송되면 안 된다", status)
		}
		if got := channel.Classify(err); got != channel.FailurePermanentContent {
			t.Fatalf("status=%s: permanent_content 기대, got %v", status, got)
		}
		if built["t_variables"].sends != before {
			t.Fatalf("status=%s: 벤더에 닿으면 안 된다", status)
		}
	}
}

// 미동기화(캐시본 없음)는 경고 후 진행 — 검증 캐시가 없다고 주문 알림을 막지 않는다.
func TestGuardMissingTemplateProceeds(t *testing.T) {
	w, reg := guardWorker(t)
	before := built["t_variables"].sends
	job := jobFor(t, reg, "t_variables", validPayload())
	job.Template = nil

	if _, err := w.Send(context.Background(), testEnv(), job, channel.Credentials{}); err != nil {
		t.Fatalf("미동기화는 발송을 막지 않는다: %v", err)
	}
	if built["t_variables"].sends != before+1 {
		t.Fatal("미동기화 발송이 벤더에 닿지 않았다")
	}
}

// 승인 본문에서 도출한 치환자가 비면 벤더에 가기 전에 끊는다 (빈 값 발송 = 사고).
func TestGuardMissingVariableFails(t *testing.T) {
	w, reg := guardWorker(t)
	p := validPayload()
	p.Content.Template.Code = "TPL_1"
	p.Content.Template.Variables = map[string]string{"other": "x"}
	job := jobFor(t, reg, "t_variables", p)
	job.Template = storedTemplate(alimtalk.TemplateApproved)

	_, err := w.Send(context.Background(), testEnv(), job, channel.Credentials{})
	if err == nil || channel.Classify(err) != channel.FailurePermanentContent {
		t.Fatalf("치환자 누락은 permanent_content여야 한다: %v", err)
	}
}

// pg가 없으면 조회 없이 nil — 테스트·미마이그레이션 배포에서 발송이 서지 않는다.
func TestLoadStoredTemplateWithoutPG(t *testing.T) {
	w, _ := guardWorker(t)
	if got := w.loadStoredTemplate(context.Background(), testEnv().TenantID, testEnv().AppID, "sk-1", "TPL_1"); got != nil {
		t.Fatalf("pg 미주입이면 nil이어야 한다: %+v", got)
	}
}

// 발신프로필 키는 발송 지정값 우선, 없으면 커넥터 배선 설정 — 템플릿 조회와 발송이 같은 답을 봐야 한다.
func TestSenderKeyForFallsBackToConfig(t *testing.T) {
	p := validPayload()
	job := &Job{P: &p, Config: []byte(`{"sender_key":"sk-config"}`)}
	if got := senderKeyFor(job); got != "sk-1" {
		t.Fatalf("발송 지정값 우선: got %q", got)
	}
	p.Content.Template.SenderKey = ""
	if got := senderKeyFor(job); got != "sk-config" {
		t.Fatalf("배선 설정으로 폴백해야 한다: got %q", got)
	}
}
