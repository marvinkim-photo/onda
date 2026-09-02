// Package conformance — 알림톡 벤더 계약 테스트 스위트.
//
// 이 스위트는 "벤더 플러그인을 받아줄지" 판정하는 관문이다. 특정 벤더에 묶이지 않고
// 어떤 alimtalk.Vendor 구현체에도 돌릴 수 있어야 하므로, 벤더마다 다른 것(유효/무효
// 크리덴셜, 결과를 조종하는 수신번호, 콜백 원문)은 전부 Env로 주입받는다.
//
// 판정 규칙: manifest가 지원하지 않는다고 선언한 능력의 케이스는 사유를 남기고 skip하고,
// skip되지 않은 케이스가 전부 통과하면 그 벤더는 계약을 만족한다.
// manifest.contract_tests에 선언된 id와 Suite()의 id는 일치해야 한다(RunSuite가 대조).
package conformance

import (
	"context"
	"testing"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
	"github.com/ondahq/onda/apps/worker/internal/clock"
	"github.com/ondahq/onda/apps/worker/internal/connector"
)

// Outcome — 하네스가 벤더를 몰아가야 하는 결과. Env.Target이 이걸 수신번호로 번역한다.
type Outcome string

const (
	OutcomeDelivered        Outcome = "delivered"
	OutcomeInvalidTarget    Outcome = "invalid_target"
	OutcomeRateLimited      Outcome = "rate_limited"
	OutcomePermanentContent Outcome = "permanent_content"
	OutcomeCredentialAuth   Outcome = "credential_auth"
	OutcomeRetryable        Outcome = "retryable"
	// OutcomeFallback — 알림톡은 실패하지만 대체발송으로 살아나는 대상.
	OutcomeFallback Outcome = "fallback"
)

// Env — 하네스가 공급해야 하는 벤더별 재료.
type Env struct {
	// Valid — 실제로 통과해야 하는 크리덴셜. 모든 케이스의 SendRequest.Credential에 실린다.
	Valid alimtalk.Credential
	// Invalid — 반드시 credential_auth로 떨어져야 하는 크리덴셜.
	Invalid alimtalk.Credential

	SenderKey    string
	TemplateCode string
	// Variables — TemplateCode 본문의 치환자를 빠짐없이 채운 값.
	Variables map[string]string
	// RenderedText — substitution이 rendered|both인 벤더용 완성 본문.
	RenderedText string
	// Buttons — manifest가 buttons를 선언한 벤더에만 실린다.
	Buttons []alimtalk.Button
	// Fallback — vendor_fallback을 선언한 벤더의 대체발송 설정.
	Fallback *alimtalk.Fallback

	Clock clock.Clock

	// Target — 원하는 결과로 벤더를 몰아가는 수신번호. 그 결과를 만들 수 없으면 false를
	// 돌려주고, 해당 케이스는 skip된다.
	Target func(Outcome) (string, bool)

	// Callback — 이 접수들에 대해 공급자가 보냈을 법한 웹훅 원문.
	// 콜백 수명주기를 지원하는 벤더만 채운다.
	Callback func([]alimtalk.Receipt) (alimtalk.RawCallback, bool)
}

// Case — 계약 테스트 한 건. ID는 manifest.contract_tests의 값과 같다.
type Case struct {
	ID  string
	Run func(t *testing.T, v alimtalk.Vendor, env Env)
}

// Suite — 계약 테스트 9종. 순서는 의존이 아니라 읽기 편한 순서다.
func Suite() []Case {
	return []Case{
		{ID: "validate_credentials", Run: caseValidateCredentials},
		{ID: "send_ok", Run: caseSendOK},
		{ID: "send_invalid_target", Run: caseSendInvalidTarget},
		{ID: "send_rate_limited", Run: caseSendRateLimited},
		{ID: "send_permanent_content", Run: caseSendPermanentContent},
		{ID: "callback_parse", Run: caseCallbackParse},
		{ID: "idempotent_resend", Run: caseIdempotentResend},
		{ID: "unsupported_content", Run: caseUnsupportedContent},
		{ID: "fallback_trigger", Run: caseFallbackTrigger},
	}
}

// IDs — 스위트가 구현하는 계약 테스트 id (Suite 순서).
func IDs() []string {
	out := make([]string, 0, len(Suite()))
	for _, c := range Suite() {
		out = append(out, c.ID)
	}
	return out
}

// RunSuite — 전체 스위트를 하나의 벤더에 돌린다.
func RunSuite(t *testing.T, v alimtalk.Vendor, env Env) {
	t.Helper()
	m := v.Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("벤더가 내놓은 manifest 자체가 무효다: %v", err)
	}
	if m.Channel != alimtalk.ChannelID {
		t.Fatalf("manifest.channel이 %q여야 한다 (got %q)", alimtalk.ChannelID, m.Channel)
	}
	if env.Target == nil {
		t.Fatalf("Env.Target이 없다 — 하네스가 결과를 조종할 방법을 줘야 한다")
	}
	if env.TemplateCode == "" {
		t.Fatalf("Env.TemplateCode가 없다")
	}

	declared := map[string]bool{}
	for _, id := range m.ContractTests {
		declared[id] = true
	}
	implemented := map[string]bool{}
	for _, c := range Suite() {
		implemented[c.ID] = true
	}
	for _, id := range m.ContractTests {
		if !implemented[id] {
			t.Errorf("manifest가 선언한 contract_test %q가 스위트에 없다", id)
		}
	}
	for _, c := range Suite() {
		if !declared[c.ID] {
			t.Errorf("manifest.contract_tests에 %q가 빠졌다 — 9종은 모두 선언해야 한다", c.ID)
		}
	}

	for _, c := range Suite() {
		t.Run(c.ID, func(t *testing.T) { c.Run(t, v, env) })
	}
}

// request — 케이스 공통 발송 요청. manifest가 선언한 것만 싣는다.
func (e Env) request(caseID string, m connector.Manifest) alimtalk.SendRequest {
	req := alimtalk.SendRequest{
		MessageID:      "conf-" + caseID,
		IdempotencyKey: "conf-" + caseID,
		Credential:     e.Valid,
		SenderKey:      e.SenderKey,
		TemplateCode:   e.TemplateCode,
		Variables:      e.Variables,
	}
	if mode := m.SubstitutionMode(); mode == connector.SubstitutionRendered || mode == connector.SubstitutionBoth {
		req.RenderedText = e.RenderedText
	}
	if len(e.Buttons) > 0 && declaresContent(m, "buttons") {
		req.Buttons = e.Buttons
	}
	return req
}

func declaresContent(m connector.Manifest, feature string) bool {
	for _, c := range m.Capabilities.Content {
		if c == feature {
			return true
		}
	}
	return false
}

// target — 원하는 결과의 수신번호를 얻거나, 못 얻으면 케이스를 skip한다.
func target(t *testing.T, env Env, o Outcome) string {
	t.Helper()
	to, ok := env.Target(o)
	if !ok || to == "" {
		t.Skipf("이 벤더는 %s 결과로 몰아갈 수 없다 — 하네스가 Env.Target에서 미지원으로 선언했다", o)
	}
	return to
}

// terminalOf — 접수 하나의 종결 이벤트를 얻는다. 폴링을 지원하면 폴링으로,
// 아니면 콜백 원문으로 확인한다. 둘 다 없으면 skip.
func terminalOf(ctx context.Context, t *testing.T, v alimtalk.Vendor, env Env, r alimtalk.Receipt) alimtalk.Event {
	t.Helper()
	m := v.Manifest()
	if m.NeedsPolling() {
		evs, err := v.PollResults(ctx, env.Valid, []alimtalk.Receipt{r})
		if err != nil {
			t.Fatalf("PollResults 실패: %v", err)
		}
		if ev, ok := pick(evs, r.ProviderMessageID); ok {
			if !ev.Terminal {
				t.Fatalf("PollResults가 %s를 종결로 표시하지 않았다 (status=%s)", r.ProviderMessageID, ev.Status)
			}
			return ev
		}
		t.Fatalf("PollResults가 %s의 결과를 주지 않았다 (%d건 반환)", r.ProviderMessageID, len(evs))
	}
	if m.NeedsCallback() {
		if env.Callback == nil {
			t.Skipf("Env.Callback이 없어 콜백 경로로 종결을 확인할 수 없다")
		}
		cb, ok := env.Callback([]alimtalk.Receipt{r})
		if !ok {
			t.Skipf("하네스가 %s의 콜백 원문을 만들지 못했다", r.ProviderMessageID)
		}
		evs, err := v.ParseCallback(ctx, cb)
		if err != nil {
			t.Fatalf("ParseCallback 실패: %v", err)
		}
		if ev, ok := pick(evs, r.ProviderMessageID); ok {
			// 폴링 경로와 같은 기준으로 본다. 같은 발송이 경로에 따라 종결되기도 하고
			// 영원히 대기하기도 하면 미종결 접수가 조용히 쌓인다.
			if !ev.Terminal {
				t.Fatalf("종결 콜백인데 %s를 종결로 표시하지 않았다 (status=%s)", r.ProviderMessageID, ev.Status)
			}
			return ev
		}
		t.Fatalf("ParseCallback이 %s의 이벤트를 주지 않았다 (%d건)", r.ProviderMessageID, len(evs))
	}
	t.Skipf("lifecycle_mode=%s — 종결 이벤트를 얻을 경로가 없다", m.Mode())
	return alimtalk.Event{}
}

func pick(evs []alimtalk.Event, providerMessageID string) (alimtalk.Event, bool) {
	for _, ev := range evs {
		if ev.ProviderMessageID == providerMessageID {
			return ev, true
		}
	}
	return alimtalk.Event{}, false
}

// sendExpectingFailure — 발송이 실패해야 하는 케이스의 공통 골격.
func sendExpectingFailure(ctx context.Context, t *testing.T, v alimtalk.Vendor, req alimtalk.SendRequest, want channel.FailureClass) error {
	t.Helper()
	r, err := v.Send(ctx, req)
	if err == nil {
		t.Fatalf("%s여야 하는데 Send가 접수를 반환했다 (provider_message_id=%s)", want, r.ProviderMessageID)
	}
	if got := v.Classify(err); got != want {
		t.Fatalf("분류가 %s여야 한다 (got %s): %v", want, got, err)
	}
	return err
}
