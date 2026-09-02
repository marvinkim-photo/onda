package mock

import (
	"context"
	"time"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
)

// 픽스처 템플릿 코드. 계약 테스트와 E2E가 이 코드로 발송한다.
const (
	// TemplateOrder — BA 기본형(정보성). Category()가 transactional이라 야간 제한 예외다.
	TemplateOrder = "ONDA_ORDER_01"
	// TemplateDelivery — EX 부가정보형(정보성). DS 배송조회 버튼을 단다.
	TemplateDelivery = "ONDA_DELIVERY_01"
	// TemplatePromo — AD 광고추가형(광고성). IsAd()가 true라 AC(채널추가) 버튼이 허용된다.
	TemplatePromo = "ONDA_PROMO_01"
)

// templates — 승인 템플릿 픽스처. 상태를 갖지 않도록 매번 새로 만든다.
//
// 본문에 #{변수}가 있어 alimtalk.Variables·Render가 실제로 걸리고,
// BA/EX/AD가 섞여 있어 Template.Category()·IsAd()가 다운스트림에서 갈린다.
func fixtures(now time.Time) []alimtalk.Template {
	return []alimtalk.Template{
		{
			Code:          TemplateOrder,
			Name:          "주문 접수 안내",
			Content:       "#{고객명}님, 주문 #{주문번호}이 정상 접수되었습니다.\n결제금액: #{결제금액}원\n\n주문 상세는 아래 버튼에서 확인하실 수 있습니다.",
			MessageType:   "BA",
			EmphasizeType: "NONE",
			Buttons: []alimtalk.Button{
				{Type: "WL", Name: "주문 상세 보기", LinkMo: "https://m.example.com/orders", LinkPC: "https://example.com/orders"},
			},
			Status:       alimtalk.TemplateApproved,
			VendorStatus: "APPROVED",
			UpdatedAt:    now,
		},
		{
			Code:          TemplateDelivery,
			Name:          "배송 출발 안내",
			Content:       "#{고객명}님, 주문하신 상품이 출발했습니다.\n택배사: #{택배사}\n송장번호: #{송장번호}",
			MessageType:   "EX",
			EmphasizeType: "TEXT",
			Buttons: []alimtalk.Button{
				{Type: "DS", Name: "배송 조회"},
			},
			Status:       alimtalk.TemplateApproved,
			VendorStatus: "APPROVED",
			UpdatedAt:    now,
		},
		{
			Code:          TemplatePromo,
			Name:          "쿠폰 발급 안내(광고)",
			Content:       "(광고) #{고객명}님께 #{쿠폰명} 쿠폰을 드립니다.\n사용기한: #{사용기한}\n\n무료수신거부 080-000-0000",
			MessageType:   "AD",
			EmphasizeType: "TEXT",
			Buttons: []alimtalk.Button{
				{Type: "WL", Name: "쿠폰 받기", LinkMo: "https://m.example.com/coupon", LinkPC: "https://example.com/coupon"},
				{Type: "AC", Name: "채널 추가"},
			},
			QuickReplies: []alimtalk.QuickReply{
				{Type: "WL", Name: "수신 거부", LinkMo: "https://m.example.com/optout"},
			},
			Status:       alimtalk.TemplateApproved,
			VendorStatus: "APPROVED",
			UpdatedAt:    now,
		},
	}
}

func (v *Vendor) templates() []alimtalk.Template { return fixtures(v.clk.Now()) }

func (v *Vendor) template(code string) (alimtalk.Template, bool) {
	for _, t := range v.templates() {
		if t.Code == code {
			return t, true
		}
	}
	return alimtalk.Template{}, false
}

// SampleVariables — 주어진 템플릿의 치환자를 모두 채운 값. 계약 테스트·E2E가 쓴다.
// 모르는 코드면 nil이라 호출부가 오타를 바로 알아챈다.
func SampleVariables(code string) map[string]string {
	pool := map[string]string{
		"고객명":  "홍길동",
		"주문번호": "20260902-0001",
		"결제금액": "38,000",
		"택배사":  "CJ대한통운",
		"송장번호": "123456789012",
		"쿠폰명":  "가을 10% 할인",
		"사용기한": "2026-09-30",
	}
	for _, t := range fixtures(time.Time{}) {
		if t.Code != code {
			continue
		}
		out := map[string]string{}
		for _, name := range alimtalk.Variables(t.Content) {
			out[name] = pool[name]
		}
		return out
	}
	return nil
}

// SampleRendered — 승인 본문에 SampleVariables를 치환한 완성 텍스트.
// substitution이 rendered|both인 벤더의 RenderedText로 쓴다.
func SampleRendered(code string) string {
	for _, t := range fixtures(time.Time{}) {
		if t.Code != code {
			continue
		}
		out, err := alimtalk.Render(t.Content, SampleVariables(code))
		if err != nil {
			return ""
		}
		return out
	}
	return ""
}

// ListTemplates — 승인 템플릿 목록. senderKey는 검증만 하고 필터에는 쓰지 않는다
// (픽스처는 발신프로필 하나만 가정한다).
func (v *Vendor) ListTemplates(_ context.Context, cred alimtalk.Credential, senderKey string) ([]alimtalk.Template, error) {
	c, err := parseCredential(cred)
	if err != nil {
		return nil, err
	}
	if senderKey != "" && c.SenderKey != "" && senderKey != c.SenderKey {
		return nil, channel.NewSendError(channel.FailurePermanentContent,
			"등록되지 않은 발신프로필 키입니다: %s", senderKey)
	}
	return v.templates(), nil
}
