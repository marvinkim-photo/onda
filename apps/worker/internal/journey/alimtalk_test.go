package journey

import "testing"

func TestNormalizeKoreanPhone(t *testing.T) {
	cases := []struct{ in, want string }{
		{"01012345678", "+821012345678"},
		{"010-1234-5678", "+821012345678"},
		{"010 1234 5678", "+821012345678"},
		{"+821012345678", "+821012345678"},
		{"+82 10 1234 5678", "+821012345678"},
		{"821012345678", "+821012345678"},
		{"8201012345678", "+821012345678"},
		{"0212345678", "+82212345678"},
		{"  01012345678  ", "+821012345678"},
		// 판단할 수 없는 입력은 발송하지 않는다 — 잘못된 대상에 보내는 것보다 안 보내는 게 낫다.
		{"", ""},
		{"1012345678", ""},     // 국가·국내 접두 없음
		{"010", ""},            // 너무 짧음
		{"01012345678901", ""}, // 너무 김
		{"+1", ""},
		{"없음", ""},
	}
	for _, c := range cases {
		if got := normalizeKoreanPhone(c.in); got != c.want {
			t.Errorf("normalizeKoreanPhone(%q) = %q, 기대 %q", c.in, got, c.want)
		}
	}
}

// endpoint_id는 번호에서 결정적으로 유도돼야 한다 — 멱등 키의 마지막 요소이므로
// 같은 번호가 재시도마다 다른 값이면 중복 발송이 난다.
func TestPhoneEndpointIDIsDeterministic(t *testing.T) {
	a := phoneEndpointID("+821012345678")
	b := phoneEndpointID("+821012345678")
	if a != b {
		t.Fatalf("같은 번호에서 다른 endpoint_id: %s vs %s", a, b)
	}
	if c := phoneEndpointID("+821012345679"); c == a {
		t.Fatalf("다른 번호가 같은 endpoint_id를 냈다: %s", c)
	}
	if len(a) != 36 {
		t.Fatalf("endpoint_id가 UUID 형식이 아니다: %q", a)
	}
}

func TestPolicyLabels(t *testing.T) {
	if quietDecisionLabel(false) != "bypassed_transactional" {
		t.Error("정보성 알림톡은 야간 제한 예외로 표기돼야 한다")
	}
	if quietDecisionLabel(true) != "allowed" {
		t.Error("광고성은 정상 판정으로 표기돼야 한다")
	}
	if capLabel(false) != "bypassed_transactional" {
		t.Error("정보성은 빈도 상한 대상이 아니다")
	}
}
