package mock

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// fixedClock — 결정적 테스트용 고정 시계. CLAUDE.md 규칙 3.
func fixedClock() *clock.Fake {
	return &clock.Fake{Current: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}
}

// validCred — 손으로 만드는 SendRequest에 실을 유효 크리덴셜.
// Send가 크리덴셜을 검사하므로(무효 키가 permanent_content로 새면 크리덴셜 정지가 안 걸린다)
// 발송 테스트는 전부 이걸 싣는다.
func validCred() alimtalk.Credential {
	return alimtalk.Credential{ConnectorID: ConnectorID, JSON: ValidCredentialJSON()}
}

func newVendor(t *testing.T) *Vendor {
	t.Helper()
	m, err := EmbeddedManifest()
	if err != nil {
		t.Fatalf("내장 manifest: %v", err)
	}
	v, err := NewWithClock(m, fixedClock())
	if err != nil {
		t.Fatalf("NewWithClock: %v", err)
	}
	return v
}

// TestConformance — 참조 구현이 계약 스위트 9종을 통과하는지.
// 이 테스트가 초록이면 스위트 자체도 살아 있다는 뜻이라, NHN·알리고가 붙을 때
// 실패의 원인이 벤더인지 스위트인지 구분할 수 있다.
func TestConformance(t *testing.T) {
	v := newVendor(t)
	conformance.RunSuite(t, v, v.ConformanceEnv())
}

func TestEmbeddedManifestIsValid(t *testing.T) {
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
	if m.SubstitutionMode() != connector.SubstitutionBoth {
		t.Fatalf("substitution: want both, got %q", m.SubstitutionMode())
	}
	if m.Mode() != connector.LifecycleBoth {
		t.Fatalf("lifecycle_mode: want both, got %q", m.Mode())
	}
	if !m.NeedsCallback() || !m.NeedsPolling() {
		t.Fatal("both 모드는 콜백·폴링을 모두 요구해야 한다")
	}
	if !m.Capabilities.VendorFallback {
		t.Fatal("vendor_fallback이 true여야 한다")
	}
	if m.Lifecycle.Callback == nil || m.Lifecycle.Callback.Path == "" || m.Lifecycle.Callback.Verify == "" {
		t.Fatal("lifecycle.callback의 path·verify가 있어야 한다")
	}
	want := conformance.IDs()
	if len(m.ContractTests) != len(want) {
		t.Fatalf("contract_tests 개수: want %d, got %d", len(want), len(m.ContractTests))
	}
	have := map[string]bool{}
	for _, id := range m.ContractTests {
		have[id] = true
	}
	for _, id := range want {
		if !have[id] {
			t.Fatalf("contract_tests에 %q가 없다", id)
		}
	}
}

// TestRegisteredInRegistry — init()의 자기 등록과 레지스트리 해석이 이어지는지.
func TestRegisteredInRegistry(t *testing.T) {
	m, err := EmbeddedManifest()
	if err != nil {
		t.Fatalf("내장 manifest: %v", err)
	}
	reg, err := alimtalk.NewRegistry([]connector.Manifest{m})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	v, err := reg.Get(ConnectorID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v.Manifest().ID != ConnectorID {
		t.Fatalf("Manifest().ID: got %q", v.Manifest().ID)
	}
	if got := reg.IDs(); len(got) != 1 || got[0] != ConnectorID {
		t.Fatalf("IDs: got %v", got)
	}
}

func TestSteeringTable(t *testing.T) {
	v := newVendor(t)
	ctx := context.Background()
	base := func(id string) alimtalk.SendRequest {
		return alimtalk.SendRequest{
			MessageID:    id,
			Credential:   validCred(),
			SenderKey:    "mock-sender-key",
			TemplateCode: TemplateOrder,
			Variables:    SampleVariables(TemplateOrder),
		}
	}
	cases := []struct {
		suffix    string
		wantClass channel.FailureClass
		accepted  bool
	}{
		{SuffixDelivered, channel.FailureNone, true},
		{SuffixInvalidTarget, channel.FailureNone, true},
		{SuffixFallback, channel.FailureNone, true},
		{SuffixPermanentContent, channel.FailurePermanentContent, false},
		{SuffixCredentialAuth, channel.FailureCredentialAuth, false},
		{SuffixRateLimited, channel.FailureRateLimited, false},
		{SuffixRetryable, channel.FailureRetryable, false},
		{"7777", channel.FailureNone, true}, // 미지정 번호는 해피패스
	}
	for _, tc := range cases {
		t.Run(tc.suffix, func(t *testing.T) {
			req := base("steer-" + tc.suffix)
			req.To = Number(tc.suffix)
			r, err := v.Send(ctx, req)
			if tc.accepted {
				if err != nil {
					t.Fatalf("접수돼야 하는데 실패했다: %v", err)
				}
				if r.AcceptedAt != v.clk.Now() {
					t.Fatalf("AcceptedAt이 주입 시계와 다르다: %v", r.AcceptedAt)
				}
				return
			}
			if err == nil {
				t.Fatalf("실패해야 하는데 접수됐다: %s", r.ProviderMessageID)
			}
			if got := v.Classify(err); got != tc.wantClass {
				t.Fatalf("분류: want %s, got %s", tc.wantClass, got)
			}
		})
	}
}

func TestRateLimitCarriesRetryAfter(t *testing.T) {
	v := newVendor(t)
	_, err := v.Send(context.Background(), alimtalk.SendRequest{
		MessageID:    "rl",
		Credential:   validCred(),
		SenderKey:    "mock-sender-key",
		TemplateCode: TemplateOrder,
		Variables:    SampleVariables(TemplateOrder),
		To:           Number(SuffixRateLimited),
	})
	if got := channel.RetryAfterOf(err); got != RateLimitRetryAfter {
		t.Fatalf("Retry-After: want %v, got %v", RateLimitRetryAfter, got)
	}
}

func TestFallbackSteering(t *testing.T) {
	v := newVendor(t)
	ctx := context.Background()
	req := alimtalk.SendRequest{
		MessageID:    "fb",
		Credential:   validCred(),
		SenderKey:    "mock-sender-key",
		TemplateCode: TemplateOrder,
		Variables:    SampleVariables(TemplateOrder),
		To:           Number(SuffixFallback),
	}
	cred := alimtalk.Credential{ConnectorID: ConnectorID, JSON: ValidCredentialJSON()}

	bare, err := v.Send(ctx, req)
	if err != nil {
		t.Fatalf("Fallback 없는 발송: %v", err)
	}
	evs, err := v.PollResults(ctx, cred, []alimtalk.Receipt{bare})
	if err != nil || len(evs) != 1 {
		t.Fatalf("PollResults: %v (%d건)", err, len(evs))
	}
	if evs[0].Status != "failed" || evs[0].FailureClass != channel.FailureInvalidTarget.String() {
		t.Fatalf("Fallback 없으면 invalid_target으로 실패해야 한다: %+v", evs[0])
	}

	req.MessageID = "fb2"
	req.Fallback = &alimtalk.Fallback{Type: "SMS", Text: "대체발송", SenderNo: "0212345678"}
	saved, err := v.Send(ctx, req)
	if err != nil {
		t.Fatalf("Fallback 발송: %v", err)
	}
	evs, err = v.PollResults(ctx, cred, []alimtalk.Receipt{saved})
	if err != nil || len(evs) != 1 {
		t.Fatalf("PollResults: %v (%d건)", err, len(evs))
	}
	if evs[0].Status != "sent" || evs[0].FailureClass != "" {
		t.Fatalf("Fallback이 실리면 대체발송으로 살아나야 한다: %+v", evs[0])
	}
	if evs[0].DeliveredVia != ChannelSMS {
		t.Fatalf("SMS 대체발송은 DeliveredVia가 %q여야 한다: %+v", ChannelSMS, evs[0])
	}
	if evs[0].CostCurrency != "KRW" || evs[0].CostAmount != FallbackCostSMS {
		t.Fatalf("대체발송은 SMS 단가로 잡혀야 한다: %+v", evs[0])
	}

	// LMS로 대체하면 도달 채널과 단가가 함께 바뀐다.
	req.MessageID = "fb3"
	req.Fallback = &alimtalk.Fallback{Type: "LMS", Title: "주문", Text: "주문이 접수되었습니다.", SenderNo: "0212345678"}
	long, err := v.Send(ctx, req)
	if err != nil {
		t.Fatalf("LMS 대체발송: %v", err)
	}
	evs, err = v.PollResults(ctx, cred, []alimtalk.Receipt{long})
	if err != nil || len(evs) != 1 {
		t.Fatalf("PollResults: %v (%d건)", err, len(evs))
	}
	if evs[0].DeliveredVia != ChannelLMS || evs[0].CostAmount != FallbackCostLMS {
		t.Fatalf("LMS 대체발송은 채널·단가가 모두 LMS여야 한다: %+v", evs[0])
	}
}

// TestExpiredSteering — 조회 보존 기간을 넘긴 접수는 Terminal이 아니라 Expired다.
// 폴러가 이 둘을 섞으면 결과를 모르는 건이 성공이나 실패로 집계된다.
func TestExpiredSteering(t *testing.T) {
	v := newVendor(t)
	ctx := context.Background()
	r, err := v.Send(ctx, alimtalk.SendRequest{
		MessageID:    "expired",
		Credential:   validCred(),
		SenderKey:    "mock-sender-key",
		TemplateCode: TemplateOrder,
		Variables:    SampleVariables(TemplateOrder),
		To:           Number(SuffixExpired),
	})
	if err != nil {
		t.Fatalf("접수돼야 한다: %v", err)
	}
	evs, err := v.PollResults(ctx, alimtalk.Credential{JSON: ValidCredentialJSON()}, []alimtalk.Receipt{r})
	if err != nil || len(evs) != 1 {
		t.Fatalf("PollResults: %v (%d건)", err, len(evs))
	}
	ev := evs[0]
	if !ev.Expired {
		t.Fatalf("Expired여야 한다: %+v", ev)
	}
	if ev.Terminal {
		t.Fatalf("Expired는 Terminal이 아니다 — 결과가 확정된 게 아니라 못 알아낸 것이다: %+v", ev)
	}
	if ev.Status != "" || ev.CostAmount != 0 {
		t.Fatalf("결과를 모르는 건에 상태·원가를 붙이면 집계가 오염된다: %+v", ev)
	}
	// 공급자가 "결과를 잊었다"를 웹훅으로 밀어주는 일은 없다.
	if _, ok := v.TerminalCallback([]alimtalk.Receipt{r}); ok {
		t.Fatal("Expired 접수는 콜백 원문에 실리면 안 된다")
	}
}

func TestValidateCredential(t *testing.T) {
	v := newVendor(t)
	ctx := context.Background()
	cases := []struct {
		name string
		cred alimtalk.Credential
		ok   bool
	}{
		{"정상", alimtalk.Credential{ConnectorID: ConnectorID, JSON: ValidCredentialJSON()}, true},
		{"무효 키", alimtalk.Credential{JSON: InvalidCredentialJSON()}, false},
		{"빈 크리덴셜", alimtalk.Credential{}, false},
		{"api_key 누락", alimtalk.Credential{JSON: []byte(`{"sender_key":"s"}`)}, false},
		{"sender_key 누락", alimtalk.Credential{JSON: []byte(`{"api_key":"k"}`)}, false},
		{"JSON 깨짐", alimtalk.Credential{JSON: []byte(`{`)}, false},
		{"다른 커넥터", alimtalk.Credential{ConnectorID: "kakao_alimtalk_nhn", JSON: ValidCredentialJSON()}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := v.Validate(ctx, tc.cred)
			if tc.ok {
				if err != nil {
					t.Fatalf("통과해야 한다: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("실패해야 한다")
			}
			if got := v.Classify(err); got != channel.FailureCredentialAuth {
				t.Fatalf("credential_auth여야 한다 (got %s): %v", got, err)
			}
		})
	}
}

func TestParseCallback(t *testing.T) {
	v := newVendor(t)
	ctx := context.Background()
	id := ProviderMessageID("cb-one", outcomeDelivered)

	t.Run("단건", func(t *testing.T) {
		body := []byte(`{"provider_message_id":"` + id + `","message_id":"cb-one","status":"delivered","occurred_at":"2026-09-02T11:00:00Z"}`)
		evs, err := v.ParseCallback(ctx, alimtalk.RawCallback{ConnectorID: ConnectorID, Body: body})
		if err != nil {
			t.Fatalf("ParseCallback: %v", err)
		}
		if len(evs) != 1 {
			t.Fatalf("1건이어야 한다 (got %d)", len(evs))
		}
		if !evs[0].Terminal || evs[0].Status != "delivered" {
			t.Fatalf("delivered는 종결이어야 한다: %+v", evs[0])
		}
		if !evs[0].OccurredAt.Equal(time.Date(2026, 9, 2, 11, 0, 0, 0, time.UTC)) {
			t.Fatalf("occurred_at을 그대로 써야 한다: %v", evs[0].OccurredAt)
		}
	})

	t.Run("배열", func(t *testing.T) {
		body := []byte(`[{"provider_message_id":"a","status":"sent"},{"provider_message_id":"b","status":"failed"}]`)
		evs, err := v.ParseCallback(ctx, alimtalk.RawCallback{Body: body})
		if err != nil {
			t.Fatalf("ParseCallback: %v", err)
		}
		if len(evs) != 2 {
			t.Fatalf("2건이어야 한다 (got %d)", len(evs))
		}
		if evs[0].Terminal {
			t.Fatal("sent는 종결이 아니다")
		}
		if !evs[1].Terminal || evs[1].FailureClass != channel.FailureInvalidTarget.String() {
			t.Fatalf("failed는 종결 + 분류가 있어야 한다: %+v", evs[1])
		}
		if !evs[0].OccurredAt.Equal(v.clk.Now()) {
			t.Fatalf("occurred_at 누락 시 주입 시계를 써야 한다: %v", evs[0].OccurredAt)
		}
	})

	t.Run("거부", func(t *testing.T) {
		bad := []alimtalk.RawCallback{
			{Body: []byte(``)},
			{Body: []byte(`{`)},
			{Body: []byte(`{"status":"delivered"}`)}, // provider_message_id 누락
			{Body: []byte(`{"provider_message_id":"a","status":"read"}`)}, // 보고하지 않는 상태
			{Body: []byte(`{"provider_message_id":"a","status":"sent","occurred_at":"어제"}`)},
			{ConnectorID: "kakao_alimtalk_nhn", Body: []byte(`{"provider_message_id":"a","status":"sent"}`)},
		}
		for i, cb := range bad {
			if _, err := v.ParseCallback(ctx, cb); err == nil {
				t.Fatalf("%d번 콜백은 거부돼야 한다: %s", i, cb.Body)
			}
		}
	})
}

func TestPollResultsSkipsForeignReceipts(t *testing.T) {
	v := newVendor(t)
	cred := alimtalk.Credential{JSON: ValidCredentialJSON()}
	evs, err := v.PollResults(context.Background(), cred, []alimtalk.Receipt{
		{ProviderMessageID: "nhn:req-1:0", MessageID: "x"},
		{ProviderMessageID: ProviderMessageID("mine", outcomeDelivered), MessageID: "mine"},
	})
	if err != nil {
		t.Fatalf("PollResults: %v", err)
	}
	if len(evs) != 1 || evs[0].MessageID != "mine" {
		t.Fatalf("남의 접수는 건너뛰어야 한다: %+v", evs)
	}
}

func TestPollResultsRejectsBadCredential(t *testing.T) {
	v := newVendor(t)
	_, err := v.PollResults(context.Background(), alimtalk.Credential{JSON: InvalidCredentialJSON()}, nil)
	if got := v.Classify(err); got != channel.FailureCredentialAuth {
		t.Fatalf("credential_auth여야 한다 (got %s): %v", got, err)
	}
}

func TestListTemplates(t *testing.T) {
	v := newVendor(t)
	cred := alimtalk.Credential{JSON: ValidCredentialJSON()}
	ts, err := v.ListTemplates(context.Background(), cred, "mock-sender-key")
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(ts) != 3 {
		t.Fatalf("픽스처 3건이어야 한다 (got %d)", len(ts))
	}
	byCode := map[string]alimtalk.Template{}
	for _, tm := range ts {
		byCode[tm.Code] = tm
		if tm.Status != alimtalk.TemplateApproved {
			t.Fatalf("%s: 승인 상태여야 한다", tm.Code)
		}
		if len(alimtalk.Variables(tm.Content)) == 0 {
			t.Fatalf("%s: 치환자가 있어야 렌더 경로가 걸린다", tm.Code)
		}
		if err := alimtalk.ValidateButtons(tm.Buttons, tm.IsAd()); err != nil {
			t.Fatalf("%s: 픽스처 버튼이 카카오 규칙을 어긴다: %v", tm.Code, err)
		}
	}
	if got := byCode[TemplateOrder]; got.MessageType != "BA" || got.IsAd() || got.Category() != "transactional" {
		t.Fatalf("%s는 정보성 BA여야 한다: %+v", TemplateOrder, got)
	}
	if got := byCode[TemplatePromo]; got.MessageType != "AD" || !got.IsAd() || got.Category() != "marketing" {
		t.Fatalf("%s는 광고성 AD여야 한다: %+v", TemplatePromo, got)
	}
	if _, err := v.ListTemplates(context.Background(), cred, "다른키"); err == nil {
		t.Fatal("등록되지 않은 발신프로필은 거부해야 한다")
	}
	if _, err := v.ListTemplates(context.Background(), alimtalk.Credential{JSON: InvalidCredentialJSON()}, ""); err == nil {
		t.Fatal("무효 크리덴셜은 거부해야 한다")
	}
}

func TestSampleHelpers(t *testing.T) {
	vars := SampleVariables(TemplatePromo)
	for _, name := range alimtalk.Variables(fixtures(time.Time{})[2].Content) {
		if vars[name] == "" {
			t.Fatalf("치환자 %q가 비었다", name)
		}
	}
	if SampleVariables("없는코드") != nil {
		t.Fatal("모르는 코드는 nil이어야 한다")
	}
	if SampleRendered(TemplateOrder) == "" {
		t.Fatal("SampleRendered가 비었다")
	}
	if got := SampleRendered("없는코드"); got != "" {
		t.Fatalf("모르는 코드는 빈 문자열이어야 한다: %q", got)
	}
}

func TestUnsupportedCapabilitiesRejected(t *testing.T) {
	v := newVendor(t)
	ctx := context.Background()
	base := alimtalk.SendRequest{
		MessageID:    "caps",
		Credential:   validCred(),
		SenderKey:    "mock-sender-key",
		TemplateCode: TemplateOrder,
		Variables:    SampleVariables(TemplateOrder),
		To:           Number(SuffixDelivered),
	}
	at := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)

	quick := base
	quick.QuickReplies = []alimtalk.QuickReply{{Type: "WL", Name: "문의", LinkMo: "https://m.example.com"}}
	if _, err := v.Send(ctx, quick); channel.Classify(err) != channel.FailurePermanentContent {
		t.Fatalf("미선언 quick_replies는 permanent_content여야 한다: %v", err)
	}

	sched := base
	sched.ScheduledAt = &at
	if _, err := v.Send(ctx, sched); channel.Classify(err) != channel.FailurePermanentContent {
		t.Fatalf("미선언 예약발송은 permanent_content여야 한다: %v", err)
	}

	unknown := base
	unknown.TemplateCode = "NOPE"
	if _, err := v.Send(ctx, unknown); channel.Classify(err) != channel.FailurePermanentContent {
		t.Fatalf("미승인 템플릿은 permanent_content여야 한다: %v", err)
	}

	noMsgID := base
	noMsgID.MessageID = ""
	if _, err := v.Send(ctx, noMsgID); err == nil {
		t.Fatal("message_id 없는 발송은 거부해야 한다")
	}

	missingVar := base
	missingVar.Variables = map[string]string{"고객명": "홍길동"}
	if _, err := v.Send(ctx, missingVar); channel.Classify(err) != channel.FailurePermanentContent {
		t.Fatalf("치환자 누락은 permanent_content여야 한다: %v", err)
	}
}

func TestProviderMessageIDIsDeterministic(t *testing.T) {
	if a, b := ProviderMessageID("msg-ABC-123", outcomeDelivered), ProviderMessageID("msg-ABC-123", outcomeDelivered); a != b {
		t.Fatalf("같은 입력에 다른 값: %q vs %q", a, b)
	}
	if got := ProviderMessageID("msg-ABC-123", outcomeDelivered); got != "mock_msgabc12_dl" {
		t.Fatalf("형식이 바뀌었다: %q", got)
	}
	if got := ProviderMessageID("---", outcomeInvalidTarget); got != "mock_00000000_it" {
		t.Fatalf("영숫자가 없으면 0으로 채워야 한다: %q", got)
	}
}

func TestSuffix(t *testing.T) {
	cases := map[string]string{
		"+821000000001":  "0001",
		"010-0000-0429":  "0429",
		"":               "",
		"12":             "",
		"no-digits-here": "",
	}
	for in, want := range cases {
		if got := Suffix(in); got != want {
			t.Fatalf("Suffix(%q): want %q, got %q", in, want, got)
		}
	}
}

func TestNewWithClockRejectsWrongManifest(t *testing.T) {
	if _, err := NewWithClock(connector.Manifest{}, nil); err == nil {
		t.Fatal("nil 시계는 거부해야 한다")
	}
	if _, err := NewWithClock(connector.Manifest{ID: "other", Channel: alimtalk.ChannelID}, fixedClock()); err == nil {
		t.Fatal("다른 id의 manifest는 거부해야 한다")
	}
	if _, err := NewWithClock(connector.Manifest{ID: ConnectorID, Channel: "email"}, fixedClock()); err == nil {
		t.Fatal("다른 채널의 manifest는 거부해야 한다")
	}
	v, err := NewWithClock(connector.Manifest{}, fixedClock())
	if err != nil || v.Manifest().ID != ConnectorID {
		t.Fatalf("빈 manifest는 내장 manifest로 채워야 한다: %v", err)
	}
}

func TestCredentialJSONMatchesSchema(t *testing.T) {
	// manifest.credentials.schema의 required가 실제 파서와 어긋나면 콘솔 폼이 거짓말을 한다.
	m, err := EmbeddedManifest()
	if err != nil {
		t.Fatalf("내장 manifest: %v", err)
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(m.Credentials.Schema, &schema); err != nil {
		t.Fatalf("credentials.schema 파싱: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(ValidCredentialJSON(), &got); err != nil {
		t.Fatalf("크리덴셜 파싱: %v", err)
	}
	for _, name := range schema.Required {
		if _, ok := got[name]; !ok {
			t.Fatalf("schema가 요구하는 %q가 크리덴셜에 없다", name)
		}
		if _, ok := schema.Properties[name]; !ok {
			t.Fatalf("schema.required의 %q가 properties에 없다", name)
		}
	}
}

// variant — 내장 manifest를 능력만 깎아 복제한다. 스위트의 skip 경로를 태우기 위한 것으로,
// 실제 벤더(폴링 전용 NHN, 대체발송 없는 딜러사)가 붙을 때 그대로 걸리는 길이다.
func variant(t *testing.T, mutate func(*connector.Manifest)) *Vendor {
	t.Helper()
	m, err := EmbeddedManifest()
	if err != nil {
		t.Fatalf("내장 manifest: %v", err)
	}
	mutate(&m)
	if err := m.Validate(); err != nil {
		t.Fatalf("변형 manifest가 무효다: %v", err)
	}
	v, err := NewWithClock(m, fixedClock())
	if err != nil {
		t.Fatalf("NewWithClock: %v", err)
	}
	return v
}

// TestConformanceSkipsForUnsupportedCapabilities — 미지원 능력의 케이스가 실패가 아니라
// skip으로 빠지는지. 스위트가 "NHN 기준"으로만 통과하면 알리고에서 무너진다.
func TestConformanceSkipsForUnsupportedCapabilities(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*connector.Manifest)
	}{
		{"폴링 전용", func(m *connector.Manifest) {
			m.Capabilities.LifecycleMode = connector.LifecyclePolling
			m.Lifecycle.Callback = nil
		}},
		{"콜백 전용", func(m *connector.Manifest) {
			m.Capabilities.LifecycleMode = connector.LifecycleCallback
		}},
		{"대체발송 미지원", func(m *connector.Manifest) {
			m.Capabilities.VendorFallback = false
		}},
		{"content 전량 선언", func(m *connector.Manifest) {
			m.Capabilities.Content = []string{"template", "buttons", "quick_replies"}
		}},
		{"완성본문 요구", func(m *connector.Manifest) {
			m.Capabilities.Substitution = connector.SubstitutionRendered
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := variant(t, tc.mutate)
			conformance.RunSuite(t, v, v.ConformanceEnv())
		})
	}
}

// TestUnsupportedLifecyclePathsReturnErrUnsupported — 선언하지 않은 수명주기 경로는
// 조용한 no-op이 아니라 ErrUnsupported여야 한다. 폴러·콜백 라우터가 이걸로 분기한다.
func TestUnsupportedLifecyclePathsReturnErrUnsupported(t *testing.T) {
	ctx := context.Background()
	pollOnly := variant(t, func(m *connector.Manifest) {
		m.Capabilities.LifecycleMode = connector.LifecyclePolling
		m.Lifecycle.Callback = nil
	})
	if _, err := pollOnly.ParseCallback(ctx, alimtalk.RawCallback{Body: []byte(`{}`)}); !errors.Is(err, alimtalk.ErrUnsupported) {
		t.Fatalf("폴링 전용 벤더의 ParseCallback: want ErrUnsupported, got %v", err)
	}
	if _, ok := pollOnly.TerminalCallback(nil); ok {
		t.Fatal("폴링 전용 벤더는 콜백 원문을 만들지 않아야 한다")
	}

	cbOnly := variant(t, func(m *connector.Manifest) {
		m.Capabilities.LifecycleMode = connector.LifecycleCallback
	})
	if _, err := cbOnly.PollResults(ctx, alimtalk.Credential{JSON: ValidCredentialJSON()}, nil); !errors.Is(err, alimtalk.ErrUnsupported) {
		t.Fatalf("콜백 전용 벤더의 PollResults: want ErrUnsupported, got %v", err)
	}
}

// TestRenderedOnlyVendorRequiresRenderedText — substitution=rendered 벤더는
// 완성 본문 없이 접수하면 안 된다(알리고 message_N 계열).
func TestRenderedOnlyVendorRequiresRenderedText(t *testing.T) {
	v := variant(t, func(m *connector.Manifest) { m.Capabilities.Substitution = connector.SubstitutionRendered })
	req := alimtalk.SendRequest{
		MessageID:    "rendered",
		Credential:   validCred(),
		SenderKey:    "mock-sender-key",
		TemplateCode: TemplateOrder,
		Variables:    SampleVariables(TemplateOrder),
		To:           Number(SuffixDelivered),
	}
	if _, err := v.Send(context.Background(), req); channel.Classify(err) != channel.FailurePermanentContent {
		t.Fatalf("완성 본문 없는 발송은 permanent_content여야 한다: %v", err)
	}
	req.RenderedText = SampleRendered(TemplateOrder)
	if _, err := v.Send(context.Background(), req); err != nil {
		t.Fatalf("완성 본문이 있으면 접수돼야 한다: %v", err)
	}
}

// TestSendRejectsBadCredential — 조종표가 성공을 가리켜도 크리덴셜이 틀리면 접수되면 안 된다.
// 분류가 credential_auth여야 워커가 크리덴셜을 error로 전환하고 앱 발송을 멈춘다.
func TestSendRejectsBadCredential(t *testing.T) {
	v := newVendor(t)
	base := alimtalk.SendRequest{
		MessageID:    "badcred",
		SenderKey:    "mock-sender-key",
		TemplateCode: TemplateOrder,
		Variables:    SampleVariables(TemplateOrder),
		To:           Number(SuffixDelivered),
	}
	for name, cred := range map[string]alimtalk.Credential{
		"누락":     {},
		"무효 키":   {JSON: InvalidCredentialJSON()},
		"다른 커넥터": {ConnectorID: "kakao_alimtalk_nhn", JSON: ValidCredentialJSON()},
	} {
		t.Run(name, func(t *testing.T) {
			req := base
			req.Credential = cred
			r, err := v.Send(context.Background(), req)
			if err == nil {
				t.Fatalf("접수되면 안 된다: %s", r.ProviderMessageID)
			}
			if got := v.Classify(err); got != channel.FailureCredentialAuth {
				t.Fatalf("credential_auth여야 한다 (got %s): %v", got, err)
			}
		})
	}
}

// TestPollAndCallbackAgree — 같은 접수를 폴링으로 받든 콜백으로 받든 결론이 같아야 한다.
// 갈리면 lifecycle_mode=both 벤더에서 한 발송이 경로에 따라 다르게 집계된다.
func TestPollAndCallbackAgree(t *testing.T) {
	v := newVendor(t)
	ctx := context.Background()
	cred := alimtalk.Credential{JSON: ValidCredentialJSON()}

	send := func(id, suffix string, fb *alimtalk.Fallback) alimtalk.Receipt {
		t.Helper()
		r, err := v.Send(ctx, alimtalk.SendRequest{
			MessageID:    id,
			Credential:   validCred(),
			SenderKey:    "mock-sender-key",
			TemplateCode: TemplateOrder,
			Variables:    SampleVariables(TemplateOrder),
			To:           Number(suffix),
			Fallback:     fb,
		})
		if err != nil {
			t.Fatalf("%s 발송: %v", id, err)
		}
		return r
	}

	cases := []struct {
		name     string
		receipt  alimtalk.Receipt
		wantVia  string
		wantStat string
	}{
		{"도달", send("agree-dl", SuffixDelivered, nil), "", "delivered"},
		{"차단", send("agree-it", SuffixInvalidTarget, nil), "", "failed"},
		{"SMS 대체발송", send("agree-fs", SuffixFallback, &alimtalk.Fallback{Type: "SMS", Text: "x", SenderNo: "021"}), ChannelSMS, "sent"},
		{"LMS 대체발송", send("agree-fl", SuffixFallback, &alimtalk.Fallback{Type: "LMS", Text: "x", SenderNo: "021"}), ChannelLMS, "sent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			polled, err := v.PollResults(ctx, cred, []alimtalk.Receipt{tc.receipt})
			if err != nil || len(polled) != 1 {
				t.Fatalf("PollResults: %v (%d건)", err, len(polled))
			}
			cb, ok := v.TerminalCallback([]alimtalk.Receipt{tc.receipt})
			if !ok {
				t.Fatal("콜백 원문을 만들지 못했다")
			}
			pushed, err := v.ParseCallback(ctx, cb)
			if err != nil || len(pushed) != 1 {
				t.Fatalf("ParseCallback: %v (%d건)", err, len(pushed))
			}
			a, b := polled[0], pushed[0]
			if a.Status != tc.wantStat || b.Status != tc.wantStat {
				t.Fatalf("status: want %q, poll=%q callback=%q", tc.wantStat, a.Status, b.Status)
			}
			if a.DeliveredVia != tc.wantVia || b.DeliveredVia != tc.wantVia {
				t.Fatalf("delivered_via: want %q, poll=%q callback=%q", tc.wantVia, a.DeliveredVia, b.DeliveredVia)
			}
			if !a.Terminal || !b.Terminal {
				t.Fatalf("양쪽 다 종결이어야 한다: poll=%v callback=%v", a.Terminal, b.Terminal)
			}
			if a.CostAmount != b.CostAmount || a.CostCurrency != b.CostCurrency {
				t.Fatalf("원가가 경로에 따라 다르다: poll=%v%s callback=%v%s",
					a.CostAmount, a.CostCurrency, b.CostAmount, b.CostCurrency)
			}
			if a.FailureClass != b.FailureClass {
				t.Fatalf("실패 분류가 경로에 따라 다르다: poll=%q callback=%q", a.FailureClass, b.FailureClass)
			}
		})
	}
}

// repoRoot — go.work를 찾아 저장소 루트를 올라간다. 상대 경로 하드코딩보다 덜 깨진다.
func repoRoot() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

// deployManifestPaths — 배포용 사본 후보. 켜진 이름과 꺼진 이름 둘 다 본다.
//
// 저장소에 커밋된 것은 .disabled 쪽이고(운영자가 확장자를 바꿔 켠다), 확장자를 뗀 이름은
// E2E를 돌리는 동안에만 존재한다. 켜진 이름만 보면 깨끗한 체크아웃과 CI에서 늘 skip돼
// 가드가 있는 척만 하게 된다.
var deployManifestPaths = []string{
	filepath.Join("deploy", "connectors", "alimtalk_mock.json.disabled"),
	filepath.Join("deploy", "connectors", "alimtalk_mock.json"),
}

// TestDeployManifestMatchesEmbedded — deploy/connectors의 배포용 사본이 내장 manifest와
// 어긋나지 않는지. 존재하는 사본은 모두 검사한다.
//
// 사본이 여럿이라 한쪽만 고치는 사고가 난다. 레지스트리는 id와 channel만 보고 받아주므로
// (NewWithClock), 배포 사본의 capabilities가 다르면 목이 계약 테스트가 검증한 것과 다르게
// 동작하고 E2E는 원인이 보이지 않는 실패를 낸다. 형식 차이는 무시하고 동작을 정하는
// 필드만 비교한다.
func TestDeployManifestMatchesEmbedded(t *testing.T) {
	root, ok := repoRoot()
	if !ok {
		t.Skip("저장소 루트를 찾지 못했다 — 배포 사본 대조를 건너뛴다")
	}
	embedded, err := EmbeddedManifest()
	if err != nil {
		t.Fatalf("내장 manifest: %v", err)
	}

	checked := 0
	for _, rel := range deployManifestPaths {
		path := filepath.Join(root, rel)
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("%s 읽기: %v", rel, err)
		}
		checked++
		t.Run(rel, func(t *testing.T) { compareToEmbedded(t, rel, raw, embedded) })
	}
	if checked == 0 {
		t.Skipf("배포 사본이 하나도 없다 (%v) — 대조를 건너뛴다", deployManifestPaths)
	}
}

func compareToEmbedded(t *testing.T, rel string, raw []byte, embedded connector.Manifest) {
	t.Helper()
	deployed, err := connector.Parse(raw)
	if err != nil {
		t.Fatalf("%s가 connector.Parse를 통과하지 못한다: %v", rel, err)
	}
	if deployed.ID != embedded.ID || deployed.Channel != embedded.Channel {
		t.Fatalf("id·channel 불일치: 배포 %s/%s, 내장 %s/%s",
			deployed.ID, deployed.Channel, embedded.ID, embedded.Channel)
	}
	if deployed.Runtime.Type != embedded.Runtime.Type {
		t.Fatalf("runtime.type 불일치: 배포 %q, 내장 %q", deployed.Runtime.Type, embedded.Runtime.Type)
	}
	if !reflect.DeepEqual(deployed.Capabilities, embedded.Capabilities) {
		t.Fatalf("capabilities 불일치 — 목이 계약 테스트와 다르게 동작한다\n배포: %+v\n내장: %+v",
			deployed.Capabilities, embedded.Capabilities)
	}
	if !reflect.DeepEqual(deployed.Lifecycle, embedded.Lifecycle) {
		t.Fatalf("lifecycle 불일치\n배포: %+v\n내장: %+v", deployed.Lifecycle, embedded.Lifecycle)
	}
	if !reflect.DeepEqual(deployed.ContractTests, embedded.ContractTests) {
		t.Fatalf("contract_tests 불일치\n배포: %v\n내장: %v", deployed.ContractTests, embedded.ContractTests)
	}
	if !reflect.DeepEqual(deployed.Cost, embedded.Cost) {
		t.Fatalf("cost 불일치 — 원가 집계가 갈린다\n배포: %+v\n내장: %+v", deployed.Cost, embedded.Cost)
	}
}

// e2eScriptPath — 알림톡 P0 E2E 스크립트. 이 목의 픽스처를 seed 데이터로 되풀이해 적는다.
var e2eScriptPath = filepath.Join("tests", "e2e", "alimtalk.mjs")

// jsStringLiteral — Go 문자열을 JS 큰따옴표 리터럴 표기로 바꾼다.
//
// 픽스처의 Content에는 진짜 개행이 들어 있지만 E2E는 소스에 "\n" 두 글자로 적는다.
// 이 변환 없이 그대로 부분문자열을 찾으면 본문이 같아도 늘 실패한다.
func jsStringLiteral(s string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
	).Replace(s)
}

// TestE2ETemplateBodiesMatchFixtures — E2E가 DB에 심는 승인 본문이 목 픽스처와 같은지.
//
// ValidateSend는 저장된 content에서 필수 치환자를 도출하므로, 한 글자만 어긋나도
// "치환자 값 누락"으로 거절되고 원인은 전혀 다른 곳을 가리킨다. 게다가 이 스크립트는
// CI에서 돌지 않고 사람이 docker를 띄워야 도는 것이라, 여기서 잡지 않으면 드리프트가
// 누군가 손으로 돌릴 때까지 드러나지 않는다.
func TestE2ETemplateBodiesMatchFixtures(t *testing.T) {
	root, ok := repoRoot()
	if !ok {
		t.Skip("저장소 루트를 찾지 못했다 — E2E 대조를 건너뛴다")
	}
	raw, err := os.ReadFile(filepath.Join(root, e2eScriptPath))
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("E2E 스크립트가 없다: %s", e2eScriptPath)
		}
		t.Fatalf("%s 읽기: %v", e2eScriptPath, err)
	}
	script := string(raw)
	v := newVendor(t)

	for _, code := range []string{TemplateOrder, TemplatePromo} {
		t.Run(code, func(t *testing.T) {
			tmpl, ok := v.template(code)
			if !ok {
				t.Fatalf("픽스처에 %s가 없다", code)
			}
			if !strings.Contains(script, code) {
				t.Fatalf("%s가 %s에 없다 — E2E가 다른 템플릿을 심고 있다", code, e2eScriptPath)
			}
			if want := jsStringLiteral(tmpl.Content); !strings.Contains(script, want) {
				t.Fatalf("%s의 승인 본문이 %s와 어긋난다.\n"+
					"픽스처(templates.go)의 본문을 JS 리터럴로 적으면:\n  %s\n"+
					"양쪽을 같게 맞춰야 한다 — 안 그러면 E2E가 치환자 누락으로 거절되고 원인이 보이지 않는다.",
					code, e2eScriptPath, want)
			}
			// 치환자 배열도 같은 본문에서 나온다. ARRAY[...] 서식에 기대지 않도록
			// 이름이 따옴표로 감싸여 등장하는지만 본다.
			for _, name := range alimtalk.Variables(tmpl.Content) {
				if !strings.Contains(script, "'"+name+"'") {
					t.Fatalf("%s의 치환자 %q가 %s의 variables 배열에 없다", code, name, e2eScriptPath)
				}
			}
		})
	}
}
