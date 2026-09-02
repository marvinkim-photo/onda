package message

import (
	"testing"
	"time"

	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
)

func TestGroupReceipts(t *testing.T) {
	in := []receipt{
		{TenantID: "t1", AppID: "a1", ConnectorID: "c1", MessageID: "m1"},
		{TenantID: "t1", AppID: "a1", ConnectorID: "c1", MessageID: "m2"},
		{TenantID: "t1", AppID: "a2", ConnectorID: "c1", MessageID: "m3"},
		{TenantID: "t2", AppID: "a1", ConnectorID: "c1", MessageID: "m4"},
	}
	got := groupReceipts(in)
	if len(got) != 3 {
		t.Fatalf("(테넌트,앱,커넥터) 3묶음 기대, got %d", len(got))
	}
	k := groupKey{tenantID: "t1", appID: "a1", connectorID: "c1"}
	if len(got[k]) != 2 {
		t.Fatalf("같은 앱·커넥터는 한 번에 조회해야 한다, got %d", len(got[k]))
	}
}

// 결과가 늦는 접수가 매 tick마다 벤더를 두드리면 안 된다 — 간격이 지수적으로 늘고 상한에서 멈춘다.
func TestPollBackoff(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, pollBackoffBase},
		{1, pollBackoffBase},
		{2, 2 * pollBackoffBase},
		{3, 4 * pollBackoffBase},
		{99, pollBackoffCap},
	}
	for _, c := range cases {
		if got := pollBackoff(c.attempts); got != c.want {
			t.Fatalf("attempts=%d: %v 기대, got %v", c.attempts, c.want, got)
		}
	}
	if pollBackoff(6) > pollBackoffCap {
		t.Fatal("상한을 넘으면 안 된다")
	}
}

func TestSplitExhausted(t *testing.T) {
	rs := []receipt{
		{MessageID: "young", Attempts: 0},
		{MessageID: "mid", Attempts: maxPollAttempts - 3},
		{MessageID: "old", Attempts: maxPollAttempts - 1},
	}
	giveUp, keep := splitExhausted(rs)
	if len(giveUp) != 1 || giveUp[0].MessageID != "old" {
		t.Fatalf("상한 도달분만 포기해야 한다, got %+v", giveUp)
	}
	if len(keep) != 2 {
		t.Fatalf("나머지는 계속 조회, got %d", len(keep))
	}
}

func TestRemaining(t *testing.T) {
	rs := []receipt{{MessageID: "m1"}, {MessageID: "m2"}, {MessageID: "m3"}}
	got := remaining(rs, []string{"m2"})
	if len(got) != 2 || got[0].MessageID != "m1" || got[1].MessageID != "m3" {
		t.Fatalf("종결분만 빠져야 한다, got %+v", got)
	}
	if len(remaining(rs, nil)) != 3 {
		t.Fatal("종결이 없으면 전부 남는다")
	}
}

func TestBuildPollEvent(t *testing.T) {
	at := time.Date(2026, 9, 2, 1, 2, 3, 0, time.UTC)
	r := receipt{TenantID: "t1", AppID: "a1", ConnectorID: "kakao_alimtalk_nhn",
		MessageID: "m1", ProviderMessageID: "prov-1"}

	t.Run("delivered", func(t *testing.T) {
		ev := buildPollEvent(r, alimtalk.Event{
			MessageID: "m1", Status: "delivered", OccurredAt: at, Terminal: true,
			CostCurrency: "KRW", CostAmount: 6.5,
		}, alimtalk.ChannelID)
		switch {
		case ev.Status != "delivered":
			t.Fatalf("status delivered 기대, got %q", ev.Status)
		case ev.Source != "provider_callback":
			t.Fatalf("공급자가 만든 사실이므로 provider_callback 기대, got %q", ev.Source)
		case ev.ConnectorID != "kakao_alimtalk_nhn":
			t.Fatalf("connector_id 기대, got %q", ev.ConnectorID)
		case ev.ProviderMessageID == nil || *ev.ProviderMessageID != "prov-1":
			t.Fatalf("provider_message_id 기대, got %v", ev.ProviderMessageID)
		case ev.Cost == nil || ev.Cost.Currency != "KRW" || ev.Cost.Amount != 6.5:
			t.Fatalf("원가 기대, got %+v", ev.Cost)
		case ev.FailureClass != nil:
			t.Fatalf("성공에 failure_class는 없어야 한다: %v", *ev.FailureClass)
		}
		if ev.OccurredAt != at.Format(time.RFC3339Nano) {
			t.Fatalf("occurred_at 기대 %q, got %q", at.Format(time.RFC3339Nano), ev.OccurredAt)
		}
	})

	t.Run("failed는 enum으로 환원", func(t *testing.T) {
		ev := buildPollEvent(r, alimtalk.Event{
			MessageID: "m1", Status: "failed", OccurredAt: at, Terminal: true,
			FailureClass: "수신거부", FailureDetail: "코드 1004",
		}, alimtalk.ChannelID)
		if ev.FailureClass == nil || *ev.FailureClass != "unsupported" {
			t.Fatalf("enum 밖 분류는 unsupported로 환원 기대, got %v", ev.FailureClass)
		}
		if ev.FailureDetail == nil || *ev.FailureDetail != "수신거부: 코드 1004" {
			t.Fatalf("원문 분류가 detail에 보존돼야 한다, got %v", ev.FailureDetail)
		}
	})
}

func TestNormalizeClassKeepsEnumValues(t *testing.T) {
	for _, c := range []string{"retryable", "rate_limited", "permanent_content",
		"invalid_target", "credential_auth", "unsupported", "retry_exhausted"} {
		got, detail := normalizeClass(c, "사유")
		if got != c || detail != "사유" {
			t.Fatalf("%s는 그대로여야 한다, got %s / %q", c, got, detail)
		}
	}
	if got, _ := normalizeClass("", "x"); got != "" {
		t.Fatalf("빈 분류는 빈 채로, got %q", got)
	}
}

func TestStrPtrTruncatesDetail(t *testing.T) {
	long := make([]byte, maxDetail+100)
	for i := range long {
		long[i] = 'a'
	}
	got := strPtr(string(long))
	if got == nil || len(*got) != maxDetail {
		t.Fatalf("failure_detail 상한 %d로 잘려야 한다", maxDetail)
	}
	if strPtr("") != nil {
		t.Fatal("빈 문자열은 null이어야 한다(스키마 nullable)")
	}
}

// Expired는 "결과가 확정됐다"가 아니라 "못 알아냈다"이다. 대체발송이면 도달 채널이 바뀐다.
func TestBuildPollEventUsesDeliveredVia(t *testing.T) {
	r := receipt{ConnectorID: "c1", MessageID: "m1", ProviderMessageID: "p1"}
	ev := buildPollEvent(r, alimtalk.Event{
		MessageID: "m1", Status: "delivered", DeliveredVia: "lms",
		OccurredAt: time.Now().UTC(), Terminal: true,
	}, alimtalk.ChannelID)
	if ev.Channel != "lms" {
		t.Fatalf("대체발송 도달 채널 lms 기대, got %q", ev.Channel)
	}
	ev = buildPollEvent(r, alimtalk.Event{
		MessageID: "m1", Status: "delivered", OccurredAt: time.Now().UTC(), Terminal: true,
	}, alimtalk.ChannelID)
	if ev.Channel != alimtalk.ChannelID {
		t.Fatalf("대체발송이 아니면 원 채널 기대, got %q", ev.Channel)
	}
}
