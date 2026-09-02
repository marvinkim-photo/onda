package nhn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
)

// sendAndPoll — 발송 한 건을 접수하고 그 결과를 조회한다.
func sendAndPoll(t *testing.T, v *Vendor, base string, req alimtalk.SendRequest) (alimtalk.Receipt, []alimtalk.Event) {
	t.Helper()
	r, err := v.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	evs, err := v.PollResults(context.Background(), ValidCredential(base), []alimtalk.Receipt{r})
	if err != nil {
		t.Fatalf("PollResults: %v", err)
	}
	return r, evs
}

func TestPollDelivered(t *testing.T) {
	v, f, base := newTestVendor(t)
	r, evs := sendAndPoll(t, v, base, sendReq(base, SuffixDelivered))
	if len(evs) != 1 {
		t.Fatalf("이벤트 %d건: %v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Status != "delivered" || !ev.Terminal || ev.Expired {
		t.Fatalf("COMPLETED+MRC01은 delivered 종결이어야 한다: %+v", ev)
	}
	if ev.ProviderMessageID != r.ProviderMessageID || ev.MessageID != r.MessageID {
		t.Fatalf("식별자 왕복이 깨졌다: %+v", ev)
	}
	if ev.CostCurrency != "KRW" || ev.CostAmount <= 0 {
		t.Fatalf("도달 건은 원가를 실어야 한다: %+v", ev)
	}
	// receiveDate(KST)가 그대로 이벤트 시각이 된다.
	want := time.Date(2026, 9, 2, 19, 30, 11, 0, kst)
	if !ev.OccurredAt.Equal(want) {
		t.Fatalf("OccurredAt: want %v, got %v", want, ev.OccurredAt)
	}
	// 조회 경로에 복합키가 되펴져 들어갔는가.
	reqID, seq, _ := DecodeReceiptID(r.ProviderMessageID)
	wantPath := "/alimtalk/" + APIVersion + "/appkeys/" + ConfAppKey + "/messages/" + reqID + "/1"
	if f.LastPollPath != wantPath || seq != 1 {
		t.Fatalf("조회 경로: want %q, got %q", wantPath, f.LastPollPath)
	}
}

func TestPollFailedInvalidTarget(t *testing.T) {
	v, _, base := newTestVendor(t)
	_, evs := sendAndPoll(t, v, base, sendReq(base, SuffixInvalidTarget))
	if len(evs) != 1 {
		t.Fatalf("이벤트 %d건", len(evs))
	}
	ev := evs[0]
	if ev.Status != "failed" || !ev.Terminal {
		t.Fatalf("FAILED는 failed 종결이어야 한다: %+v", ev)
	}
	if ev.FailureClass != channel.FailureInvalidTarget.String() {
		t.Fatalf("failure_class: want invalid_target, got %q (%s)", ev.FailureClass, ev.FailureDetail)
	}
}

// TestPollExpired — 보존 기간이 지난 건은 Terminal이 아니라 Expired다.
// 둘을 섞으면 알 수 없는 건이 성공·실패로 집계된다.
func TestPollExpired(t *testing.T) {
	v, _, base := newTestVendor(t)
	_, evs := sendAndPoll(t, v, base, sendReq(base, SuffixExpired))
	if len(evs) != 1 {
		t.Fatalf("이벤트 %d건", len(evs))
	}
	ev := evs[0]
	if !ev.Expired {
		t.Fatalf("Expired여야 한다: %+v", ev)
	}
	if ev.Terminal {
		t.Fatalf("Expired는 Terminal이면 안 된다: %+v", ev)
	}
	if ev.Status != "" {
		t.Fatalf("결과를 모르는데 상태를 단정했다: %q", ev.Status)
	}
}

// TestPollFallbackDeliveredVia — 대체발송으로 살아난 건은 실제 도달 채널을 밝혀야 한다.
func TestPollFallbackDeliveredVia(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		via  string
		cost float64
	}{
		{"SMS", ChannelSMS, FallbackCostSMS},
		{"LMS", ChannelLMS, FallbackCostLMS},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			v, _, base := newTestVendor(t)
			req := sendReq(base, SuffixFallback)
			req.Fallback = &alimtalk.Fallback{Type: tc.typ, Title: "제목", Text: "대체 본문", SenderNo: "0212345678"}
			_, evs := sendAndPoll(t, v, base, req)
			if len(evs) != 1 {
				t.Fatalf("이벤트 %d건", len(evs))
			}
			ev := evs[0]
			if ev.Status != "sent" || !ev.Terminal {
				t.Fatalf("대체발송 성공은 sent 종결이어야 한다: %+v", ev)
			}
			if ev.DeliveredVia != tc.via {
				t.Fatalf("DeliveredVia: want %q, got %q", tc.via, ev.DeliveredVia)
			}
			if ev.CostAmount != tc.cost {
				t.Fatalf("대체발송 원가가 알림톡 단가로 잡혔다: got %v", ev.CostAmount)
			}
		})
	}
}

// TestPollFallbackAbsentFails — 같은 번호를 대체발송 없이 보내면 실패로 끝나야
// 위 성공이 "대체발송이 살렸다"임이 증명된다.
func TestPollFallbackAbsentFails(t *testing.T) {
	v, _, base := newTestVendor(t)
	_, evs := sendAndPoll(t, v, base, sendReq(base, SuffixFallback))
	if len(evs) != 1 || evs[0].Status != "failed" {
		t.Fatalf("대체발송 없이는 failed여야 한다: %+v", evs)
	}
	if evs[0].DeliveredVia != "" {
		t.Fatalf("대체발송이 아닌데 DeliveredVia가 찼다: %q", evs[0].DeliveredVia)
	}
}

// TestEventOfMapping — messageStatus·resultCode 조합 표를 순수 함수로 직접 본다.
// 진행 중 상태와 CANCEL은 가짜 서버 조종표에 없어 여기서만 걸린다.
func TestEventOfMapping(t *testing.T) {
	v := vendorFor(t)
	r := alimtalk.Receipt{ProviderMessageID: "req-1:1", MessageID: "m1"}
	cases := []struct {
		name     string
		in       messageResult
		wantNil  bool
		status   string
		terminal bool
		class    channel.FailureClass
	}{
		{name: "COMPLETED+MRC01", in: messageResult{MessageStatus: StatusCompleted, ResultCode: ResultCodeSuccess},
			status: "delivered", terminal: true},
		{name: "FAILED+MRC02(수신자)", in: messageResult{MessageStatus: StatusFailed, ResultCode: ResultCodeFail, ResultCodeName: "수신번호 오류"},
			status: "failed", terminal: true, class: channel.FailureInvalidTarget},
		{name: "FAILED(본문 사유)", in: messageResult{MessageStatus: StatusFailed, ResultCode: ResultCodeFail, ResultCodeName: "템플릿 본문 불일치"},
			status: "failed", terminal: true, class: channel.FailurePermanentContent},
		{name: "COMPLETED+MRC02", in: messageResult{MessageStatus: StatusCompleted, ResultCode: ResultCodeFail, ResultCodeName: "알 수 없는 사유"},
			status: "failed", terminal: true, class: channel.FailureInvalidTarget},
		{name: "CANCEL", in: messageResult{MessageStatus: StatusCancel},
			status: "failed", terminal: true, class: channel.FailurePermanentContent},
		{name: "진행 중(READY)", in: messageResult{MessageStatus: "READY"}, wantNil: true},
		{name: "진행 중(SENDING)", in: messageResult{MessageStatus: "SENDING"}, wantNil: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := v.eventOf(r, c.in)
			if c.wantNil {
				if ev != nil {
					t.Fatalf("진행 중 상태는 이벤트를 내면 안 된다 (폴링마다 중복 보고된다): %+v", ev)
				}
				return
			}
			if ev == nil {
				t.Fatal("이벤트가 나와야 한다")
			}
			if ev.Status != c.status || ev.Terminal != c.terminal {
				t.Fatalf("status/terminal: want (%q,%v), got (%q,%v)", c.status, c.terminal, ev.Status, ev.Terminal)
			}
			if c.class != channel.FailureNone && ev.FailureClass != c.class.String() {
				t.Fatalf("failure_class: want %s, got %q", c.class, ev.FailureClass)
			}
			if ev.OccurredAt.IsZero() {
				t.Fatal("OccurredAt이 제로다 — 수명주기 순서를 정할 수 없다")
			}
		})
	}
}

// TestPollSkipsForeignReceipt — 우리가 발급하지 않은 식별자로 배치 전체를 잃으면 안 된다.
func TestPollSkipsForeignReceipt(t *testing.T) {
	v, f, base := newTestVendor(t)
	r, err := v.Send(context.Background(), sendReq(base, SuffixDelivered))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	evs, err := v.PollResults(context.Background(), ValidCredential(base), []alimtalk.Receipt{
		{ProviderMessageID: "남의-식별자", MessageID: "x"},
		r,
	})
	if err != nil {
		t.Fatalf("PollResults: %v", err)
	}
	if len(evs) != 1 || evs[0].ProviderMessageID != r.ProviderMessageID {
		t.Fatalf("정상 건의 결과가 살아남아야 한다: %+v", evs)
	}
	if f.PollCalls != 1 {
		t.Fatalf("형식이 틀린 식별자는 조회하지 않아야 한다: got %d", f.PollCalls)
	}
}

// TestPollStopsOnCredentialAuth — 크리덴셜 오류는 뒤 건도 똑같이 실패하므로 즉시 멈춘다.
func TestPollStopsOnCredentialAuth(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		writeJSON(w, http.StatusUnauthorized, map[string]any{"header": failHeader(-40010, "유효하지 않은 SecretKey입니다")})
	}))
	defer srv.Close()
	v := vendorFor(t)
	_, err := v.PollResults(context.Background(), ValidCredential(srv.URL), []alimtalk.Receipt{
		{ProviderMessageID: "req-1:1"}, {ProviderMessageID: "req-2:1"}, {ProviderMessageID: "req-3:1"},
	})
	if got := v.Classify(err); got != channel.FailureCredentialAuth {
		t.Fatalf("분류: want credential_auth, got %s (%v)", got, err)
	}
	if calls != 1 {
		t.Fatalf("첫 크리덴셜 오류에서 멈춰야 한다: got %d회", calls)
	}
}

// TestPollExpiredByMessage — 200 + isSuccessful=false + "조회 기간 만료"도 Expired다.
func TestPollExpiredByMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"header": failHeader(-40005, "조회 기간이 만료되었습니다")})
	}))
	defer srv.Close()
	v := vendorFor(t)
	evs, err := v.PollResults(context.Background(), ValidCredential(srv.URL),
		[]alimtalk.Receipt{{ProviderMessageID: "req-9:3", MessageID: "m9"}})
	if err != nil {
		t.Fatalf("PollResults: %v", err)
	}
	if len(evs) != 1 || !evs[0].Expired || evs[0].Terminal {
		t.Fatalf("Expired(비종결)여야 한다: %+v", evs)
	}
}

func TestPollCredentialErrorsBeforeNetwork(t *testing.T) {
	v := vendorFor(t)
	_, err := v.PollResults(context.Background(), alimtalk.Credential{ConnectorID: ConnectorID}, nil)
	if got := v.Classify(err); got != channel.FailureCredentialAuth {
		t.Fatalf("want credential_auth, got %s (%v)", got, err)
	}
}
