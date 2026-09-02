package nhn

import (
	"testing"

	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk/conformance"
)

// TestConformance — 계약 테스트 9종을 NHN 벤더에 돌린다.
//
// 실 계정이 없으므로 v2.3 스펙대로 응답하는 httptest 서버를 물린다. 실 계정이 생기면
// base_url과 키만 갈아 끼우면 같은 스위트가 그대로 돈다 — 조종은 수신번호로 하기 때문이다.
//
// callback_parse는 skip된다. NHN은 발송 결과 웹훅을 발행하지 않아 lifecycle_mode가
// polling이고, 스위트가 ParseCallback이 ErrUnsupported임을 확인한 뒤 건너뛴다.
// 그 skip이 "구현을 빼먹었다"가 아니라 "이 벤더에는 그 경로가 없다"임을 밝히는 것이
// skip 기제의 목적이다.
func TestConformance(t *testing.T) {
	v, _, base := newTestVendor(t)
	conformance.RunSuite(t, v, v.ConformanceEnv(base))
}
