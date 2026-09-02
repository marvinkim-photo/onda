package alimtalk

import (
	"strings"
	"testing"

	"github.com/ondahq/onda/apps/worker/internal/connector"
)

func TestVariables(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{"등장 순서 유지", "#{고객명}님 #{주문번호} 건", []string{"고객명", "주문번호"}},
		{"중복 제거", "#{a}/#{b}/#{a}", []string{"a", "b"}},
		{"공백 제거", "#{ 고객명 }", []string{"고객명"}},
		{"빈 치환자 무시", "#{ }", nil},
		{"닫히지 않음", "#{고객명", nil},
		{"치환자 없음", "안내드립니다.", nil},
		{"50자 초과 이름은 치환자가 아니다", "#{" + strings.Repeat("가", 51) + "}", nil},
		{"우리 저니 표기는 잡지 않는다", "{{name}}", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Variables(tc.content)
			if len(got) != len(tc.want) {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("want %v, got %v", tc.want, got)
				}
			}
		})
	}
}

func TestRender(t *testing.T) {
	t.Run("정상", func(t *testing.T) {
		got, err := Render("#{고객명}님, 주문 #{주문번호} 접수", map[string]string{
			"고객명": "홍길동", "주문번호": "A-1",
		})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if got != "홍길동님, 주문 A-1 접수" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("반복 치환자도 모두 채운다", func(t *testing.T) {
		got, err := Render("#{n}-#{n}", map[string]string{"n": "7"})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if got != "7-7" {
			t.Fatalf("got %q", got)
		}
	})

	// 미치환 변수는 빈 문자열이 아니라 오류다. 승인 본문과 어긋난 메시지가 나가면 사고다.
	t.Run("값 누락", func(t *testing.T) {
		_, err := Render("#{고객명}님 #{주문번호}", map[string]string{"고객명": "홍길동"})
		if err == nil {
			t.Fatal("누락된 치환자는 오류여야 한다")
		}
		if !strings.Contains(err.Error(), "주문번호") {
			t.Fatalf("누락된 이름이 메시지에 있어야 한다: %v", err)
		}
	})

	t.Run("빈 값도 누락", func(t *testing.T) {
		if _, err := Render("#{n}", map[string]string{"n": ""}); err == nil {
			t.Fatal("빈 값은 누락으로 봐야 한다")
		}
	})

	t.Run("누락 이름 중복 제거", func(t *testing.T) {
		_, err := Render("#{n}#{n}", nil)
		if err == nil {
			t.Fatal("오류여야 한다")
		}
		if strings.Count(err.Error(), "n") != 1 {
			t.Fatalf("이름이 중복 보고됐다: %v", err)
		}
	})

	t.Run("치환 후 본문 상한", func(t *testing.T) {
		long := strings.Repeat("가", MaxBodyRunes)
		if _, err := Render("#{v}", map[string]string{"v": long}); err != nil {
			t.Fatalf("정확히 %d자는 통과해야 한다: %v", MaxBodyRunes, err)
		}
		if _, err := Render("#{v}!", map[string]string{"v": long}); err == nil {
			t.Fatalf("%d자 초과는 오류여야 한다", MaxBodyRunes)
		}
	})
}

func TestValidateButtons(t *testing.T) {
	wl := Button{Type: "WL", Name: "주문 상세", LinkMo: "https://m.example.com"}
	cases := []struct {
		name    string
		buttons []Button
		isAd    bool
		wantErr string
	}{
		{"정상 WL", []Button{wl}, false, ""},
		{"버튼 없음", nil, false, ""},
		{"WL에 link_mo 누락", []Button{{Type: "WL", Name: "주문 상세"}}, false, "link_mo"},
		{"AL 링크 1개", []Button{{Type: "AL", Name: "앱 열기", LinkIOS: "app://x"}}, false, "링크 2개"},
		{"AL 링크 2개", []Button{{Type: "AL", Name: "앱 열기", LinkIOS: "app://x", LinkAndroid: "app://y"}}, false, ""},
		{"AC는 정보성 템플릿 불가", []Button{{Type: "AC", Name: "채널 추가"}}, false, "광고추가형"},
		{"AC는 광고성이면 허용", []Button{{Type: "AC", Name: "채널 추가"}}, true, ""},
		{"알 수 없는 타입", []Button{{Type: "ZZ", Name: "x"}}, false, "알 수 없는 타입"},
		{"이름 누락", []Button{{Type: "DS"}}, false, "이름 누락"},
		{"이름 28자", []Button{{Type: "DS", Name: strings.Repeat("가", MaxButtonNameRunes)}}, false, ""},
		{"이름 29자", []Button{{Type: "DS", Name: strings.Repeat("가", MaxButtonNameRunes+1)}}, false, "상한"},
		{"5개", []Button{wl, wl, wl, wl, wl}, false, ""},
		{"6개", []Button{wl, wl, wl, wl, wl, wl}, false, "최대 5개"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateButtons(tc.buttons, tc.isAd)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("통과해야 한다: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%q를 담은 오류여야 한다", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q in error, got %v", tc.wantErr, err)
			}
		})
	}
}

func approvedTemplate() Template {
	return Template{
		Code:        "T1",
		Content:     "#{고객명}님, 주문 #{주문번호} 접수",
		MessageType: "BA",
		Status:      TemplateApproved,
	}
}

func validSendRequest() SendRequest {
	return SendRequest{
		MessageID:    "m1",
		SenderKey:    "sk",
		TemplateCode: "T1",
		To:           "+821000000001",
		Variables:    map[string]string{"고객명": "홍길동", "주문번호": "A-1"},
	}
}

func TestValidateSend(t *testing.T) {
	t.Run("정상", func(t *testing.T) {
		if err := ValidateSend(approvedTemplate(), validSendRequest(), connector.SubstitutionVariables); err != nil {
			t.Fatalf("통과해야 한다: %v", err)
		}
	})

	cases := []struct {
		name    string
		tmpl    func(*Template)
		req     func(*SendRequest)
		wantErr string
	}{
		{"미승인 템플릿", func(tm *Template) { tm.Status = TemplatePending }, nil, "승인되지 않은"},
		{"반려 템플릿", func(tm *Template) { tm.Status = TemplateRejected }, nil, "승인되지 않은"},
		{"발신프로필 누락", nil, func(r *SendRequest) { r.SenderKey = "" }, "발신프로필"},
		{"템플릿 코드 누락", nil, func(r *SendRequest) { r.TemplateCode = "" }, "템플릿 코드"},
		{"수신번호 누락", nil, func(r *SendRequest) { r.To = "" }, "수신 번호"},
		{"치환자 누락", nil, func(r *SendRequest) { delete(r.Variables, "주문번호") }, "치환자 값 누락"},
		{"치환자 빈 값", nil, func(r *SendRequest) { r.Variables["주문번호"] = "" }, "치환자 값 누락"},
		{"버튼 규칙 위반", nil, func(r *SendRequest) {
			r.Buttons = []Button{{Type: "WL", Name: "링크"}}
		}, "link_mo"},
		{"정보성 템플릿에 AC 버튼", nil, func(r *SendRequest) {
			r.Buttons = []Button{{Type: "AC", Name: "채널 추가"}}
		}, "광고추가형"},
		{"바로연결 초과", nil, func(r *SendRequest) {
			r.QuickReplies = make([]QuickReply, MaxQuickReplies+1)
		}, "바로연결"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tm := approvedTemplate()
			req := validSendRequest()
			if tc.tmpl != nil {
				tc.tmpl(&tm)
			}
			if tc.req != nil {
				tc.req(&req)
			}
			err := ValidateSend(tm, req, connector.SubstitutionVariables)
			if err == nil {
				t.Fatalf("%q를 담은 오류여야 한다", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q in error, got %v", tc.wantErr, err)
			}
		})
	}

	// 광고성 템플릿이면 AC 버튼이 통과한다 — IsAd()가 ValidateSend까지 흐르는지 확인.
	t.Run("광고성 템플릿의 AC 버튼", func(t *testing.T) {
		tm := approvedTemplate()
		tm.MessageType = "AD"
		req := validSendRequest()
		req.Buttons = []Button{{Type: "AC", Name: "채널 추가"}}
		if err := ValidateSend(tm, req, connector.SubstitutionVariables); err != nil {
			t.Fatalf("통과해야 한다: %v", err)
		}
	})
}

func TestTemplateCategory(t *testing.T) {
	cases := map[string]struct {
		isAd     bool
		category string
	}{
		"BA": {false, "transactional"},
		"EX": {false, "transactional"},
		"AD": {true, "marketing"},
		"MI": {true, "marketing"},
		"":   {false, "transactional"},
	}
	for mt, want := range cases {
		tm := Template{MessageType: mt}
		if tm.IsAd() != want.isAd {
			t.Fatalf("%s: IsAd want %v", mt, want.isAd)
		}
		if tm.Category() != want.category {
			t.Fatalf("%s: Category want %q, got %q", mt, want.category, tm.Category())
		}
	}
}
