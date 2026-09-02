package mock

import (
	"encoding/json"

	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk/conformance"
)

// ValidCredentialJSON — 계약 테스트·E2E가 쓰는 유효 크리덴셜.
func ValidCredentialJSON() []byte {
	b, _ := json.Marshal(credential{APIKey: "mock-api-key", SenderKey: "mock-sender-key"})
	return b
}

// InvalidCredentialJSON — 반드시 credential_auth로 떨어져야 하는 크리덴셜.
func InvalidCredentialJSON() []byte {
	b, _ := json.Marshal(credential{APIKey: InvalidAPIKey, SenderKey: "mock-sender-key"})
	return b
}

// Number — 조종 코드가 붙은 수신번호. 010-0000-XXXX 형태라 실제 번호와 섞이지 않는다.
func Number(suffix string) string { return "+8210" + "0000" + suffix }

// steering — 계약 테스트의 결과 요구를 조종표의 수신번호로 번역한다.
var steering = map[conformance.Outcome]string{
	conformance.OutcomeDelivered:        SuffixDelivered,
	conformance.OutcomeInvalidTarget:    SuffixInvalidTarget,
	conformance.OutcomeRateLimited:      SuffixRateLimited,
	conformance.OutcomePermanentContent: SuffixPermanentContent,
	conformance.OutcomeCredentialAuth:   SuffixCredentialAuth,
	conformance.OutcomeRetryable:        SuffixRetryable,
	conformance.OutcomeFallback:         SuffixFallback,
}

// ConformanceEnv — 이 벤더용 계약 테스트 하네스 재료.
// 다른 벤더(NHN·알리고)는 자기 계정으로 같은 모양의 Env를 만들어 같은 스위트를 돌린다.
func (v *Vendor) ConformanceEnv() conformance.Env {
	return conformance.Env{
		Valid:        alimtalk.Credential{ConnectorID: ConnectorID, JSON: ValidCredentialJSON()},
		Invalid:      alimtalk.Credential{ConnectorID: ConnectorID, JSON: InvalidCredentialJSON()},
		SenderKey:    "mock-sender-key",
		TemplateCode: TemplateOrder,
		Variables:    SampleVariables(TemplateOrder),
		RenderedText: SampleRendered(TemplateOrder),
		Buttons: []alimtalk.Button{
			{Type: "WL", Name: "주문 상세 보기", LinkMo: "https://m.example.com/orders", LinkPC: "https://example.com/orders"},
		},
		Fallback: &alimtalk.Fallback{
			Type: "SMS", Text: "주문이 접수되었습니다.", SenderNo: "0212345678",
		},
		Clock: v.clk,
		Target: func(o conformance.Outcome) (string, bool) {
			s, ok := steering[o]
			if !ok {
				return "", false
			}
			return Number(s), true
		},
		Callback: v.TerminalCallback,
	}
}
