package nhn

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeNHN — v2.3 스펙대로 응답하는 가짜 NHN. 실 계정이 없으므로 모든 테스트가 이걸 문다.
//
// 결과는 수신번호 끝 4자리로 조종한다(conformance_env.go의 조종표). 조종 결과는 requestId에
// 붙는 게 아니라 서버가 기억하므로, 멱등 헤더 재사용도 같은 requestId를 돌려준다 —
// 실제 NHN이 10분 창 안에서 하는 일과 같다.
type fakeNHN struct {
	mu      sync.Mutex
	records map[string]fakeRecord // requestId → 조종 결과
	idem    map[string]string     // 멱등 키 → requestId
	nextID  int

	SendCalls     int
	PollCalls     int
	TemplateCalls int

	LastSendPath   string
	LastSendHeader http.Header
	LastSendBody   []byte
	LastPollPath   string
	LastQuery      url.Values

	// TemplatePages — 페이징 테스트용. 비어 있으면 기본 픽스처 한 페이지를 준다.
	TemplatePages [][]map[string]any
	// TemplateTotal — totalCount로 실어 보낼 값. 0이면 페이지 합계를 쓴다.
	TemplateTotal int
	// TemplateWrapper — 목록을 감싸는 키("templateListResponse" | "message" | "" = 최상위).
	TemplateWrapper string
}

type fakeRecord struct {
	suffix     string
	resend     bool
	resendType string
}

func newFakeNHN(t *testing.T) (*fakeNHN, string) {
	t.Helper()
	f := &fakeNHN{records: map[string]fakeRecord{}, idem: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeNHN) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 인증: SecretKey가 틀리면 401. AppKey는 경로에 있으므로 경로 매칭이 곧 검증이다.
	if r.Header.Get(SecretKeyHeader) != ConfSecretKey {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"header": failHeader(-40010, "유효하지 않은 SecretKey입니다"),
		})
		return
	}
	prefix := "/alimtalk/" + APIVersion + "/appkeys/" + ConfAppKey
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"header": failHeader(-40004, "등록되지 않은 AppKey입니다"),
		})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	switch {
	case r.Method == http.MethodPost && rest == "/messages":
		f.handleSend(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "/messages/"):
		f.handleQuery(w, r, strings.TrimPrefix(rest, "/messages/"))
	case r.Method == http.MethodGet && strings.HasSuffix(rest, "/templates"):
		f.handleTemplates(w, r, rest)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"header": failHeader(-40004, "없는 경로")})
	}
}

func (f *fakeNHN) handleSend(w http.ResponseWriter, r *http.Request) {
	var body sendBody
	raw := readAll(r)
	_ = json.Unmarshal(raw, &body)

	f.mu.Lock()
	f.SendCalls++
	f.LastSendPath = r.URL.Path
	f.LastSendHeader = r.Header.Clone()
	f.LastSendBody = raw
	f.mu.Unlock()

	if len(body.RecipientList) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"header": failHeader(-40001, "recipientList가 비어 있습니다"),
		})
		return
	}
	rc := body.RecipientList[0]
	suffix := last4(rc.RecipientNo)

	// 조종표 중 HTTP 계층에서 끝나는 것들.
	switch suffix {
	case SuffixPermanentContent:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"header": failHeader(-40002, "템플릿 본문이 승인 내용과 일치하지 않습니다"),
		})
		return
	case SuffixAuthOn200:
		// 함정 케이스: HTTP 200인데 실패이고, 사유가 권한이다.
		writeJSON(w, http.StatusOK, map[string]any{
			"header": failHeader(-40003, "등록되지 않은 발신프로필입니다"),
		})
		return
	case SuffixForbidden:
		writeJSON(w, http.StatusForbidden, map[string]any{
			"header": failHeader(-40011, "해당 발신프로필에 대한 권한이 없습니다"),
		})
		return
	case SuffixRateLimited:
		w.Header().Set("Retry-After", "3")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"header": failHeader(-40029, "요청 한도를 초과했습니다"),
		})
		return
	case SuffixRetryable:
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"header": failHeader(-9999, "일시적인 시스템 오류입니다"),
		})
		return
	}

	// 멱등: 같은 키가 다시 오면 앞선 requestId를 그대로 돌려준다.
	f.mu.Lock()
	key := r.Header.Get(IdempotencyHeader)
	requestID, replayed := f.idem[key]
	if !replayed || key == "" {
		f.nextID++
		requestID = fmt.Sprintf("req-%08d", f.nextID)
		rec := fakeRecord{suffix: suffix}
		if rc.ResendParameter != nil && rc.ResendParameter.IsResend {
			rec.resend = true
			rec.resendType = rc.ResendParameter.ResendType
		}
		f.records[requestID] = rec
		if key != "" {
			f.idem[key] = requestID
		}
	}
	f.mu.Unlock()

	// 수신자별 거절 — 요청은 200이지만 이 수신자만 떨어진다.
	switch suffix {
	case SuffixRecipientRejected:
		writeJSON(w, http.StatusOK, sendOKBody(requestID, rc.RecipientNo, 1001, "수신번호가 올바르지 않습니다"))
		return
	case SuffixRecipientBadContent:
		writeJSON(w, http.StatusOK, sendOKBody(requestID, rc.RecipientNo, 1002, "치환 변수가 승인 본문과 다릅니다"))
		return
	}
	writeJSON(w, http.StatusOK, sendOKBody(requestID, rc.RecipientNo, 0, "성공"))
}

func (f *fakeNHN) handleQuery(w http.ResponseWriter, r *http.Request, rest string) {
	f.mu.Lock()
	f.PollCalls++
	f.LastPollPath = r.URL.Path
	f.mu.Unlock()

	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 {
		writeJSON(w, http.StatusNotFound, map[string]any{"header": failHeader(-40004, "잘못된 조회 경로")})
		return
	}
	requestID := parts[0]
	seq, _ := strconv.Atoi(parts[1])

	f.mu.Lock()
	rec, ok := f.records[requestID]
	f.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"header": failHeader(-40004, "요청을 찾을 수 없습니다"),
		})
		return
	}

	msg := map[string]any{
		"requestId":    requestID,
		"recipientSeq": seq,
		"recipientNo":  "0100000" + rec.suffix,
		"receiveDate":  "2026-09-02 19:30:11",
		"requestDate":  "2026-09-02 19:30:00",
	}
	switch {
	case rec.suffix == SuffixExpired:
		// 보존 기간이 지난 건은 조회 자체가 404다.
		writeJSON(w, http.StatusNotFound, map[string]any{
			"header": failHeader(-40004, "조회 기간이 만료되어 결과를 확인할 수 없습니다"),
		})
		return
	case rec.suffix == SuffixFallback && rec.resend:
		msg["messageStatus"] = StatusFailed
		msg["resultCode"] = ResultCodeFail
		msg["resultCodeName"] = "카카오톡 미가입 수신자"
		msg["resendStatus"] = StatusCompleted
		msg["resendType"] = rec.resendType
	case rec.suffix == SuffixFallback, rec.suffix == SuffixInvalidTarget:
		msg["messageStatus"] = StatusFailed
		msg["resultCode"] = ResultCodeFail
		msg["resultCodeName"] = "수신번호 오류 — 카카오톡 미가입"
	default:
		msg["messageStatus"] = StatusCompleted
		msg["resultCode"] = ResultCodeSuccess
		msg["resultCodeName"] = "성공"
	}
	writeJSON(w, http.StatusOK, map[string]any{"header": okHeader(), "message": msg})
}

func (f *fakeNHN) handleTemplates(w http.ResponseWriter, r *http.Request, rest string) {
	f.mu.Lock()
	f.TemplateCalls++
	f.LastQuery = r.URL.Query()
	pages := f.TemplatePages
	total := f.TemplateTotal
	wrapper := f.TemplateWrapper
	f.mu.Unlock()

	if !strings.HasPrefix(rest, "/senders/"+ConfSenderKey+"/") {
		writeJSON(w, http.StatusOK, map[string]any{
			"header": failHeader(-40012, "등록되지 않은 발신프로필 키입니다"),
		})
		return
	}
	if pages == nil {
		pages = [][]map[string]any{defaultTemplatePage()}
	}
	pageNum, _ := strconv.Atoi(r.URL.Query().Get("pageNum"))
	if pageNum < 1 {
		pageNum = 1
	}
	var items []map[string]any
	if pageNum <= len(pages) {
		items = pages[pageNum-1]
	} else {
		items = []map[string]any{}
	}
	if total == 0 {
		for _, p := range pages {
			total += len(p)
		}
	}
	block := map[string]any{"templates": items, "totalCount": total}
	out := map[string]any{"header": okHeader()}
	switch wrapper {
	case "":
		out["templateListResponse"] = block
	case "flat":
		out["templates"] = items
		out["totalCount"] = total
	default:
		out[wrapper] = block
	}
	writeJSON(w, http.StatusOK, out)
}

// defaultTemplatePage — 승인·심사중·반려가 섞인 픽스처. 버튼 필드명 매핑도 여기서 걸린다.
func defaultTemplatePage() []map[string]any {
	return []map[string]any{
		{
			"templateCode":          ConfTemplateCode,
			"templateName":          "주문 접수 안내",
			"templateContent":       "#{고객명}님, 주문 #{주문번호}이 정상 접수되었습니다.",
			"templateMessageType":   "BA",
			"templateEmphasizeType": "NONE",
			"templateStatus":        TemplateStatusApproved,
			"updateDate":            "2026-09-01 12:00:00",
			"buttons": []map[string]any{
				{"ordering": 2, "type": "AL", "name": "앱에서 보기",
					"schemeIos": "ondaapp://orders", "schemeAndroid": "ondaapp://orders"},
				{"ordering": 1, "type": "WL", "name": "주문 상세 보기",
					"linkMo": "https://m.example.com/orders", "linkPc": "https://example.com/orders"},
			},
		},
		{
			"templateCode":        "ONDA_PENDING_01",
			"templateName":        "심사 중 템플릿",
			"templateContent":     "#{고객명}님 안녕하세요.",
			"templateMessageType": "BA",
			"templateStatus":      TemplateStatusRequested,
		},
		{
			"templateCode":        "ONDA_REJECT_01",
			"templateName":        "반려된 템플릿",
			"templateContent":     "(광고) #{고객명}님 혜택 안내",
			"templateMessageType": "AD",
			"templateStatus":      TemplateStatusRejected,
			"comments": []map[string]any{
				{"id": 1, "comment": "광고 표기 누락", "status": "REJ", "createDate": "2026-08-30 10:00:00"},
			},
		},
	}
}

func sendOKBody(requestID, recipientNo string, resultCode int, resultMessage string) map[string]any {
	return map[string]any{
		"header": okHeader(),
		"message": map[string]any{
			"requestId":         requestID,
			"senderGroupingKey": "",
			"sendResults": []map[string]any{{
				"recipientSeq":  1,
				"recipientNo":   recipientNo,
				"resultCode":    resultCode,
				"resultMessage": resultMessage,
			}},
		},
	}
}

func okHeader() map[string]any {
	return map[string]any{"resultCode": 0, "resultMessage": "success", "isSuccessful": true}
}

func failHeader(code int, msg string) map[string]any {
	return map[string]any{"resultCode": code, "resultMessage": msg, "isSuccessful": false}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func readAll(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	defer func() { _ = r.Body.Close() }()
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}

func last4(s string) string {
	digits := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	if len(digits) < 4 {
		return ""
	}
	return string(digits[len(digits)-4:])
}
