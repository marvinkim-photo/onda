package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
)

// caseValidateCredentials — 유효한 키는 통과하고, 무효한 키는 반드시 credential_auth여야 한다.
//
// 이 케이스가 스위트에서 가장 중요하다. channel.Verifier.judge가 invalid_target·
// permanent_content를 "인증은 통과"로 읽으므로, 400을 주는 공급자를 그대로 두면
// 틀린 키가 verified로 저장되고 운영자는 발송이 멈춘 뒤에야 알게 된다.
func caseValidateCredentials(t *testing.T, v alimtalk.Vendor, env Env) {
	ctx := context.Background()
	if err := v.Validate(ctx, env.Valid); err != nil {
		t.Fatalf("유효한 크리덴셜인데 Validate가 실패했다: %v", err)
	}
	err := v.Validate(ctx, env.Invalid)
	if err == nil {
		t.Fatal("무효한 크리덴셜인데 Validate가 통과했다")
	}
	if got := v.Classify(err); got != channel.FailureCredentialAuth {
		t.Fatalf("무효한 키는 credential_auth로 분류돼야 한다 (got %s): %v", got, err)
	}

	// 발송 경로도 같은 결론이어야 한다. Validate만 막고 Send가 통과하면,
	// 크리덴셜이 만료된 앱의 발송이 "본문 오류"로 분류돼 크리덴셜 정지가 걸리지 않는다.
	to, ok := env.Target(OutcomeDelivered)
	if !ok || to == "" {
		return
	}
	req := env.request("validate-credentials", v.Manifest())
	req.Credential = env.Invalid
	req.To = to
	if r, err := v.Send(ctx, req); err == nil {
		t.Fatalf("무효한 크리덴셜로 발송이 접수됐다 (provider_message_id=%s)", r.ProviderMessageID)
	} else if got := v.Classify(err); got != channel.FailureCredentialAuth {
		t.Fatalf("무효한 크리덴셜의 발송 실패는 credential_auth여야 한다 (got %s): %v", got, err)
	}
}

// caseSendOK — 접수 성공의 최소 계약: 공급자 식별자·message_id 왕복·접수 시각.
func caseSendOK(t *testing.T, v alimtalk.Vendor, env Env) {
	ctx := context.Background()
	m := v.Manifest()
	req := env.request("send-ok", m)
	req.To = target(t, env, OutcomeDelivered)

	r, err := v.Send(ctx, req)
	if err != nil {
		t.Fatalf("정상 발송이 실패했다: %v", err)
	}
	if r.ProviderMessageID == "" {
		t.Fatal("Receipt.ProviderMessageID가 비었다 — 콜백·폴링 조인이 불가능하다")
	}
	if r.MessageID != req.MessageID {
		t.Fatalf("Receipt.MessageID가 요청과 다르다: want %q, got %q", req.MessageID, r.MessageID)
	}
	if r.AcceptedAt.IsZero() {
		t.Fatal("Receipt.AcceptedAt이 제로다 — 접수 시각을 채워야 한다")
	}
}

// caseSendInvalidTarget — 잘못된 수신자는 invalid_target으로 드러나야 한다.
// 벤더에 따라 발송 시점(알리고류)일 수도, 나중 수명주기(NHN류)일 수도 있어 둘 다 인정한다.
// 어느 쪽이든 재시도 대상이 아니라는 결론만 같으면 된다.
func caseSendInvalidTarget(t *testing.T, v alimtalk.Vendor, env Env) {
	ctx := context.Background()
	m := v.Manifest()
	req := env.request("send-invalid-target", m)
	req.To = target(t, env, OutcomeInvalidTarget)

	r, err := v.Send(ctx, req)
	if err != nil {
		if got := v.Classify(err); got != channel.FailureInvalidTarget {
			t.Fatalf("동기 거절이면 invalid_target이어야 한다 (got %s): %v", got, err)
		}
		return
	}
	ev := terminalOf(ctx, t, v, env, r)
	if ev.Status != "failed" {
		t.Fatalf("종결 상태가 failed여야 한다 (got %q)", ev.Status)
	}
	if ev.FailureClass != channel.FailureInvalidTarget.String() {
		t.Fatalf("종결 failure_class가 invalid_target이어야 한다 (got %q)", ev.FailureClass)
	}
	if !m.Reports(ev.Status) {
		t.Fatalf("manifest.lifecycle.reports에 없는 상태를 보고했다: %q", ev.Status)
	}
}

// caseSendRateLimited — 429는 rate_limited로 분류되고 Retry-After를 실어야 한다.
// 공급자가 준 대기 시간을 버리고 지수 백오프로 되돌아가면 한도 초과가 길게 이어진다.
func caseSendRateLimited(t *testing.T, v alimtalk.Vendor, env Env) {
	ctx := context.Background()
	req := env.request("send-rate-limited", v.Manifest())
	req.To = target(t, env, OutcomeRateLimited)

	err := sendExpectingFailure(ctx, t, v, req, channel.FailureRateLimited)
	if d := channel.RetryAfterOf(err); d <= 0 {
		t.Fatalf("rate_limited 오류는 양수 Retry-After를 실어야 한다 (got %v)", d)
	}
}

// caseSendPermanentContent — 본문·템플릿 문제는 재시도 대상이 아니다.
func caseSendPermanentContent(t *testing.T, v alimtalk.Vendor, env Env) {
	ctx := context.Background()
	req := env.request("send-permanent-content", v.Manifest())
	req.To = target(t, env, OutcomePermanentContent)
	sendExpectingFailure(ctx, t, v, req, channel.FailurePermanentContent)
}

// caseCallbackParse — 웹훅 원문이 수명주기 이벤트로 정확히 환원되는가.
// 폴링 전용 벤더는 ParseCallback이 ErrUnsupported임을 확인한 뒤 skip한다.
func caseCallbackParse(t *testing.T, v alimtalk.Vendor, env Env) {
	ctx := context.Background()
	m := v.Manifest()
	if !m.NeedsCallback() {
		_, err := v.ParseCallback(ctx, alimtalk.RawCallback{ConnectorID: m.ID, Body: []byte("{}")})
		if !errors.Is(err, alimtalk.ErrUnsupported) {
			t.Fatalf("콜백 미지원 벤더의 ParseCallback은 ErrUnsupported여야 한다 (got %v)", err)
		}
		t.Skipf("manifest.lifecycle_mode=%s — 콜백을 받지 않는 벤더", m.Mode())
	}
	if m.Lifecycle.Callback == nil || m.Lifecycle.Callback.Path == "" {
		t.Fatal("콜백을 받겠다고 선언했으면 lifecycle.callback.path가 있어야 한다")
	}
	if env.Callback == nil {
		t.Skip("Env.Callback이 없어 콜백 원문을 만들 수 없다")
	}

	req := env.request("callback-parse", m)
	req.To = target(t, env, OutcomeDelivered)
	r, err := v.Send(ctx, req)
	if err != nil {
		t.Fatalf("콜백 케이스의 사전 발송이 실패했다: %v", err)
	}
	cb, ok := env.Callback([]alimtalk.Receipt{r})
	if !ok {
		t.Skipf("하네스가 %s의 콜백 원문을 만들지 못했다", r.ProviderMessageID)
	}

	evs, err := v.ParseCallback(ctx, cb)
	if err != nil {
		t.Fatalf("ParseCallback 실패: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("ParseCallback이 이벤트를 하나도 내지 않았다")
	}
	ev, ok := pick(evs, r.ProviderMessageID)
	if !ok {
		t.Fatalf("콜백 결과에 %s가 없다", r.ProviderMessageID)
	}
	if !m.Reports(ev.Status) {
		t.Fatalf("manifest.lifecycle.reports에 없는 상태를 보고했다: %q", ev.Status)
	}
	if ev.OccurredAt.IsZero() {
		t.Fatal("Event.OccurredAt이 제로다 — 수명주기 순서를 정할 수 없다")
	}
	if !ev.Terminal {
		t.Fatalf("delivered 접수의 콜백은 종결이어야 한다 (status=%q)", ev.Status)
	}

	if _, err := v.ParseCallback(ctx, alimtalk.RawCallback{ConnectorID: m.ID, Body: []byte("")}); err == nil {
		t.Fatal("빈 콜백 본문은 오류여야 한다")
	}
}

// caseIdempotentResend — 같은 message_id로 두 번 보내면 같은 공급자 식별자여야 한다.
// 워커 재기동·중복 소비로 재발송이 일어나는 것은 정상이고, 그때 식별자가 갈리면
// 수명주기 조인이 깨져 한 발송이 영원히 미종결로 남는다.
func caseIdempotentResend(t *testing.T, v alimtalk.Vendor, env Env) {
	ctx := context.Background()
	req := env.request("idempotent-resend", v.Manifest())
	req.To = target(t, env, OutcomeDelivered)

	first, err := v.Send(ctx, req)
	if err != nil {
		t.Fatalf("1차 발송 실패: %v", err)
	}
	second, err := v.Send(ctx, req)
	if err != nil {
		t.Fatalf("동일 message_id 재발송이 실패했다: %v", err)
	}
	if first.ProviderMessageID != second.ProviderMessageID {
		t.Fatalf("같은 message_id의 재발송이 다른 식별자를 냈다: %q vs %q",
			first.ProviderMessageID, second.ProviderMessageID)
	}
}

// contentProbe — manifest.capabilities.content의 값 하나와, 그것을 요청에 싣는 방법.
// SendRequest로 표현 가능한 content만 넣는다(image·item_list는 실을 필드가 없다).
type contentProbe struct {
	feature string
	apply   func(*alimtalk.SendRequest)
}

var contentProbes = []contentProbe{
	{"buttons", func(r *alimtalk.SendRequest) {
		r.Buttons = append(r.Buttons, alimtalk.Button{
			Type: "WL", Name: "미지원 확인용", LinkMo: "https://m.example.com/probe",
		})
	}},
	{"quick_replies", func(r *alimtalk.SendRequest) {
		r.QuickReplies = append(r.QuickReplies, alimtalk.QuickReply{
			Type: "WL", Name: "미지원 확인용", LinkMo: "https://m.example.com/probe",
		})
	}},
}

// caseUnsupportedContent — manifest가 선언하지 않은 content를 보내면 거절해야 한다.
//
// 선언과 구현이 어긋나면 엔진은 미지원 기능을 계속 재시도하거나 조용히 잘린 메시지를
// 내보낸다. 그래서 "재시도 불가(permanent_content)"로 분류되는 것까지 확인한다.
// 검사는 SendRequest로 표현 가능한 content(buttons·quick_replies)로 한정하며,
// 벤더가 그 둘을 모두 선언했으면 검사할 거리가 없으므로 skip한다.
func caseUnsupportedContent(t *testing.T, v alimtalk.Vendor, env Env) {
	ctx := context.Background()
	m := v.Manifest()
	var probe *contentProbe
	for i := range contentProbes {
		if !declaresContent(m, contentProbes[i].feature) {
			probe = &contentProbes[i]
			break
		}
	}
	if probe == nil {
		t.Skipf("이 벤더는 검사 가능한 content를 모두 선언했다 (%v)", m.Capabilities.Content)
	}

	req := env.request("unsupported-content", m)
	req.To = target(t, env, OutcomeDelivered)
	probe.apply(&req)

	r, err := v.Send(ctx, req)
	if err == nil {
		t.Fatalf("manifest가 선언하지 않은 content(%s)를 보냈는데 접수됐다 (provider_message_id=%s)",
			probe.feature, r.ProviderMessageID)
	}
	if got := v.Classify(err); got != channel.FailurePermanentContent {
		t.Fatalf("미선언 content는 permanent_content여야 한다 (got %s): %v", got, err)
	}
}

// caseFallbackTrigger — vendor_fallback을 선언한 벤더는 대체발송을 실제로 태워야 한다.
// 선언하지 않은 벤더는 Fallback이 실린 요청을 거절해야 한다(엔진이 폴백 체인을 돌린다).
func caseFallbackTrigger(t *testing.T, v alimtalk.Vendor, env Env) {
	ctx := context.Background()
	m := v.Manifest()
	if !m.Capabilities.VendorFallback {
		req := env.request("fallback-unsupported", m)
		req.To = target(t, env, OutcomeDelivered)
		req.Fallback = &alimtalk.Fallback{Type: "SMS", Text: "대체발송", SenderNo: "0212345678"}
		if _, err := v.Send(ctx, req); err == nil {
			t.Fatal("vendor_fallback=false인데 Fallback이 실린 요청을 접수했다")
		}
		t.Skip("manifest.capabilities.vendor_fallback=false — 대체발송은 엔진 폴백 체인의 몫")
	}
	if env.Fallback == nil {
		t.Skip("Env.Fallback이 없어 대체발송을 태울 수 없다")
	}

	to := target(t, env, OutcomeFallback)

	with := env.request("fallback-on", m)
	with.To = to
	with.Fallback = env.Fallback
	r, err := v.Send(ctx, with)
	if err != nil {
		t.Fatalf("대체발송이 실린 요청이 접수되지 않았다: %v", err)
	}
	ev := terminalOf(ctx, t, v, env, r)
	if ev.Status == "failed" {
		t.Fatalf("대체발송이 실렸는데 종결이 failed다 (%s)", ev.FailureDetail)
	}
	if !m.Reports(ev.Status) {
		t.Fatalf("manifest.lifecycle.reports에 없는 상태를 보고했다: %q", ev.Status)
	}
	// 어느 채널로 나갔는지 밝혀야 한다. 사유 문자열에만 남기면 엔진이 파싱할 수 없고,
	// SMS로 도달한 건이 알림톡 도달률·원가에 잡혀 집계가 조용히 틀어진다.
	if ev.DeliveredVia == "" {
		t.Fatal("대체발송으로 도달했는데 Event.DeliveredVia가 비었다 — 채널별 도달률·원가가 틀어진다")
	}
	if ev.DeliveredVia == m.Channel {
		t.Fatalf("DeliveredVia가 원 채널(%s)과 같다 — 대체발송이면 실제 도달 채널이어야 한다", m.Channel)
	}

	// 같은 대상을 Fallback 없이 보내면 실패해야 한다. 그래야 위 성공이
	// "이 번호는 원래 잘 간다"가 아니라 "대체발송이 살렸다"임이 증명된다.
	without := env.request("fallback-off", m)
	without.To = to
	bare, err := v.Send(ctx, without)
	if err != nil {
		if got := v.Classify(err); got == channel.FailureRetryable {
			t.Fatalf("대체발송 없는 실패가 retryable로 분류됐다 — 폴백 판단 근거가 되지 못한다: %v", err)
		}
		return
	}
	bareEv := terminalOf(ctx, t, v, env, bare)
	if bareEv.Status != "failed" {
		t.Fatalf("대체발송 없이도 %q로 끝났다 — 이 대상은 대체발송 검증에 쓸 수 없다", bareEv.Status)
	}
}
