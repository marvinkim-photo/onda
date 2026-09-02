package nhn

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
)

// receiptSep — 복합 식별자의 구분자. requestId는 NHN이 발급하는 UUID형이라 콜론이 없다.
// 그래도 되편을 때는 마지막 콜론을 기준으로 잘라 requestId에 콜론이 섞여도 견딘다.
const receiptSep = ":"

// EncodeReceiptID — NHN 복합키를 Receipt.ProviderMessageID 한 문자열로 접는다.
//
// NHN에서 발송 한 건을 특정하려면 requestId(요청 단위)와 recipientSeq(그 요청 안의 수신자
// 순번)가 둘 다 필요하다. Receipt.ProviderMessageID는 벤더 중립 계약이라 문자열 하나뿐이므로
// 여기서 접고 PollResults가 DecodeReceiptID로 되편다. 이 접기가 벤더 안에 갇혀 있어야
// 엔진·큐·DB가 벤더별 식별자 모양을 몰라도 된다.
func EncodeReceiptID(requestID string, recipientSeq int) string {
	return requestID + receiptSep + strconv.Itoa(recipientSeq)
}

// DecodeReceiptID — EncodeReceiptID의 역함수.
func DecodeReceiptID(id string) (string, int, error) {
	i := strings.LastIndex(id, receiptSep)
	if i <= 0 || i == len(id)-1 {
		return "", 0, fmt.Errorf("nhn: 복합 식별자 형식이 아니다: %q (requestId%srecipientSeq여야 한다)", id, receiptSep)
	}
	seq, err := strconv.Atoi(id[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("nhn: recipientSeq가 숫자가 아니다: %q", id)
	}
	return id[:i], seq, nil
}

// nhnButton — NHN 버튼 표현. 필드명이 우리 Button과 다르다(schemeIos/schemeAndroid ↔ LinkIOS/LinkAndroid).
// 양방향 매핑을 한 곳에 모아 두어야 발송과 템플릿 동기화가 어긋나지 않는다.
type nhnButton struct {
	Ordering      int    `json:"ordering"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	LinkMo        string `json:"linkMo,omitempty"`
	LinkPc        string `json:"linkPc,omitempty"`
	SchemeIos     string `json:"schemeIos,omitempty"`
	SchemeAndroid string `json:"schemeAndroid,omitempty"`
}

// toNHNButtons — alimtalk.Button → NHN. ordering은 1부터 우리 배열 순서를 따른다.
func toNHNButtons(in []alimtalk.Button) []nhnButton {
	if len(in) == 0 {
		return nil
	}
	out := make([]nhnButton, 0, len(in))
	for i, b := range in {
		out = append(out, nhnButton{
			Ordering:      i + 1,
			Type:          b.Type,
			Name:          b.Name,
			LinkMo:        b.LinkMo,
			LinkPc:        b.LinkPC,
			SchemeIos:     b.LinkIOS,
			SchemeAndroid: b.LinkAndroid,
		})
	}
	return out
}

// fromNHNButton — NHN → alimtalk.Button. 템플릿 동기화가 쓴다.
func fromNHNButton(b nhnButton) alimtalk.Button {
	return alimtalk.Button{
		Type:        b.Type,
		Name:        b.Name,
		LinkMo:      b.LinkMo,
		LinkPC:      b.LinkPc,
		LinkIOS:     b.SchemeIos,
		LinkAndroid: b.SchemeAndroid,
	}
}

// resendParameter — NHN 대체발송. 알림톡이 도달하지 못하면 공급자가 직접 문자로 보낸다.
type resendParameter struct {
	IsResend      bool   `json:"isResend"`
	ResendType    string `json:"resendType,omitempty"`
	ResendTitle   string `json:"resendTitle,omitempty"`
	ResendContent string `json:"resendContent,omitempty"`
	ResendSendNo  string `json:"resendSendNo,omitempty"`
}

type recipient struct {
	RecipientNo          string            `json:"recipientNo"`
	TemplateParameter    map[string]string `json:"templateParameter,omitempty"`
	ResendParameter      *resendParameter  `json:"resendParameter,omitempty"`
	Buttons              []nhnButton       `json:"buttons,omitempty"`
	RecipientGroupingKey string            `json:"recipientGroupingKey,omitempty"`
}

type sendBody struct {
	SenderKey     string      `json:"senderKey"`
	TemplateCode  string      `json:"templateCode"`
	RequestDate   string      `json:"requestDate,omitempty"`
	RecipientList []recipient `json:"recipientList"`
}

type sendResult struct {
	RecipientSeq         int     `json:"recipientSeq"`
	RecipientNo          string  `json:"recipientNo"`
	ResultCode           flexInt `json:"resultCode"`
	ResultMessage        string  `json:"resultMessage"`
	RecipientGroupingKey string  `json:"recipientGroupingKey"`
}

type sendResponse struct {
	Header  nhnHeader `json:"header"`
	Message struct {
		RequestID         string       `json:"requestId"`
		SenderGroupingKey string       `json:"senderGroupingKey"`
		SendResults       []sendResult `json:"sendResults"`
	} `json:"message"`
}

// Send — 단건 발송(접수).
//
// 검사 순서는 크리덴셜 → 능력 → 본문 → 네트워크다. 잘못된 키로 나간 발송이 "본문 오류"로
// 분류되면 크리덴셜 정지가 걸리지 않으므로 크리덴셜을 가장 먼저 본다.
//
// 치환은 templateParameter로만 넘긴다. NHN이 승인 본문에 직접 렌더하므로 완성 본문
// (RenderedText)을 보내면 승인 본문과 어긋나 반려된다 — manifest.capabilities.substitution이
// "variables"인 이유다.
func (v *Vendor) Send(ctx context.Context, req alimtalk.SendRequest) (alimtalk.Receipt, error) {
	if strings.TrimSpace(req.MessageID) == "" {
		return alimtalk.Receipt{}, channel.NewSendError(channel.FailurePermanentContent, "message_id 누락")
	}
	c, err := parseCredential(req.Credential)
	if err != nil {
		return alimtalk.Receipt{}, err
	}
	senderKey := strings.TrimSpace(req.SenderKey)
	if senderKey == "" {
		senderKey = strings.TrimSpace(c.SenderKey)
	}
	if senderKey == "" {
		// 발신프로필 키는 "권한" 쪽 값이다. permanent_content로 흘리면 verifier가
		// 이것을 인증 통과로 읽어 설정이 덜 된 커넥터가 verified로 남는다.
		return alimtalk.Receipt{}, channel.NewSendError(channel.FailureCredentialAuth,
			"발신프로필 키(senderKey)가 없습니다 — 요청·크리덴셜·커넥터 설정 어디에도 없습니다")
	}
	if err := v.checkCapabilities(req); err != nil {
		return alimtalk.Receipt{}, err
	}
	body, err := v.buildSendBody(senderKey, c, req)
	if err != nil {
		return alimtalk.Receipt{}, err
	}

	raw, err := v.doJSON(ctx, c, "POST", c.appkeyPath("/messages"), body,
		// 멱등의 기준은 MessageID다. 워커 재기동·중복 소비로 같은 발송이 다시 흘러도
		// NHN이 10분 안에는 새 requestId를 만들지 않아 수명주기 조인이 깨지지 않는다.
		map[string]string{IdempotencyHeader: req.MessageID})
	if err != nil {
		return alimtalk.Receipt{}, err
	}
	if _, err := checkHeader(raw); err != nil {
		return alimtalk.Receipt{}, err
	}

	var resp sendResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return alimtalk.Receipt{}, channel.NewSendError(channel.FailureRetryable,
			"NHN 발송 응답 파싱 실패: %v (%s)", err, snippet(raw))
	}
	if resp.Message.RequestID == "" {
		return alimtalk.Receipt{}, channel.NewSendError(channel.FailureRetryable,
			"NHN이 requestId를 돌려주지 않았습니다 — 결과를 조회할 방법이 없습니다 (%s)", snippet(raw))
	}
	if len(resp.Message.SendResults) == 0 {
		return alimtalk.Receipt{}, channel.NewSendError(channel.FailureRetryable,
			"NHN이 수신자별 접수 결과(sendResults)를 돌려주지 않았습니다 (requestId=%s)", resp.Message.RequestID)
	}
	r := resp.Message.SendResults[0]
	if int(r.ResultCode) != 0 {
		// 요청은 받아들여졌지만 이 수신자만 떨어졌다. 수신자를 가리키면 invalid_target,
		// 아니면 본문·템플릿 문제다. 어느 쪽이든 재시도해서 나아지지 않는다.
		return alimtalk.Receipt{}, channel.NewSendError(
			classifyRecipient(int(r.ResultCode), r.ResultMessage),
			"NHN이 수신자를 거절했습니다 (resultCode=%d): %s", int(r.ResultCode), r.ResultMessage)
	}
	return alimtalk.Receipt{
		ProviderMessageID: EncodeReceiptID(resp.Message.RequestID, r.RecipientSeq),
		MessageID:         req.MessageID,
		AcceptedAt:        v.clk.Now(),
	}, nil
}

// buildSendBody — SendRequest를 v2.3 본문으로 옮긴다.
func (v *Vendor) buildSendBody(senderKey string, c credential, req alimtalk.SendRequest) (sendBody, error) {
	code := strings.TrimSpace(req.TemplateCode)
	if code == "" {
		return sendBody{}, channel.NewSendError(channel.FailurePermanentContent, "템플릿 코드 누락")
	}
	if len(code) > MaxTemplateCodeLen {
		return sendBody{}, channel.NewSendError(channel.FailurePermanentContent,
			"템플릿 코드가 %d자로 NHN 상한(%d자)을 넘습니다: %s", len(code), MaxTemplateCodeLen, code)
	}
	to, err := normalizeRecipient(req.To)
	if err != nil {
		return sendBody{}, err
	}
	// isAd는 승인 템플릿을 알아야 판정되는데 발송 시점의 벤더에는 템플릿이 없다.
	// AC(채널추가) 버튼의 광고형 제한은 템플릿을 아는 엔진 쪽(alimtalk.ValidateSend)이 이미 걸었으므로
	// 여기서는 타입·이름·링크 같은 벤더 무관 규칙만 다시 확인한다.
	if err := alimtalk.ValidateButtons(req.Buttons, true); err != nil {
		return sendBody{}, channel.NewSendError(channel.FailurePermanentContent, "%s", err)
	}

	rc := recipient{
		RecipientNo:       to,
		TemplateParameter: req.Variables,
		Buttons:           toNHNButtons(req.Buttons),
		// recipientGroupingKey로 우리 message_id를 실어 둔다. 대량 조회 응답에서
		// 우리 발송을 되찾는 유일한 단서이고, NHN이 그대로 되돌려준다.
		RecipientGroupingKey: req.MessageID,
	}
	if req.Fallback != nil {
		rp, err := resendOf(*req.Fallback, c)
		if err != nil {
			return sendBody{}, err
		}
		rc.ResendParameter = rp
	}

	out := sendBody{SenderKey: senderKey, TemplateCode: code, RecipientList: []recipient{rc}}
	if req.ScheduledAt != nil {
		d, err := v.requestDate(*req.ScheduledAt)
		if err != nil {
			return sendBody{}, err
		}
		out.RequestDate = d
	}
	return out, nil
}

// resendOf — Fallback을 resendParameter로 옮긴다.
func resendOf(f alimtalk.Fallback, c credential) (*resendParameter, error) {
	typ := strings.ToUpper(strings.TrimSpace(f.Type))
	if typ == "" {
		typ = "SMS"
	}
	if typ != "SMS" && typ != "LMS" {
		return nil, channel.NewSendError(channel.FailurePermanentContent,
			"대체발송 유형은 SMS 또는 LMS여야 합니다 (got %q)", f.Type)
	}
	if strings.TrimSpace(f.Text) == "" {
		return nil, channel.NewSendError(channel.FailurePermanentContent, "대체발송 본문이 비어 있습니다")
	}
	no := strings.TrimSpace(f.SenderNo)
	if no == "" {
		no = strings.TrimSpace(c.SMSFallbackSender)
	}
	if no == "" {
		return nil, channel.NewSendError(channel.FailurePermanentContent,
			"대체발송 발신번호가 없습니다 — 사전 등록된 발신번호를 요청이나 커넥터 설정에 넣어야 합니다")
	}
	rp := &resendParameter{
		IsResend:      true,
		ResendType:    typ,
		ResendContent: f.Text,
		ResendSendNo:  no,
	}
	// resendTitle은 LMS에만 의미가 있다. SMS에 제목을 실으면 NHN이 거절한다.
	if typ == "LMS" {
		rp.ResendTitle = f.Title
	}
	return rp, nil
}

// kst — NHN의 requestDate는 KST 벽시계다. tzdata 유무에 영향받지 않도록 고정 오프셋을 쓴다.
var kst = time.FixedZone("KST", 9*60*60)

// requestDate — 예약 발송 시각을 "yyyy-MM-dd HH:mm"(KST)로. 과거·60일 초과는 거절한다.
func (v *Vendor) requestDate(at time.Time) (string, error) {
	now := v.clk.Now()
	if at.Before(now) {
		return "", channel.NewSendError(channel.FailurePermanentContent,
			"예약 발송 시각이 과거입니다 (%s)", at.In(kst).Format("2006-01-02 15:04"))
	}
	if at.After(now.AddDate(0, 0, MaxScheduleDaysAhead)) {
		return "", channel.NewSendError(channel.FailurePermanentContent,
			"예약 발송은 최대 %d일 뒤까지만 가능합니다", MaxScheduleDaysAhead)
	}
	return at.In(kst).Format("2006-01-02 15:04"), nil
}

// normalizeRecipient — E.164 수신번호를 NHN이 받는 국내 표기로 바꾼다.
//
// 우리 프로필의 phone은 E.164(+8210…)인데 NHN recipientNo는 하이픈 없는 국내 번호를 받는다.
// TODO(실계정 검증): 해외 번호 표기 규칙은 v2.3 문서에 명시가 없다. 지금은 +82만
// 국내 표기로 바꾸고 나머지는 숫자만 남긴다.
func normalizeRecipient(to string) (string, error) {
	digits := make([]rune, 0, len(to))
	for _, r := range strings.TrimSpace(to) {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	s := string(digits)
	if s == "" {
		return "", channel.NewSendError(channel.FailureInvalidTarget, "수신 번호가 비어 있습니다")
	}
	if strings.HasPrefix(strings.TrimSpace(to), "+82") {
		s = "0" + strings.TrimPrefix(s, "82")
	}
	if len(s) > MaxRecipientNoLen {
		return "", channel.NewSendError(channel.FailureInvalidTarget,
			"수신 번호가 %d자리로 NHN 상한(%d자리)을 넘습니다", len(s), MaxRecipientNoLen)
	}
	return s, nil
}

// checkCapabilities — manifest가 선언하지 않은 것을 보내면 스스로 거절한다.
// 선언과 구현이 어긋나면 엔진은 미지원 기능을 계속 재시도하거나 조용히 잘린 메시지를 내보낸다.
func (v *Vendor) checkCapabilities(req alimtalk.SendRequest) error {
	if len(req.Buttons) > 0 && !v.declaresContent("buttons") {
		return channel.NewSendError(channel.FailurePermanentContent, "이 벤더는 버튼을 지원하지 않습니다")
	}
	if len(req.QuickReplies) > 0 && !v.declaresContent("quick_replies") {
		return channel.NewSendError(channel.FailurePermanentContent,
			"이 벤더는 바로연결(quick_replies)을 지원하지 않습니다")
	}
	if req.Fallback != nil && !v.manifest.Capabilities.VendorFallback {
		return channel.NewSendError(channel.FailurePermanentContent, "이 벤더는 대체발송을 지원하지 않습니다")
	}
	if req.ScheduledAt != nil && !v.manifest.Capabilities.ScheduledSend {
		return channel.NewSendError(channel.FailurePermanentContent, "이 벤더는 예약 발송을 지원하지 않습니다")
	}
	return nil
}

func (v *Vendor) declaresContent(feature string) bool {
	for _, c := range v.manifest.Capabilities.Content {
		if c == feature {
			return true
		}
	}
	return false
}
