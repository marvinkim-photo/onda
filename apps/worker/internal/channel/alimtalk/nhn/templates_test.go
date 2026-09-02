package nhn

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
)

func TestListTemplatesMapping(t *testing.T) {
	v, f, base := newTestVendor(t)
	tmpls, err := v.ListTemplates(context.Background(), ValidCredential(base), ConfSenderKey)
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(tmpls) != 3 {
		t.Fatalf("템플릿 %d건: %+v", len(tmpls), tmpls)
	}
	if q := f.LastQuery; q.Get("pageNum") != "1" || q.Get("pageSize") != fmt.Sprint(TemplatePageSize) {
		t.Fatalf("페이징 질의: %v", q)
	}

	approved := tmpls[0]
	if approved.Code != ConfTemplateCode || approved.Name != "주문 접수 안내" {
		t.Fatalf("코드·이름: %+v", approved)
	}
	// Content는 필수다. 완성 본문을 요구하는 벤더로의 폴백과 콘솔 변수 매핑이 여기에 의존한다.
	if approved.Content == "" {
		t.Fatal("Content(승인 본문)가 비었다 — 렌더 원본이 없으면 rendered 벤더로 폴백할 수 없다")
	}
	if got := alimtalk.Variables(approved.Content); len(got) != 2 {
		t.Fatalf("승인 본문에서 치환자를 뽑지 못했다: %v", got)
	}
	if approved.Status != alimtalk.TemplateApproved || approved.VendorStatus != TemplateStatusApproved {
		t.Fatalf("상태 매핑: %+v", approved)
	}
	if approved.MessageType != "BA" || approved.IsAd() || approved.Category() != "transactional" {
		t.Fatalf("유형 매핑: %+v", approved)
	}
	if want := time.Date(2026, 9, 1, 12, 0, 0, 0, kst); !approved.UpdatedAt.Equal(want) {
		t.Fatalf("UpdatedAt: want %v, got %v", want, approved.UpdatedAt)
	}

	// 버튼: ordering 순으로 정렬되고, schemeIos/schemeAndroid가 LinkIOS/LinkAndroid로 온다.
	if len(approved.Buttons) != 2 {
		t.Fatalf("버튼 %d개: %+v", len(approved.Buttons), approved.Buttons)
	}
	if approved.Buttons[0].Type != "WL" || approved.Buttons[0].LinkMo != "https://m.example.com/orders" ||
		approved.Buttons[0].LinkPC != "https://example.com/orders" {
		t.Fatalf("WL 버튼: %+v", approved.Buttons[0])
	}
	al := approved.Buttons[1]
	if al.Type != "AL" || al.LinkIOS != "ondaapp://orders" || al.LinkAndroid != "ondaapp://orders" {
		t.Fatalf("AL 버튼의 schemeIos/schemeAndroid 역매핑: %+v", al)
	}

	if tmpls[1].Status != alimtalk.TemplatePending || tmpls[1].VendorStatus != TemplateStatusRequested {
		t.Fatalf("REQ → pending: %+v", tmpls[1])
	}
	if tmpls[2].Status != alimtalk.TemplateRejected || tmpls[2].VendorStatus != TemplateStatusRejected {
		t.Fatalf("REJ → rejected: %+v", tmpls[2])
	}
	if !tmpls[2].IsAd() || tmpls[2].Category() != "marketing" {
		t.Fatalf("AD는 광고성이어야 한다: %+v", tmpls[2])
	}
}

// TestTemplateStatusUnknownIsPending — 모르는 상태를 approved로 낙관하면
// 승인되지 않은 템플릿으로 발송을 시도한다.
func TestTemplateStatusUnknownIsPending(t *testing.T) {
	for _, s := range []string{"", "TSC01", "hold", "APPROVED"} {
		if got := templateStatusOf(s); got != alimtalk.TemplatePending {
			t.Fatalf("%q: want pending, got %s", s, got)
		}
	}
	if got := templateStatusOf("apr"); got != alimtalk.TemplateApproved {
		t.Fatalf("소문자도 승인으로 읽어야 한다: got %s", got)
	}
}

// TestListTemplatesPaging — pageSize를 채운 페이지가 계속되면 다음 장을 더 가져온다.
func TestListTemplatesPaging(t *testing.T) {
	v, f, base := newTestVendor(t)
	page1 := make([]map[string]any, TemplatePageSize)
	for i := range page1 {
		page1[i] = map[string]any{
			"templateCode":   fmt.Sprintf("T%04d", i),
			"templateName":   "t",
			"templateStatus": TemplateStatusApproved,
		}
	}
	page2 := []map[string]any{{"templateCode": "LAST", "templateName": "마지막", "templateStatus": TemplateStatusApproved}}
	f.TemplatePages = [][]map[string]any{page1, page2}

	tmpls, err := v.ListTemplates(context.Background(), ValidCredential(base), ConfSenderKey)
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(tmpls) != TemplatePageSize+1 {
		t.Fatalf("페이징 합계: want %d, got %d", TemplatePageSize+1, len(tmpls))
	}
	if tmpls[len(tmpls)-1].Code != "LAST" {
		t.Fatalf("마지막 페이지를 못 가져왔다: %+v", tmpls[len(tmpls)-1])
	}
	if f.TemplateCalls != 2 {
		t.Fatalf("조회 횟수: want 2, got %d", f.TemplateCalls)
	}
	if f.LastQuery.Get("pageNum") != "2" {
		t.Fatalf("2페이지 질의: %v", f.LastQuery)
	}
}

// TestListTemplatesWrapperVariants — 문서 예시가 목록을 감싸는 키가 갈려서 셋 다 받는다.
func TestListTemplatesWrapperVariants(t *testing.T) {
	for _, wrapper := range []string{"", "message", "flat"} {
		t.Run("wrapper="+wrapper, func(t *testing.T) {
			v, f, base := newTestVendor(t)
			f.TemplateWrapper = wrapper
			tmpls, err := v.ListTemplates(context.Background(), ValidCredential(base), ConfSenderKey)
			if err != nil {
				t.Fatalf("ListTemplates: %v", err)
			}
			if len(tmpls) != 3 {
				t.Fatalf("템플릿 %d건", len(tmpls))
			}
		})
	}
}

func TestListTemplatesUsesCredentialSenderKey(t *testing.T) {
	v, _, base := newTestVendor(t)
	// 인자가 비면 크리덴셜·config의 발신프로필 키로 떨어진다.
	if _, err := v.ListTemplates(context.Background(), ValidCredential(base), ""); err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	cred := alimtalk.Credential{ConnectorID: ConnectorID, JSON: CredentialJSON(ConfAppKey, ConfSecretKey, "", base)}
	_, err := v.ListTemplates(context.Background(), cred, "")
	if got := v.Classify(err); got != channel.FailureCredentialAuth {
		t.Fatalf("발신프로필 키 부재: want credential_auth, got %s (%v)", got, err)
	}
}

// TestConfigSuppliesSenderKeyAndBaseURL — 비밀이 아닌 값은 커넥터 config에서 와도 된다.
func TestConfigSuppliesSenderKeyAndBaseURL(t *testing.T) {
	v, _, base := newTestVendor(t)
	cred := alimtalk.Credential{
		ConnectorID: ConnectorID,
		JSON:        CredentialJSON(ConfAppKey, ConfSecretKey, "", ""),
		Config:      []byte(fmt.Sprintf(`{"sender_key":%q,"base_url":%q}`, ConfSenderKey, base)),
	}
	if err := v.Validate(context.Background(), cred); err != nil {
		t.Fatalf("config로 채운 크리덴셜이 통과해야 한다: %v", err)
	}
}

// TestConfigSuppliesFallbackSender — 대체발송 발신번호도 config에서 올 수 있다.
func TestConfigSuppliesFallbackSender(t *testing.T) {
	v, f, base := newTestVendor(t)
	req := sendReq(base, SuffixDelivered)
	req.Credential = alimtalk.Credential{
		ConnectorID: ConnectorID,
		JSON:        CredentialJSON(ConfAppKey, ConfSecretKey, ConfSenderKey, base),
		Config:      []byte(`{"sms_fallback_sender":"0299998888"}`),
	}
	req.Fallback = &alimtalk.Fallback{Type: "SMS", Text: "대체 본문"}
	if _, err := v.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(string(f.LastSendBody), "0299998888") {
		t.Fatalf("config의 대체발송 발신번호가 실리지 않았다: %s", f.LastSendBody)
	}
}
