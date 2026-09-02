package alimtalk

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ondahq/onda/apps/worker/internal/connector"
)

// 카카오 원본 제약 (벤더 무관 — 어느 딜러사를 쓰든 동일하다).
const (
	MaxBodyRunes       = 1000 // 알림톡 본문
	MaxButtons         = 5
	MaxButtonNameRunes = 28
	MaxQuickReplies    = 10 // 템플릿 등록 상한 (발송 시 노출은 2개)
)

// varPattern — 카카오 치환자 표기 #{변수명}. 우리 저니의 {{변수}}와 다르므로 경계에서 변환한다.
var varPattern = regexp.MustCompile(`#\{([^}]{1,50})\}`)

// Variables — 승인 본문에서 치환자 이름을 추출한다(등장 순서, 중복 제거).
// 콘솔의 변수 매핑 UI와 alimtalk_templates.variables가 이 목록을 쓴다.
func Variables(content string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range varPattern.FindAllStringSubmatch(content, -1) {
		name := strings.TrimSpace(m[1])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// Render — 승인 본문에 변수를 치환해 완성 텍스트를 만든다.
//
// 알리고처럼 완성 본문을 요구하는 벤더(substitution=rendered)를 지원하려면 승인 본문이
// 우리 쪽에 있어야 하고, 그래서 템플릿 동기화가 선택이 아니라 전제다.
// 미치환 변수는 빈 문자열이 아니라 오류다 — 알림톡은 승인 본문과 정확히 일치해야 하고,
// 빈 값이 들어가면 공급자가 거절하거나(다행) 이상한 문구가 발송된다(사고).
func Render(content string, vars map[string]string) (string, error) {
	var missing []string
	out := varPattern.ReplaceAllStringFunc(content, func(tok string) string {
		name := strings.TrimSpace(varPattern.FindStringSubmatch(tok)[1])
		v, ok := vars[name]
		if !ok || v == "" {
			missing = append(missing, name)
			return tok
		}
		return v
	})
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("치환자 값 누락: %s", strings.Join(dedupe(missing), ", "))
	}
	if n := utf8.RuneCountInString(out); n > MaxBodyRunes {
		return "", fmt.Errorf("치환 후 본문이 %d자로 상한(%d자)을 넘는다", n, MaxBodyRunes)
	}
	return out, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// buttonLinks — 버튼 타입별 필수 링크. 카카오 정의를 그대로 따른다.
var buttonLinks = map[string]func(Button) error{
	"WL": func(b Button) error {
		if b.LinkMo == "" {
			return fmt.Errorf("WL 버튼은 link_mo가 필수")
		}
		return nil
	},
	"AL": func(b Button) error {
		n := 0
		for _, l := range []string{b.LinkIOS, b.LinkAndroid, b.LinkMo, b.LinkPC} {
			if l != "" {
				n++
			}
		}
		if n < 2 {
			return fmt.Errorf("AL 버튼은 링크 2개 이상이 필수")
		}
		return nil
	},
	"DS": func(Button) error { return nil },
	"BK": func(Button) error { return nil },
	"MD": func(Button) error { return nil },
	"BC": func(Button) error { return nil },
	"BT": func(Button) error { return nil },
	"AC": func(Button) error { return nil },
}

// ValidateButtons — 카카오 버튼 규칙. AC(채널추가)는 광고추가형·복합형에서만 허용된다.
func ValidateButtons(buttons []Button, isAd bool) error {
	if len(buttons) > MaxButtons {
		return fmt.Errorf("버튼은 최대 %d개 (got %d)", MaxButtons, len(buttons))
	}
	for i, b := range buttons {
		check, ok := buttonLinks[b.Type]
		if !ok {
			return fmt.Errorf("버튼 %d: 알 수 없는 타입 %q", i+1, b.Type)
		}
		if b.Name == "" {
			return fmt.Errorf("버튼 %d: 이름 누락", i+1)
		}
		if n := utf8.RuneCountInString(b.Name); n > MaxButtonNameRunes {
			return fmt.Errorf("버튼 %d: 이름이 %d자로 상한(%d자)을 넘는다", i+1, n, MaxButtonNameRunes)
		}
		if b.Type == "AC" && !isAd {
			return fmt.Errorf("버튼 %d: AC(채널추가)는 광고추가형·복합형 템플릿만 쓸 수 있다", i+1)
		}
		if err := check(b); err != nil {
			return fmt.Errorf("버튼 %d: %w", i+1, err)
		}
	}
	return nil
}

// ValidateSend — 발송 직전 벤더 무관 검증. 벤더에 보내기 전에 걸러 공급자 거절과 과금을 아낀다.
//
// mode는 이 벤더의 치환 방식(manifest.SubstitutionMode())이다. 완성 본문만 보내는 벤더(rendered)는
// 치환자 맵이 비어 있어도 정상이므로 RenderedText가 있는지를 대신 본다.
func ValidateSend(t Template, req SendRequest, mode connector.Substitution) error {
	if t.Status != TemplateApproved {
		return fmt.Errorf("승인되지 않은 템플릿입니다 (status=%s)", t.Status)
	}
	if req.SenderKey == "" {
		return fmt.Errorf("발신프로필 키 누락")
	}
	if req.TemplateCode == "" {
		return fmt.Errorf("템플릿 코드 누락")
	}
	if req.To == "" {
		return fmt.Errorf("수신 번호 누락")
	}
	switch mode {
	case connector.SubstitutionRendered:
		if strings.TrimSpace(req.RenderedText) == "" {
			return fmt.Errorf("완성 본문(RenderedText) 누락 — 이 벤더는 승인 본문과 일치하는 전문을 요구한다")
		}
	default: // variables · both — 공급자가 렌더하므로 치환자가 모두 있어야 한다
		for _, name := range Variables(t.Content) {
			if v, ok := req.Variables[name]; !ok || v == "" {
				return fmt.Errorf("치환자 값 누락: %s", name)
			}
		}
	}
	if err := ValidateButtons(req.Buttons, t.IsAd()); err != nil {
		return err
	}
	if len(req.QuickReplies) > MaxQuickReplies {
		return fmt.Errorf("바로연결은 최대 %d개 (got %d)", MaxQuickReplies, len(req.QuickReplies))
	}
	return nil
}
