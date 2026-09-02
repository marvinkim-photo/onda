package message

import (
	"context"
	"testing"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk/mock"
)

// mockvendor_test.go — 진짜 목 벤더를 태우는 결합 테스트.
//
// message_test.go의 페이크 벤더는 내가 만든 값을 내가 다시 확인하는 구조라,
// 벤더가 실제로 무엇을 돌려주는지는 검증하지 못한다. 여기서는 목의 조종표로
// 발송 → 결과 조회 → lifecycle까지 한 번에 태워서 경계 매핑을 굳힌다.

func mockSetup(t *testing.T) (*Worker, *alimtalk.Registry, string) {
	t.Helper()
	m, err := mock.EmbeddedManifest()
	if err != nil {
		t.Fatalf("내장 manifest: %v", err)
	}
	reg := testRegistry(t, m)
	w, _ := newTestWorker(t, reg)
	return w, reg, mock.ConnectorID
}

func mockCredential() channel.Credentials {
	return channel.Credentials{Kind: "alimtalk", JSON: []byte(`{"api_key":"k","sender_key":"sk-1"}`)}
}

// mockPayload — 목의 승인 템플릿 픽스처에 맞춘 발송. to의 끝 4자리가 결과를 조종한다.
func mockPayload(suffix string) Payload {
	p := validPayload()
	p.Connector.ID = mock.ConnectorID
	p.Target.Value = "+8210000" + suffix
	p.Content.Template = &Template{
		Code: mock.TemplateOrder,
		Variables: map[string]string{
			"고객명": "김철수", "주문번호": "A-1", "결제금액": "12000",
		},
		SenderKey:       "sk-1",
		RenderedPreview: "김철수님, 주문 A-1이 정상 접수되었습니다.\n결제금액: 12000원\n\n주문 상세는 아래 버튼에서 확인하실 수 있습니다.",
	}
	return p
}

// sendVia — 워커의 Send를 태우고 접수를 돌려준다.
func sendVia(t *testing.T, w *Worker, reg *alimtalk.Registry, p Payload) receipt {
	t.Helper()
	job := jobFor(t, reg, mock.ConnectorID, p)
	providerID, err := w.Send(context.Background(), testEnv(), job, mockCredential())
	if err != nil {
		t.Fatalf("발송 실패: %v", err)
	}
	return receipt{
		TenantID: testEnv().TenantID, AppID: testEnv().AppID,
		ConnectorID: mock.ConnectorID, MessageID: p.MessageID, ProviderMessageID: providerID,
	}
}

func pollOne(t *testing.T, reg *alimtalk.Registry, r receipt) alimtalk.Event {
	t.Helper()
	v, err := reg.Get(mock.ConnectorID)
	if err != nil {
		t.Fatalf("벤더 해석: %v", err)
	}
	cred := alimtalk.Credential{ConnectorID: mock.ConnectorID, JSON: mockCredential().JSON}
	events, err := v.PollResults(context.Background(), cred,
		[]alimtalk.Receipt{{ProviderMessageID: r.ProviderMessageID, MessageID: r.MessageID}})
	if err != nil {
		t.Fatalf("PollResults: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("이벤트 1건 기대, got %d", len(events))
	}
	return events[0]
}

// 대체발송은 도달 채널과 원가가 함께 바뀐다. 채널만 바꾸고 원가를 알림톡 단가로 두면
// SMS 전환이 공짜로 보이고, 원가를 바꾸고 채널을 두면 알림톡 도달률이 부풀어 오른다.
func TestMockFallbackCarriesChannelAndCost(t *testing.T) {
	w, reg, _ := mockSetup(t)
	m, _ := mock.EmbeddedManifest()

	cases := []struct {
		name     string
		fallback string
		wantVia  string
		wantCost float64
	}{
		{"SMS 대체발송", "sms", mock.ChannelSMS, mock.FallbackCostSMS},
		{"LMS 대체발송", "lms", mock.ChannelLMS, mock.FallbackCostLMS},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := mockPayload(mock.SuffixFallback)
			p.Fallback = []Step{{
				Channel: c.fallback,
				Content: Content{Text: &TextContent{Body: "주문이 접수되었습니다", Sender: "025550000"}},
				On:      []string{"invalid_target"},
			}}

			r := sendVia(t, w, reg, p)
			ev := pollOne(t, reg, r)

			if ev.DeliveredVia != c.wantVia {
				t.Fatalf("DeliveredVia %q 기대, got %q", c.wantVia, ev.DeliveredVia)
			}
			if decidePollEvent(ev, true) != pollEmit {
				t.Fatalf("대체발송 결과는 발행 대상이어야 한다")
			}
			if !ev.Terminal {
				t.Fatal("대체발송된 sent는 종결이어야 한다 — 아니면 접수가 대기 목록에서 영영 안 빠진다")
			}

			out := buildPollEvent(r, ev, m.Channel)
			if out.Channel != c.wantVia {
				t.Fatalf("lifecycle channel %q 기대, got %q", c.wantVia, out.Channel)
			}
			if out.Cost == nil || out.Cost.Amount != c.wantCost {
				t.Fatalf("원가 %.0f 기대, got %+v", c.wantCost, out.Cost)
			}
			if out.Status != "sent" {
				t.Fatalf("status sent 기대, got %q", out.Status)
			}
		})
	}
}

// 조회 보존 기간이 지난 접수는 발행 없이 정리한다. 성공으로도 실패로도 세지 않는다.
func TestMockExpiredDropsWithoutEmitting(t *testing.T) {
	w, reg, _ := mockSetup(t)

	r := sendVia(t, w, reg, mockPayload(mock.SuffixExpired))
	ev := pollOne(t, reg, r)

	if !ev.Expired || ev.Terminal {
		t.Fatalf("Expired=true·Terminal=false 기대, got expired=%v terminal=%v", ev.Expired, ev.Terminal)
	}
	if got := decidePollEvent(ev, true); got != pollDrop {
		t.Fatalf("발행 없이 정리(pollDrop) 기대, got %v", got)
	}
}

// 원 채널 도달은 채널도 원가도 알림톡 그대로다 (대체발송 매핑이 새지 않는지 확인).
func TestMockDeliveredKeepsOriginalChannel(t *testing.T) {
	w, reg, _ := mockSetup(t)
	m, _ := mock.EmbeddedManifest()

	r := sendVia(t, w, reg, mockPayload(mock.SuffixDelivered))
	ev := pollOne(t, reg, r)
	if decidePollEvent(ev, true) != pollEmit || !ev.Terminal {
		t.Fatalf("도달은 발행·종결이어야 한다, got %+v", ev)
	}

	out := buildPollEvent(r, ev, m.Channel)
	if out.Channel != alimtalk.ChannelID {
		t.Fatalf("원 채널 기대, got %q", out.Channel)
	}
	if out.Cost == nil || out.Cost.Amount != m.Cost.PerMessage {
		t.Fatalf("manifest 단가 %.0f 기대, got %+v", m.Cost.PerMessage, out.Cost)
	}
	if out.Status != "delivered" || out.FailureClass != nil {
		t.Fatalf("delivered·실패분류 없음 기대, got %q %v", out.Status, out.FailureClass)
	}
}

// 공급자가 거절한 수신자는 invalid_target으로 환원된다 (enum 밖 문자열이 새지 않는지).
func TestMockInvalidTargetMapsToEnum(t *testing.T) {
	w, reg, _ := mockSetup(t)
	m, _ := mock.EmbeddedManifest()

	r := sendVia(t, w, reg, mockPayload(mock.SuffixInvalidTarget))
	out := buildPollEvent(r, pollOne(t, reg, r), m.Channel)

	if out.Status != "failed" {
		t.Fatalf("failed 기대, got %q", out.Status)
	}
	if out.FailureClass == nil || *out.FailureClass != "invalid_target" {
		t.Fatalf("invalid_target 기대, got %v", out.FailureClass)
	}
}

// 남의 접수(우리가 발급하지 않은 message_id)는 무시한다.
func TestMockUnknownReceiptIgnored(t *testing.T) {
	if got := decidePollEvent(alimtalk.Event{MessageID: "남의 것", Status: "delivered"}, false); got != pollIgnore {
		t.Fatalf("pollIgnore 기대, got %v", got)
	}
}

// 크리덴셜은 요청마다 실린다. 목이 발송 시점에 인증하므로 이 경로가 실제로 검증된다.
func TestMockRejectsBadCredentialThroughWorker(t *testing.T) {
	w, reg, _ := mockSetup(t)
	job := jobFor(t, reg, mock.ConnectorID, mockPayload(mock.SuffixDelivered))

	_, err := w.Send(context.Background(), testEnv(), job,
		channel.Credentials{Kind: "alimtalk", JSON: []byte(`{"api_key":"invalid","sender_key":"sk-1"}`)})
	if channel.Classify(err) != channel.FailureCredentialAuth {
		t.Fatalf("credential_auth 기대, got %v", err)
	}
}

// 목의 조종표 → 워커 실패 분류. Send가 벤더 분류를 SendError에 실어 넘기는지 확인한다.
func TestMockSteeringToFailureClass(t *testing.T) {
	w, reg, _ := mockSetup(t)
	cases := []struct {
		suffix string
		want   channel.FailureClass
	}{
		{mock.SuffixPermanentContent, channel.FailurePermanentContent},
		{mock.SuffixRateLimited, channel.FailureRateLimited},
		{mock.SuffixRetryable, channel.FailureRetryable},
		{mock.SuffixCredentialAuth, channel.FailureCredentialAuth},
	}
	for _, c := range cases {
		job := jobFor(t, reg, mock.ConnectorID, mockPayload(c.suffix))
		_, err := w.Send(context.Background(), testEnv(), job, mockCredential())
		if got := w.Classify(err); got != c.want {
			t.Fatalf("%s: %v 기대, got %v (%v)", c.suffix, c.want, got, err)
		}
	}
	// 429는 Retry-After가 백오프로 전달돼야 한다.
	job := jobFor(t, reg, mock.ConnectorID, mockPayload(mock.SuffixRateLimited))
	_, err := w.Send(context.Background(), testEnv(), job, mockCredential())
	if channel.RetryAfterOf(err) != mock.RateLimitRetryAfter {
		t.Fatalf("Retry-After %v 기대, got %v", mock.RateLimitRetryAfter, channel.RetryAfterOf(err))
	}
}
