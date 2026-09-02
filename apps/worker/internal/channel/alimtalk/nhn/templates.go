package nhn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
)

// nhnTemplate — 템플릿 목록 응답의 항목.
type nhnTemplate struct {
	TemplateCode          string       `json:"templateCode"`
	TemplateName          string       `json:"templateName"`
	TemplateContent       string       `json:"templateContent"`
	TemplateMessageType   string       `json:"templateMessageType"`
	TemplateEmphasizeType string       `json:"templateEmphasizeType"`
	TemplateStatus        string       `json:"templateStatus"`
	Buttons               []nhnButton  `json:"buttons"`
	Comments              []nhnComment `json:"comments"`
	QuickReplies          []nhnButton  `json:"quickReplies"`
	CreateDate            string       `json:"createDate"`
	UpdateDate            string       `json:"updateDate"`
}

// nhnComment — 심사 반려 사유 등. 우리 Template에 실을 자리는 없지만
// 파싱해 두면 반려 사유를 로그로 남길 수 있다.
type nhnComment struct {
	ID         int    `json:"id"`
	Comment    string `json:"comment"`
	Status     string `json:"status"`
	CreateDate string `json:"createDate"`
}

// templateListBlock — 목록이 담기는 블록.
type templateListBlock struct {
	Templates  []nhnTemplate `json:"templates"`
	TotalCount int           `json:"totalCount"`
}

// templateListEnvelope — 목록 응답 전체.
//
// TODO(실계정 검증): v2.3 문서는 목록을 templateListResponse 아래에 두는 예시와
// message 아래에 두는 예시가 섞여 있다. 어느 쪽이 와도 읽히게 셋 다 받는다.
type templateListEnvelope struct {
	Header               nhnHeader          `json:"header"`
	TemplateListResponse *templateListBlock `json:"templateListResponse"`
	Message              *templateListBlock `json:"message"`
	Templates            []nhnTemplate      `json:"templates"`
	TotalCount           int                `json:"totalCount"`
}

func (e templateListEnvelope) block() templateListBlock {
	switch {
	case e.TemplateListResponse != nil:
		return *e.TemplateListResponse
	case e.Message != nil:
		return *e.Message
	default:
		return templateListBlock{Templates: e.Templates, TotalCount: e.TotalCount}
	}
}

// ListTemplates — 발신프로필의 템플릿 목록. pageNum/pageSize로 끝까지 넘긴다.
//
// Content(승인 본문)를 반드시 채운다. 알리고처럼 완성 본문을 요구하는 벤더로 폴백하거나
// 콘솔이 변수 매핑 UI를 그리려면 승인 본문이 우리 쪽에 있어야 하고,
// 그래서 템플릿 동기화는 선택이 아니라 전제다(alimtalk.Render).
func (v *Vendor) ListTemplates(ctx context.Context, cred alimtalk.Credential, senderKey string) ([]alimtalk.Template, error) {
	c, err := parseCredential(cred)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(senderKey)
	if key == "" {
		key = strings.TrimSpace(c.SenderKey)
	}
	if key == "" {
		return nil, channel.NewSendError(channel.FailureCredentialAuth,
			"발신프로필 키(senderKey)가 없습니다 — 크리덴셜이나 커넥터 설정에 카카오 발신프로필 키를 넣어야 합니다")
	}

	var out []alimtalk.Template
	for page := 1; page <= maxTemplatePages; page++ {
		batch, total, err := v.fetchTemplatePage(ctx, c, key, page)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		// 마지막 페이지 판정: 한 페이지를 못 채웠거나, 총계를 이미 다 받았다.
		if len(batch) < TemplatePageSize || (total > 0 && len(out) >= total) {
			return out, nil
		}
	}
	return out, nil
}

// fetchTemplatePage — 한 페이지. Validate도 이 호출로 크리덴셜을 실검증한다(무해한 읽기).
func (v *Vendor) fetchTemplatePage(ctx context.Context, c credential, senderKey string, pageNum int) ([]alimtalk.Template, int, error) {
	q := url.Values{}
	q.Set("pageNum", fmt.Sprintf("%d", pageNum))
	q.Set("pageSize", fmt.Sprintf("%d", TemplatePageSize))
	endpoint := c.appkeyPath("/senders/"+url.PathEscape(senderKey)+"/templates") + "?" + q.Encode()

	raw, err := v.doJSON(ctx, c, http.MethodGet, endpoint, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	if _, err := checkHeader(raw); err != nil {
		return nil, 0, err
	}
	var env templateListEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, 0, channel.NewSendError(channel.FailureRetryable,
			"NHN 템플릿 목록 파싱 실패: %v (%s)", err, snippet(raw))
	}
	blk := env.block()
	out := make([]alimtalk.Template, 0, len(blk.Templates))
	for _, t := range blk.Templates {
		out = append(out, v.toTemplate(t))
	}
	return out, blk.TotalCount, nil
}

// toTemplate — NHN 템플릿을 벤더 중립 Template으로 옮긴다.
func (v *Vendor) toTemplate(t nhnTemplate) alimtalk.Template {
	out := alimtalk.Template{
		Code:          t.TemplateCode,
		Name:          t.TemplateName,
		Content:       t.TemplateContent,
		MessageType:   strings.ToUpper(strings.TrimSpace(t.TemplateMessageType)),
		EmphasizeType: strings.ToUpper(strings.TrimSpace(t.TemplateEmphasizeType)),
		Buttons:       toButtons(t.Buttons),
		QuickReplies:  toQuickReplies(t.QuickReplies),
		Status:        templateStatusOf(t.TemplateStatus),
		// 원본 상태 코드는 그대로 보존한다. 우리 3분류로 접으면 "심사 중"과
		// 벤더가 새로 만든 상태를 구분할 수 없어진다.
		VendorStatus: strings.TrimSpace(t.TemplateStatus),
		UpdatedAt:    v.templateUpdatedAt(t),
	}
	if out.EmphasizeType == "" {
		out.EmphasizeType = "NONE"
	}
	return out
}

// templateStatusOf — NHN templateStatus → 우리 3분류.
//
// REQ(심사요청)·APR(승인)·REJ(반려)가 문서에 나온 전부다. 모르는 코드는 pending으로 둔다 —
// approved로 낙관하면 승인되지 않은 템플릿으로 발송을 시도해 공급자가 거절한다.
func templateStatusOf(s string) alimtalk.TemplateStatus {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case TemplateStatusApproved:
		return alimtalk.TemplateApproved
	case TemplateStatusRejected:
		return alimtalk.TemplateRejected
	default:
		return alimtalk.TemplatePending
	}
}

// toButtons — ordering 순으로 정렬해 옮긴다. NHN이 순서를 섞어 주더라도
// 우리 쪽 배열 순서가 곧 버튼 노출 순서라 정렬이 필요하다.
func toButtons(in []nhnButton) []alimtalk.Button {
	if len(in) == 0 {
		return nil
	}
	sorted := append([]nhnButton(nil), in...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Ordering < sorted[j].Ordering })
	out := make([]alimtalk.Button, 0, len(sorted))
	for _, b := range sorted {
		out = append(out, fromNHNButton(b))
	}
	return out
}

func toQuickReplies(in []nhnButton) []alimtalk.QuickReply {
	if len(in) == 0 {
		return nil
	}
	out := make([]alimtalk.QuickReply, 0, len(in))
	for _, b := range toButtons(in) {
		out = append(out, alimtalk.QuickReply{
			Type: b.Type, Name: b.Name,
			LinkMo: b.LinkMo, LinkPC: b.LinkPC,
			LinkIOS: b.LinkIOS, LinkAndroid: b.LinkAndroid,
		})
	}
	return out
}

// templateUpdatedAt — 갱신 시각. 없으면 조회 시각으로 둔다(제로 시각은 동기화 diff를 망친다).
func (v *Vendor) templateUpdatedAt(t nhnTemplate) time.Time {
	for _, s := range []string{t.UpdateDate, t.CreateDate} {
		if ts, ok := parseNHNTime(s); ok {
			return ts
		}
	}
	return v.clk.Now()
}
