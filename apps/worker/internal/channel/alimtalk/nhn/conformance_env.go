package nhn

import (
	"encoding/json"

	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk/conformance"
)

// 계약 테스트 하네스 재료.
//
// NHN에는 샌드박스 계정이 없고 실 계정도 아직 없으므로, 계약 테스트는 v2.3 스펙대로 응답하는
// httptest 서버를 물려 돈다(FakeServer, nhn_test.go 계열). 그래서 이 Env는 base_url을 인자로 받는다.
// 실 계정이 생기면 같은 Env에 진짜 엔드포인트와 진짜 키만 갈아 끼우면 된다 —
// 조종은 수신번호 끝 4자리로 하므로 벤더 코드도 하네스도 바뀌지 않는다.
const (
	ConfAppKey       = "conf-appkey"
	ConfSecretKey    = "conf-secret-key"
	ConfSenderKey    = "conf-sender-key"
	ConfTemplateCode = "ONDA_ORDER_01"
)

// 수신번호 끝 4자리 조종 코드. mock 벤더의 조종표와 같은 자리값을 쓴다.
//
//	0001 접수 성공 → 조회에서 COMPLETED/MRC01 (delivered)
//	0002 접수 성공 → 조회에서 FAILED/MRC02, 수신자 사유 (invalid_target)
//	0003 접수 응답의 sendResults[0].resultCode != 0, 수신자 사유 (동기 invalid_target)
//	0004 접수 응답의 sendResults[0].resultCode != 0, 본문 사유 (동기 permanent_content)
//	0010 resendParameter가 있으면 대체발송 성공(sent + DeliveredVia), 없으면 0002와 같다
//	0400 HTTP 400 + 본문 사유 (permanent_content)
//	0401 HTTP 200 + header.isSuccessful=false + 발신프로필 권한 사유 (credential_auth — 함정 케이스)
//	0403 HTTP 403 (credential_auth)
//	0410 접수 성공 → 조회 HTTP 404 (Expired)
//	0429 HTTP 429 + Retry-After (rate_limited)
//	0500 HTTP 500 (retryable)
const (
	SuffixDelivered           = "0001"
	SuffixInvalidTarget       = "0002"
	SuffixRecipientRejected   = "0003"
	SuffixRecipientBadContent = "0004"
	SuffixFallback            = "0010"
	SuffixPermanentContent    = "0400"
	SuffixAuthOn200           = "0401"
	SuffixForbidden           = "0403"
	SuffixExpired             = "0410"
	SuffixRateLimited         = "0429"
	SuffixRetryable           = "0500"
)

// Number — 조종 코드가 붙은 E.164 수신번호. 010-0000-XXXX라 실제 번호와 섞이지 않는다.
func Number(suffix string) string { return "+8210" + "0000" + suffix }

// CredentialJSON — 크리덴셜 JSON을 만든다. 테스트·부트스트랩용.
func CredentialJSON(appKey, secretKey, senderKey, baseURL string) []byte {
	b, _ := json.Marshal(credential{
		AppKey: appKey, SecretKey: secretKey, SenderKey: senderKey, BaseURL: baseURL,
	})
	return b
}

// ValidCredential — 계약 테스트가 통과해야 하는 크리덴셜.
func ValidCredential(baseURL string) alimtalk.Credential {
	return alimtalk.Credential{
		ConnectorID: ConnectorID,
		JSON:        CredentialJSON(ConfAppKey, ConfSecretKey, ConfSenderKey, baseURL),
	}
}

// InvalidCredential — 반드시 credential_auth로 떨어져야 하는 크리덴셜.
// 형식은 멀쩡하고 SecretKey만 틀렸다 — 형식 검증만으로는 못 걸러내고 실제 호출이 필요하다.
func InvalidCredential(baseURL string) alimtalk.Credential {
	return alimtalk.Credential{
		ConnectorID: ConnectorID,
		JSON:        CredentialJSON(ConfAppKey, "wrong-secret-key", ConfSenderKey, baseURL),
	}
}

// steering — 계약 테스트의 결과 요구를 조종표의 수신번호로 번역한다.
var steering = map[conformance.Outcome]string{
	conformance.OutcomeDelivered:        SuffixDelivered,
	conformance.OutcomeInvalidTarget:    SuffixInvalidTarget,
	conformance.OutcomeRateLimited:      SuffixRateLimited,
	conformance.OutcomePermanentContent: SuffixPermanentContent,
	conformance.OutcomeCredentialAuth:   SuffixForbidden,
	conformance.OutcomeRetryable:        SuffixRetryable,
	conformance.OutcomeFallback:         SuffixFallback,
}

// ConformanceEnv — 이 벤더용 계약 테스트 하네스 재료. baseURL은 v2.3 스펙대로 응답하는 서버 주소다.
//
// Callback은 채우지 않는다. NHN은 결과 웹훅이 없어 callback_parse가 skip되는 것이 정상이고,
// 종결 확인은 전부 PollResults 경로로 간다.
func (v *Vendor) ConformanceEnv(baseURL string) conformance.Env {
	return conformance.Env{
		Valid:        ValidCredential(baseURL),
		Invalid:      InvalidCredential(baseURL),
		SenderKey:    ConfSenderKey,
		TemplateCode: ConfTemplateCode,
		Variables: map[string]string{
			"고객명":  "홍길동",
			"주문번호": "20260902-0001",
			"결제금액": "38,000",
		},
		// RenderedText는 substitution이 rendered|both인 벤더에만 실린다.
		// NHN은 variables라 하네스가 싣지 않지만, 값이 있어야 대조가 의미 있어 채워 둔다.
		RenderedText: "홍길동님, 주문 20260902-0001이 정상 접수되었습니다.",
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
	}
}
