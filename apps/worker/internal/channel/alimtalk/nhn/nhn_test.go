package nhn

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk/conformance"
	"github.com/ondahq/onda/apps/worker/internal/clock"
	"github.com/ondahq/onda/apps/worker/internal/connector"
)

// 실 NHN 계정이 없으므로 모든 테스트는 httptest 서버를 문다.
// api.nhncloudservice.com·api-alimtalk.cloud.toast.com로 나가는 호출은 이 패키지에 하나도 없다.

func fixedClock() *clock.Fake {
	return &clock.Fake{Current: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}
}

func newTestVendor(t *testing.T) (*Vendor, *fakeNHN, string) {
	t.Helper()
	f, base := newFakeNHN(t)
	m, err := EmbeddedManifest()
	if err != nil {
		t.Fatalf("내장 manifest: %v", err)
	}
	v, err := NewWithClock(m, fixedClock(), nil)
	if err != nil {
		t.Fatalf("NewWithClock: %v", err)
	}
	return v, f, base
}

func sendReq(base, suffix string) alimtalk.SendRequest {
	return alimtalk.SendRequest{
		MessageID:      "msg-" + suffix,
		IdempotencyKey: "j1:v1:u1:n1:e1",
		Credential:     ValidCredential(base),
		SenderKey:      ConfSenderKey,
		TemplateCode:   ConfTemplateCode,
		To:             Number(suffix),
		Variables:      map[string]string{"고객명": "홍길동", "주문번호": "20260902-0001"},
	}
}

func TestEmbeddedManifest(t *testing.T) {
	m, err := EmbeddedManifest()
	if err != nil {
		t.Fatalf("내장 manifest 파싱: %v", err)
	}
	if m.ID != ConnectorID {
		t.Fatalf("id: want %q, got %q", ConnectorID, m.ID)
	}
	if m.Channel != alimtalk.ChannelID {
		t.Fatalf("channel: want %q, got %q", alimtalk.ChannelID, m.Channel)
	}
	if m.SubstitutionMode() != "variables" {
		t.Fatalf("substitution: want variables, got %q", m.SubstitutionMode())
	}
	if !m.NeedsPolling() || m.NeedsCallback() {
		t.Fatalf("lifecycle_mode가 polling이어야 한다 (got %q)", m.Mode())
	}
	if m.Capabilities.BatchMax != MaxRecipients {
		t.Fatalf("batch_max: want %d, got %d", MaxRecipients, m.Capabilities.BatchMax)
	}
	if !m.Capabilities.VendorFallback || !m.Capabilities.AsyncReceipt {
		t.Fatal("vendor_fallback·async_receipt가 모두 true여야 한다")
	}
	// 알림톡은 열람·클릭을 보고하지 않는다. 선언해 두면 리포트가 "미지원"과 "0"을 구분하지 못한다.
	for _, s := range []string{"opened", "clicked"} {
		if m.Reports(s) {
			t.Fatalf("lifecycle.reports에 %q가 있으면 안 된다", s)
		}
	}
	for _, s := range []string{"accepted", "sent", "delivered", "failed"} {
		if !m.Reports(s) {
			t.Fatalf("lifecycle.reports에 %q가 있어야 한다", s)
		}
	}
	if !reflect.DeepEqual(m.ContractTests, conformance.IDs()) {
		t.Fatalf("contract_tests가 스위트와 다르다:\n want %v\n got  %v", conformance.IDs(), m.ContractTests)
	}
	if m.Compliance == nil || m.Compliance.MarketingAllowed == nil || *m.Compliance.MarketingAllowed {
		t.Fatal("compliance.marketing_allowed는 명시적 false여야 한다")
	}
}

// TestSendRequestShape — v2.3 요청이 스펙 그대로 나가는가.
// 경로·인증 헤더·멱등 헤더·본문 필드를 한 번에 본다.
func TestSendRequestShape(t *testing.T) {
	v, f, base := newTestVendor(t)
	req := sendReq(base, SuffixDelivered)
	req.Buttons = []alimtalk.Button{
		{Type: "WL", Name: "주문 상세", LinkMo: "https://m.example.com/o", LinkPC: "https://example.com/o"},
		{Type: "AL", Name: "앱에서 보기", LinkIOS: "ondaapp://o", LinkAndroid: "ondaapp://o"},
	}
	req.Fallback = &alimtalk.Fallback{Type: "LMS", Title: "주문 안내", Text: "주문이 접수되었습니다.", SenderNo: "0212345678"}
	// NHN은 templateParameter로 직접 렌더한다. 완성 본문이 실려도 요청에 나가면 안 된다.
	req.RenderedText = "홍길동님, 주문 20260902-0001이 정상 접수되었습니다."

	if _, err := v.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if want := "/alimtalk/" + APIVersion + "/appkeys/" + ConfAppKey + "/messages"; f.LastSendPath != want {
		t.Fatalf("경로: want %q, got %q", want, f.LastSendPath)
	}
	if got := f.LastSendHeader.Get(SecretKeyHeader); got != ConfSecretKey {
		t.Fatalf("%s: want %q, got %q", SecretKeyHeader, ConfSecretKey, got)
	}
	if got := f.LastSendHeader.Get(IdempotencyHeader); got != req.MessageID {
		t.Fatalf("%s는 message_id여야 한다: want %q, got %q", IdempotencyHeader, req.MessageID, got)
	}
	if ct := f.LastSendHeader.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type: got %q", ct)
	}

	var body map[string]any
	if err := json.Unmarshal(f.LastSendBody, &body); err != nil {
		t.Fatalf("본문 파싱: %v (%s)", err, f.LastSendBody)
	}
	if body["senderKey"] != ConfSenderKey || body["templateCode"] != ConfTemplateCode {
		t.Fatalf("senderKey/templateCode: %v", body)
	}
	if _, has := body["requestDate"]; has {
		t.Fatal("예약이 아닌데 requestDate가 실렸다")
	}
	if strings.Contains(string(f.LastSendBody), "정상 접수되었습니다") {
		t.Fatal("완성 본문(RenderedText)이 요청에 실렸다 — NHN은 templateParameter로 직접 렌더한다")
	}
	list, _ := body["recipientList"].([]any)
	if len(list) != 1 {
		t.Fatalf("recipientList 길이: %v", body["recipientList"])
	}
	rc, _ := list[0].(map[string]any)
	if rc["recipientNo"] != "0100000"+SuffixDelivered {
		t.Fatalf("recipientNo가 국내 표기로 정규화되지 않았다: %v", rc["recipientNo"])
	}
	tp, _ := rc["templateParameter"].(map[string]any)
	if tp["고객명"] != "홍길동" || tp["주문번호"] != "20260902-0001" {
		t.Fatalf("templateParameter: %v", rc["templateParameter"])
	}
	if rc["recipientGroupingKey"] != req.MessageID {
		t.Fatalf("recipientGroupingKey에 message_id가 실려야 한다: %v", rc["recipientGroupingKey"])
	}

	btns, _ := rc["buttons"].([]any)
	if len(btns) != 2 {
		t.Fatalf("buttons 길이: %v", rc["buttons"])
	}
	b0, _ := btns[0].(map[string]any)
	if b0["ordering"].(float64) != 1 || b0["linkMo"] != "https://m.example.com/o" || b0["linkPc"] != "https://example.com/o" {
		t.Fatalf("WL 버튼 매핑: %v", b0)
	}
	b1, _ := btns[1].(map[string]any)
	// 우리 LinkIOS/LinkAndroid는 NHN에서 schemeIos/schemeAndroid다. 이름이 달라 놓치기 쉽다.
	if b1["schemeIos"] != "ondaapp://o" || b1["schemeAndroid"] != "ondaapp://o" {
		t.Fatalf("AL 버튼의 schemeIos/schemeAndroid 매핑: %v", b1)
	}
	if _, has := b1["linkIos"]; has {
		t.Fatalf("우리 필드명이 그대로 나갔다: %v", b1)
	}

	rp, _ := rc["resendParameter"].(map[string]any)
	if rp == nil || rp["isResend"] != true || rp["resendType"] != "LMS" ||
		rp["resendContent"] != "주문이 접수되었습니다." || rp["resendSendNo"] != "0212345678" ||
		rp["resendTitle"] != "주문 안내" {
		t.Fatalf("resendParameter: %v", rc["resendParameter"])
	}
}

func TestSendOmitsResendWhenNoFallback(t *testing.T) {
	v, f, base := newTestVendor(t)
	if _, err := v.Send(context.Background(), sendReq(base, SuffixDelivered)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.Contains(string(f.LastSendBody), "resendParameter") {
		t.Fatalf("Fallback이 없는데 resendParameter가 실렸다: %s", f.LastSendBody)
	}
}

// TestReceiptCompositeRoundTrip — requestId+recipientSeq를 한 문자열로 접고 되펴는 왕복.
func TestReceiptCompositeRoundTrip(t *testing.T) {
	cases := []struct {
		requestID string
		seq       int
	}{
		{"req-00000001", 1},
		{"0-abcdef-uuid-like", 42},
		{"has:colon:inside", 7}, // 마지막 콜론 기준이라 견딘다
	}
	for _, c := range cases {
		id := EncodeReceiptID(c.requestID, c.seq)
		gotID, gotSeq, err := DecodeReceiptID(id)
		if err != nil {
			t.Fatalf("%q 되펴기 실패: %v", id, err)
		}
		if gotID != c.requestID || gotSeq != c.seq {
			t.Fatalf("왕복 불일치: want (%q,%d), got (%q,%d)", c.requestID, c.seq, gotID, gotSeq)
		}
	}
	for _, bad := range []string{"", "nocolon", ":5", "req-1:", "req-1:abc"} {
		if _, _, err := DecodeReceiptID(bad); err == nil {
			t.Fatalf("%q는 오류여야 한다", bad)
		}
	}
}

func TestSendReceiptEncodesComposite(t *testing.T) {
	v, _, base := newTestVendor(t)
	r, err := v.Send(context.Background(), sendReq(base, SuffixDelivered))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	reqID, seq, err := DecodeReceiptID(r.ProviderMessageID)
	if err != nil {
		t.Fatalf("Receipt가 복합키를 담지 않았다 (%q): %v", r.ProviderMessageID, err)
	}
	if reqID == "" || seq != 1 {
		t.Fatalf("복합키: got (%q,%d)", reqID, seq)
	}
	if r.MessageID != "msg-"+SuffixDelivered {
		t.Fatalf("MessageID 왕복: got %q", r.MessageID)
	}
	if !r.AcceptedAt.Equal(fixedClock().Now()) {
		t.Fatalf("AcceptedAt이 주입 시계를 따르지 않는다: %v", r.AcceptedAt)
	}
}

// TestSendErrorClasses — 오류 → FailureClass 표. credential_auth-on-200이 핵심이다.
func TestSendErrorClasses(t *testing.T) {
	cases := []struct {
		name   string
		suffix string
		want   channel.FailureClass
	}{
		{"HTTP 400 본문 반려", SuffixPermanentContent, channel.FailurePermanentContent},
		{"HTTP 200 + isSuccessful=false + 발신프로필 권한", SuffixAuthOn200, channel.FailureCredentialAuth},
		{"HTTP 403", SuffixForbidden, channel.FailureCredentialAuth},
		{"HTTP 429", SuffixRateLimited, channel.FailureRateLimited},
		{"HTTP 500", SuffixRetryable, channel.FailureRetryable},
		{"수신자별 거절(수신번호 사유)", SuffixRecipientRejected, channel.FailureInvalidTarget},
		{"수신자별 거절(본문 사유)", SuffixRecipientBadContent, channel.FailurePermanentContent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, _, base := newTestVendor(t)
			_, err := v.Send(context.Background(), sendReq(base, c.suffix))
			if err == nil {
				t.Fatal("오류여야 하는데 접수됐다")
			}
			if got := v.Classify(err); got != c.want {
				t.Fatalf("분류: want %s, got %s (%v)", c.want, got, err)
			}
			if c.want == channel.FailureRateLimited {
				if d := channel.RetryAfterOf(err); d != 3*time.Second {
					t.Fatalf("Retry-After를 살려야 한다: want 3s, got %v", d)
				}
			}
		})
	}
}

func TestSendRateLimitWithoutRetryAfterHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"header": failHeader(-1, "한도 초과")})
	}))
	defer srv.Close()
	v := vendorFor(t)
	_, err := v.Send(context.Background(), sendReq(srv.URL, SuffixDelivered))
	if got := v.Classify(err); got != channel.FailureRateLimited {
		t.Fatalf("분류: want rate_limited, got %s (%v)", got, err)
	}
	// 0을 흘리면 공급자가 준 대기 시간을 버리는 셈이라 계약 테스트가 양수를 요구한다.
	if d := channel.RetryAfterOf(err); d != defaultRateLimitRetryAfter {
		t.Fatalf("Retry-After 기본값: want %v, got %v", defaultRateLimitRetryAfter, d)
	}
}

func TestSendCredentialErrors(t *testing.T) {
	v, _, base := newTestVendor(t)
	cases := []struct {
		name string
		json []byte
	}{
		{"app_key 누락", CredentialJSON("", ConfSecretKey, ConfSenderKey, base)},
		{"secret_key 누락", CredentialJSON(ConfAppKey, "", ConfSenderKey, base)},
		{"secret_key 오류(HTTP 401)", CredentialJSON(ConfAppKey, "wrong", ConfSenderKey, base)},
		{"크리덴셜 비어 있음", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := sendReq(base, SuffixDelivered)
			req.Credential = alimtalk.Credential{ConnectorID: ConnectorID, JSON: c.json}
			_, err := v.Send(context.Background(), req)
			if got := v.Classify(err); got != channel.FailureCredentialAuth {
				t.Fatalf("분류: want credential_auth, got %s (%v)", got, err)
			}
		})
	}
}

// TestSendMissingSenderKeyIsCredentialAuth — 발신프로필 키 부재는 permanent_content가 아니다.
// permanent_content로 흘리면 channel.Verifier.judge가 그걸 "인증 통과"로 읽는다.
func TestSendMissingSenderKeyIsCredentialAuth(t *testing.T) {
	v, _, base := newTestVendor(t)
	req := sendReq(base, SuffixDelivered)
	req.SenderKey = ""
	req.Credential = alimtalk.Credential{
		ConnectorID: ConnectorID,
		JSON:        CredentialJSON(ConfAppKey, ConfSecretKey, "", base),
	}
	_, err := v.Send(context.Background(), req)
	if got := v.Classify(err); got != channel.FailureCredentialAuth {
		t.Fatalf("분류: want credential_auth, got %s (%v)", got, err)
	}
}

func TestSendTransportErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close() // 닫힌 포트 → 전송 실패
	v := vendorFor(t)
	_, err := v.Send(context.Background(), sendReq(base, SuffixDelivered))
	if got := v.Classify(err); got != channel.FailureRetryable {
		t.Fatalf("분류: want retryable, got %s (%v)", got, err)
	}
}

func TestSendRejectsUndeclaredContent(t *testing.T) {
	v, f, base := newTestVendor(t)
	req := sendReq(base, SuffixDelivered)
	req.QuickReplies = []alimtalk.QuickReply{{Type: "WL", Name: "바로연결", LinkMo: "https://m.example.com/q"}}
	_, err := v.Send(context.Background(), req)
	if got := v.Classify(err); got != channel.FailurePermanentContent {
		t.Fatalf("미선언 content는 permanent_content여야 한다 (got %s): %v", got, err)
	}
	if f.SendCalls != 0 {
		t.Fatal("선언하지 않은 content를 공급자에게 보냈다 — 스스로 막아야 한다")
	}
}

func TestSendValidatesFields(t *testing.T) {
	v, _, base := newTestVendor(t)
	t.Run("message_id 누락", func(t *testing.T) {
		req := sendReq(base, SuffixDelivered)
		req.MessageID = ""
		if _, err := v.Send(context.Background(), req); v.Classify(err) != channel.FailurePermanentContent {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("템플릿 코드 20자 초과", func(t *testing.T) {
		req := sendReq(base, SuffixDelivered)
		req.TemplateCode = strings.Repeat("A", 21)
		if _, err := v.Send(context.Background(), req); v.Classify(err) != channel.FailurePermanentContent {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("수신번호 없음", func(t *testing.T) {
		req := sendReq(base, SuffixDelivered)
		req.To = ""
		if _, err := v.Send(context.Background(), req); v.Classify(err) != channel.FailureInvalidTarget {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("대체발송 발신번호 없음", func(t *testing.T) {
		req := sendReq(base, SuffixDelivered)
		req.Fallback = &alimtalk.Fallback{Type: "SMS", Text: "본문"}
		if _, err := v.Send(context.Background(), req); v.Classify(err) != channel.FailurePermanentContent {
			t.Fatalf("got %v", err)
		}
	})
}

// TestSendScheduled — 예약 발송은 KST "yyyy-MM-dd HH:mm"으로 나간다.
func TestSendScheduled(t *testing.T) {
	v, f, base := newTestVendor(t)
	req := sendReq(base, SuffixDelivered)
	at := time.Date(2026, 9, 3, 1, 30, 0, 0, time.UTC) // KST 10:30
	req.ScheduledAt = &at
	if _, err := v.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(f.LastSendBody, &body)
	if body["requestDate"] != "2026-09-03 10:30" {
		t.Fatalf("requestDate(KST): got %v", body["requestDate"])
	}

	t.Run("60일 초과는 거절", func(t *testing.T) {
		far := fixedClock().Now().AddDate(0, 0, MaxScheduleDaysAhead+1)
		req := sendReq(base, SuffixDelivered)
		req.ScheduledAt = &far
		if _, err := v.Send(context.Background(), req); v.Classify(err) != channel.FailurePermanentContent {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("과거는 거절", func(t *testing.T) {
		past := fixedClock().Now().Add(-time.Hour)
		req := sendReq(base, SuffixDelivered)
		req.ScheduledAt = &past
		if _, err := v.Send(context.Background(), req); v.Classify(err) != channel.FailurePermanentContent {
			t.Fatalf("got %v", err)
		}
	})
}

// TestIdempotentResend — 같은 message_id로 두 번 보내면 같은 복합 식별자여야 한다.
func TestIdempotentResend(t *testing.T) {
	v, f, base := newTestVendor(t)
	req := sendReq(base, SuffixDelivered)
	first, err := v.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("1차: %v", err)
	}
	second, err := v.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("2차: %v", err)
	}
	if first.ProviderMessageID != second.ProviderMessageID {
		t.Fatalf("재발송 식별자가 갈렸다: %q vs %q", first.ProviderMessageID, second.ProviderMessageID)
	}
	if f.SendCalls != 2 {
		t.Fatalf("두 번 다 공급자에 갔어야 한다 (멱등은 공급자 몫): got %d", f.SendCalls)
	}
}

func TestParseCallbackUnsupported(t *testing.T) {
	v, _, _ := newTestVendor(t)
	evs, err := v.ParseCallback(context.Background(), alimtalk.RawCallback{ConnectorID: ConnectorID, Body: []byte("{}")})
	if !errors.Is(err, alimtalk.ErrUnsupported) {
		t.Fatalf("NHN은 결과 웹훅이 없다 — ErrUnsupported여야 한다 (got %v)", err)
	}
	if evs != nil {
		t.Fatalf("이벤트를 내면 안 된다: %v", evs)
	}
}

func TestValidate(t *testing.T) {
	v, f, base := newTestVendor(t)
	if err := v.Validate(context.Background(), ValidCredential(base)); err != nil {
		t.Fatalf("유효 크리덴셜: %v", err)
	}
	if f.TemplateCalls == 0 {
		t.Fatal("Validate가 실제 호출 없이 통과했다 — 무해한 읽기로 실검증해야 한다")
	}
	err := v.Validate(context.Background(), InvalidCredential(base))
	if got := v.Classify(err); got != channel.FailureCredentialAuth {
		t.Fatalf("무효 크리덴셜: want credential_auth, got %s (%v)", got, err)
	}
	t.Run("발신프로필 키 부재", func(t *testing.T) {
		cred := alimtalk.Credential{ConnectorID: ConnectorID, JSON: CredentialJSON(ConfAppKey, ConfSecretKey, "", base)}
		err := v.Validate(context.Background(), cred)
		if got := v.Classify(err); got != channel.FailureCredentialAuth {
			t.Fatalf("want credential_auth, got %s (%v)", got, err)
		}
		if !strings.Contains(err.Error(), "발신프로필") {
			t.Fatalf("한국어 사유가 분명해야 한다: %v", err)
		}
	})
	t.Run("등록되지 않은 발신프로필은 200+실패로 온다", func(t *testing.T) {
		cred := alimtalk.Credential{ConnectorID: ConnectorID, JSON: CredentialJSON(ConfAppKey, ConfSecretKey, "남의-발신키", base)}
		err := v.Validate(context.Background(), cred)
		if got := v.Classify(err); got != channel.FailureCredentialAuth {
			t.Fatalf("want credential_auth, got %s (%v)", got, err)
		}
	})
}

func TestNormalizeRecipient(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"+821012345678", "01012345678", true},
		{"+82 10-1234-5678", "01012345678", true},
		{"01012345678", "01012345678", true},
		{"010-1234-5678", "01012345678", true},
		{"+14155550100", "14155550100", true},
		{"", "", false},
		{"+8210123456789012345", "", false},
	}
	for _, c := range cases {
		got, err := normalizeRecipient(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Fatalf("%q: want %q, got %q (%v)", c.in, c.want, got, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("%q는 오류여야 한다 (got %q)", c.in, got)
		}
	}
}

func TestClassifyMessage(t *testing.T) {
	cases := []struct {
		msg  string
		want channel.FailureClass
	}{
		{"등록되지 않은 발신프로필입니다", channel.FailureCredentialAuth},
		{"유효하지 않은 SecretKey입니다", channel.FailureCredentialAuth},
		{"Unauthorized", channel.FailureCredentialAuth},
		{"요청 한도를 초과했습니다", channel.FailureRateLimited},
		{"일시적인 시스템 오류입니다", channel.FailureRetryable},
		{"수신번호가 올바르지 않습니다", channel.FailureInvalidTarget},
		{"템플릿 본문이 승인 내용과 일치하지 않습니다", channel.FailurePermanentContent},
		{"", channel.FailurePermanentContent},
	}
	for _, c := range cases {
		if got := classifyMessage(0, c.msg, channel.FailurePermanentContent); got != c.want {
			t.Fatalf("%q: want %s, got %s", c.msg, c.want, got)
		}
	}
	// 알려진 코드는 낱말보다 앞선다.
	if got := classifyMessage(-9999, "무슨 말인지 모를 메시지", channel.FailurePermanentContent); got != channel.FailureRetryable {
		t.Fatalf("코드 표가 우선해야 한다: got %s", got)
	}
	// "recipient"의 부분 문자열 때문에 인증으로 오분류되면 안 된다(과거 ip 낱말 사고).
	if got := classifyMessage(0, "recipient number is invalid", channel.FailurePermanentContent); got != channel.FailureInvalidTarget {
		t.Fatalf("got %s", got)
	}
}

// vendorFor — 서버 없이 벤더만 필요한 테스트용.
func vendorFor(t *testing.T) *Vendor {
	t.Helper()
	m, err := EmbeddedManifest()
	if err != nil {
		t.Fatalf("내장 manifest: %v", err)
	}
	v, err := NewWithClock(m, fixedClock(), nil)
	if err != nil {
		t.Fatalf("NewWithClock: %v", err)
	}
	return v
}

func TestNewWithClockRejectsBadWiring(t *testing.T) {
	m := embeddedOrDie(t)
	if _, err := NewWithClock(m, nil, nil); err == nil {
		t.Fatal("clock이 nil이면 오류여야 한다")
	}
	bad := m
	bad.ID = "alimtalk_other"
	if _, err := NewWithClock(bad, fixedClock(), nil); err == nil {
		t.Fatal("id가 다르면 오류여야 한다")
	}
	bad = m
	bad.Channel = "sms"
	if _, err := NewWithClock(bad, fixedClock(), nil); err == nil {
		t.Fatal("channel이 다르면 오류여야 한다")
	}
	// manifest 없이도(빈 값) 내장 manifest로 자립해야 한다 — 부트스트랩이 쓴다.
	if _, err := NewWithClock(connector.Manifest{}, fixedClock(), nil); err != nil {
		t.Fatalf("빈 manifest는 내장으로 대체돼야 한다: %v", err)
	}
}

func embeddedOrDie(t *testing.T) connector.Manifest {
	t.Helper()
	m, err := EmbeddedManifest()
	if err != nil {
		t.Fatalf("내장 manifest: %v", err)
	}
	return m
}

// TestRegisteredInRegistry — init()의 alimtalk.Register가 실제로 걸렸는가.
func TestRegisteredInRegistry(t *testing.T) {
	reg, err := alimtalk.NewRegistry([]connector.Manifest{embeddedOrDie(t)})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, err := reg.Get(ConnectorID)
	if err != nil {
		t.Fatalf("Get(%s): %v", ConnectorID, err)
	}
	if got.Manifest().ID != ConnectorID {
		t.Fatalf("manifest id: got %q", got.Manifest().ID)
	}
}

// TestTimeoutFromManifest — HTTP 타임아웃은 manifest.runtime.timeout_ms에서 온다.
func TestTimeoutFromManifest(t *testing.T) {
	m := embeddedOrDie(t)
	if m.Runtime.TimeoutMS <= 0 {
		t.Fatal("manifest에 runtime.timeout_ms가 있어야 한다")
	}
	if got := timeoutOf(m); got != time.Duration(m.Runtime.TimeoutMS)*time.Millisecond {
		t.Fatalf("타임아웃: got %v", got)
	}
	if got := timeoutOf(connector.Manifest{}); got != defaultTimeout {
		t.Fatalf("기본 타임아웃: got %v", got)
	}
}
