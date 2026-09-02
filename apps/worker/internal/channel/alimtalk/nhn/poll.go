package nhn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
)

// ParseCallback — NHN은 발송 결과 웹훅을 발행하지 않는다.
//
// v2.3 문서 어디에도 결과 콜백 스펙이 없고, 수신 결과는 조회 API로만 얻는다.
// 그래서 이 벤더의 manifest.capabilities.lifecycle_mode는 "polling"이고 여기서는
// ErrUnsupported를 돌려준다. 계약 테스트 callback_parse가 바로 이 응답을 확인한 뒤 skip한다.
// (카카오 상담톡·친구톡 이벤트 웹훅은 별개 상품이라 발송 수명주기와 무관하다.)
func (v *Vendor) ParseCallback(_ context.Context, _ alimtalk.RawCallback) ([]alimtalk.Event, error) {
	return nil, alimtalk.ErrUnsupported
}

// messageResult — 단건 조회 응답의 message 블록.
//
// TODO(실계정 검증): v2.3 문서의 단건 조회 예시는 결과를 message 아래에 두지만 서비스별로
// 감싸는 이름이 다른 사례가 있어, 파싱은 message가 없으면 최상위에서도 찾아본다.
type messageResult struct {
	RequestID            string `json:"requestId"`
	RecipientSeq         int    `json:"recipientSeq"`
	RecipientNo          string `json:"recipientNo"`
	MessageStatus        string `json:"messageStatus"`
	ResultCode           string `json:"resultCode"`
	ResultCodeName       string `json:"resultCodeName"`
	ReceiveDate          string `json:"receiveDate"`
	RequestDate          string `json:"requestDate"`
	CreateDate           string `json:"createDate"`
	RecipientGroupingKey string `json:"recipientGroupingKey"`
	// 대체발송 결과. TODO(실계정 검증): 필드명이 문서에 명시되지 않아 관측한 표기를 쓴다.
	ResendStatus string `json:"resendStatus"`
	ResendType   string `json:"resendType"`
}

type queryResponse struct {
	Header  nhnHeader      `json:"header"`
	Message *messageResult `json:"message"`
}

// PollResults — 미종결 접수의 결과를 단건 조회로 확정한다.
//
// 대량 조회 API도 있지만 단건 조회가 "수신자 한 명의 결과"를 주는 문서화된 경로라
// 접수 하나에 한 번씩 GET한다. 복합키(requestId+recipientSeq)를 되펴는 곳이 여기다.
//
// 한 건이 실패해도 배치 전체를 버리지 않는다. 다만 크리덴셜·한도 오류는 뒤 건들도 똑같이
// 실패할 게 뻔하므로 즉시 멈추고, 그때까지 얻은 이벤트와 함께 오류를 돌려준다.
func (v *Vendor) PollResults(ctx context.Context, cred alimtalk.Credential, pending []alimtalk.Receipt) ([]alimtalk.Event, error) {
	c, err := parseCredential(cred)
	if err != nil {
		return nil, err
	}
	out := make([]alimtalk.Event, 0, len(pending))
	var firstErr error
	for _, r := range pending {
		ev, err := v.pollOne(ctx, c, r)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			switch v.Classify(err) {
			case channel.FailureCredentialAuth, channel.FailureRateLimited:
				return out, err
			}
			continue
		}
		if ev != nil {
			out = append(out, *ev)
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// pollOne — 접수 한 건의 결과. 아직 진행 중이면 (nil, nil)이라 폴러가 다음 주기에 다시 묻는다.
func (v *Vendor) pollOne(ctx context.Context, c credential, r alimtalk.Receipt) (*alimtalk.Event, error) {
	requestID, seq, err := DecodeReceiptID(r.ProviderMessageID)
	if err != nil {
		// 우리가 발급하지 않은 식별자다. 이걸로 배치를 실패시키면 나머지 결과까지 잃는다.
		return nil, nil
	}
	endpoint := c.appkeyPath(fmt.Sprintf("/messages/%s/%d", pathEscape(requestID), seq))
	resp, raw, err := v.doRaw(ctx, c, http.MethodGet, endpoint, nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		// NHN은 발송 이력을 90일만 보관한다. 없어진 건은 실패가 아니라 "결과를 못 알아냈다"이다.
		// Terminal로 위장하면 알 수 없는 건이 성공·실패로 집계되고, 그냥 두면 폴러가 영원히 묻는다.
		return v.expiredEvent(r, "NHN 조회 기간이 지나 결과를 확인할 수 없습니다 (HTTP 404)"), nil
	}
	if err := v.classifyHTTP(resp, raw); err != nil {
		return nil, err
	}
	h, err := decodeHeader(raw)
	if err != nil {
		return nil, channel.NewSendError(channel.FailureRetryable, "NHN 조회 응답 파싱 실패: %v (%s)", err, snippet(raw))
	}
	if !h.IsSuccessful {
		if isExpired(h.ResultMessage) {
			return v.expiredEvent(r, h.ResultMessage), nil
		}
		return nil, channel.NewSendError(classifyMessage(int(h.ResultCode), h.ResultMessage, channel.FailureRetryable),
			"NHN 조회가 실패했습니다 (resultCode=%d): %s", int(h.ResultCode), h.ResultMessage)
	}

	var q queryResponse
	if err := json.Unmarshal(raw, &q); err != nil {
		return nil, channel.NewSendError(channel.FailureRetryable, "NHN 조회 응답 파싱 실패: %v (%s)", err, snippet(raw))
	}
	m := q.Message
	if m == nil {
		var flat messageResult
		if err := json.Unmarshal(raw, &flat); err == nil && flat.MessageStatus != "" {
			m = &flat
		}
	}
	if m == nil || m.MessageStatus == "" {
		return nil, channel.NewSendError(channel.FailureRetryable,
			"NHN 조회 응답에 messageStatus가 없습니다 (%s)", snippet(raw))
	}
	return v.eventOf(r, *m), nil
}

// expiredEvent — 결과를 끝내 알 수 없는 건. Terminal이 아니라 Expired다.
func (v *Vendor) expiredEvent(r alimtalk.Receipt, detail string) *alimtalk.Event {
	return &alimtalk.Event{
		MessageID:         r.MessageID,
		ProviderMessageID: r.ProviderMessageID,
		OccurredAt:        v.clk.Now(),
		Expired:           true,
		Terminal:          false,
		FailureDetail:     detail,
	}
}

// eventOf — messageStatus + resultCode를 수명주기 이벤트로 환원한다.
//
//	COMPLETED + MRC01 → delivered (종결)
//	FAILED / MRC02    → failed    (종결, 분류는 resultCodeName에서)
//	CANCEL            → failed    (종결, 취소는 재시도로 나아지지 않는다)
//	그 외(대기·발송중) → nil       (아직 결과가 아니다. 폴러가 다음 주기에 다시 묻는다)
//
// 대체발송이 걸린 건은 알림톡 쪽 결과와 무관하게 실제 도달 채널을 밝힌다.
// 사유 문자열에만 남기면 SMS 도달이 알림톡 도달률·원가에 잡혀 집계가 조용히 틀어진다.
func (v *Vendor) eventOf(r alimtalk.Receipt, m messageResult) *alimtalk.Event {
	ev := alimtalk.Event{
		MessageID:         r.MessageID,
		ProviderMessageID: r.ProviderMessageID,
		OccurredAt:        v.occurredAt(m),
		Terminal:          true,
	}
	if via := deliveredVia(m); via != "" {
		ev.Status = "sent"
		ev.DeliveredVia = via
		ev.CostCurrency, ev.CostAmount = v.costFor(via)
		return &ev
	}

	status := strings.ToUpper(strings.TrimSpace(m.MessageStatus))
	code := strings.ToUpper(strings.TrimSpace(m.ResultCode))
	switch {
	case status == StatusCompleted && (code == ResultCodeSuccess || code == ""):
		ev.Status = "delivered"
		ev.CostCurrency, ev.CostAmount = v.costFor("")
	case status == StatusCancel:
		ev.Status = "failed"
		ev.FailureClass = channel.FailurePermanentContent.String()
		ev.FailureDetail = firstNonEmpty(m.ResultCodeName, "발송이 취소되었습니다 (CANCEL)")
	case status == StatusFailed || code == ResultCodeFail:
		ev.Status = "failed"
		ev.FailureDetail = firstNonEmpty(m.ResultCodeName, "NHN이 발송 실패로 보고했습니다 ("+status+")")
		ev.FailureClass = classifyMessage(0, ev.FailureDetail, channel.FailureInvalidTarget).String()
	default:
		// READY·SENDING 같은 진행 중 상태. 이벤트를 내면 같은 발송이 폴링 주기마다
		// 중복 보고되므로 아무것도 내지 않고 대기 목록에 남긴다.
		return nil
	}
	return &ev
}

// deliveredVia — 대체발송으로 실제 도달한 채널. 대체발송이 아니면 빈 문자열.
func deliveredVia(m messageResult) string {
	st := strings.ToUpper(strings.TrimSpace(m.ResendStatus))
	if st != StatusCompleted && st != "SUCCESS" {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(m.ResendType), "LMS") {
		return ChannelLMS
	}
	return ChannelSMS
}

// costFor — 실제 도달 채널의 건당 원가. via가 비면 원 채널(알림톡, manifest.cost)이다.
func (v *Vendor) costFor(via string) (string, float64) {
	if v.manifest.Cost == nil {
		return "", 0
	}
	switch via {
	case ChannelSMS:
		return v.manifest.Cost.Currency, FallbackCostSMS
	case ChannelLMS:
		return v.manifest.Cost.Currency, FallbackCostLMS
	default:
		return v.manifest.Cost.Currency, v.manifest.Cost.PerMessage
	}
}

// nhnTimeLayouts — NHN 날짜 표기. KST 벽시계이며 초가 붙기도 안 붙기도 한다.
var nhnTimeLayouts = []string{"2006-01-02 15:04:05.0", "2006-01-02 15:04:05", "2006-01-02 15:04", time.RFC3339}

// occurredAt — 이벤트 시각. 수신 시각 → 요청 시각 → 생성 시각 순으로 찾고, 없으면 현재 시각.
// 제로 시각은 절대 내보내지 않는다 — 수명주기 순서를 정할 수 없게 된다.
func (v *Vendor) occurredAt(m messageResult) time.Time {
	for _, s := range []string{m.ReceiveDate, m.RequestDate, m.CreateDate} {
		if t, ok := parseNHNTime(s); ok {
			return t
		}
	}
	return v.clk.Now()
}

func parseNHNTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range nhnTimeLayouts {
		if layout == time.RFC3339 {
			if t, err := time.Parse(layout, s); err == nil {
				return t, true
			}
			continue
		}
		if t, err := time.ParseInLocation(layout, s, kst); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// pathEscape — requestId를 URL 경로에 안전하게 싣는다.
func pathEscape(s string) string { return url.PathEscape(s) }

func firstNonEmpty(vals ...string) string {
	for _, s := range vals {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
