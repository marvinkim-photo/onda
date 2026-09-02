// Package nhn — NHN Cloud KakaoTalk Bizmessage v2.3 알림톡 벤더.
//
// 공식 문서 실측(2026-09-02) 기준이며, 알림톡 벤더 변이 네 축에서 이 벤더의 좌표는 다음과 같다.
//
//  1. 치환: variables. NHN이 templateParameter를 받아 승인 본문에 직접 렌더한다.
//     완성 본문(RenderedText)을 보내면 안 되고, 보내도 무시된다.
//  2. 대체발송: 공급자가 한다(resendParameter). 엔진 폴백 체인은 이 벤더에 돌지 않는다.
//  3. 결과 수집: 폴링. NHN은 발송 결과 웹훅을 발행하지 않으므로 ParseCallback은
//     ErrUnsupported이고, 단건 조회 API를 우리가 당겨야 한다.
//  4. 식별자: 복합키. requestId(요청 단위) + recipientSeq(요청 안의 수신자 순번)라야
//     한 건을 특정할 수 있다. Receipt.ProviderMessageID 한 문자열에 "requestId:recipientSeq"로
//     접어 넣고 PollResults가 되편다(EncodeReceiptID/DecodeReceiptID).
//
// # 검증되지 않았다 — ASSUMPTIONS.md를 먼저 읽어라
//
// 이 구현은 공식 문서만 보고 작성됐고 실 계정으로 호출해 검증한 적이 없다. 문서가 말하지 않아
// 우리가 고른 지점이 22개 있고, 각각 틀렸을 때 어떤 증상으로 드러나는지까지 같은 디렉터리의
// ASSUMPTIONS.md에 적어 두었다. 실 계정이 생기면 그 순서대로 확인하고, 이 벤더에서 원인을
// 모를 증상(전량 미종결·도달률이 조용히 어긋남·틀린 키가 verified로 남음)을 만나면 거기서
// 역으로 찾아라. 테스트가 초록인 것은 우리 해석 안에서 일관되다는 뜻일 뿐이다.
//
// # 인증
//
// AppKey는 URL 경로에, SecretKey는 X-Secret-Key 헤더에 싣는다.
// 둘 중 하나라도 없으면 네트워크를 타기 전에 credential_auth로 끊는다.
//
// # 분류의 함정
//
// channel.Verifier.judge는 invalid_target·permanent_content를 "인증은 통과"로 읽는다.
// NHN은 잘못된 키·권한 없는 발신프로필을 HTTP 200 + header.isSuccessful=false로 알려주기도 하므로,
// 상태 코드만 보고 분류하면 틀린 키가 verified로 저장된다(Resend에서 실제로 겪은 함정).
// 그래서 본문의 결과 메시지까지 읽어 인증·권한 냄새가 나면 무조건 credential_auth로 떨어뜨린다.
package nhn

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
	"github.com/ondahq/onda/apps/worker/internal/clock"
	"github.com/ondahq/onda/apps/worker/internal/connector"
)

// ConnectorID — 레지스트리 키. manifest.json의 id와 같아야 한다.
const ConnectorID = "alimtalk_nhn"

// DefaultBaseURL — 공식 엔드포인트. kakaotalk-bizmessage.api.nhncloudservice.com도 같은 서비스를
// 가리키지만 문서의 예제 URL이 이쪽이라 기본값으로 둔다. 크리덴셜·config의 base_url로 덮어쓸 수 있다
// (계약 테스트가 httptest 서버를 물리는 유일한 방법이기도 하다).
const DefaultBaseURL = "https://api-alimtalk.cloud.toast.com"

// APIVersion — 경로에 박히는 버전. 상수로 두어 v2.4 이관 시 한 곳만 고치게 한다.
const APIVersion = "v2.3"

// IdempotencyHeader — NHN 멱등 헤더. 같은 값이 10분 안에 다시 오면 새 발송을 만들지 않고
// 앞선 요청의 결과를 그대로 돌려준다. 여기에 SendRequest.MessageID를 싣는다.
const IdempotencyHeader = "X-NC-API-IDEMPOTENCY-KEY"

// SecretKeyHeader — NHN 인증 헤더.
const SecretKeyHeader = "X-Secret-Key"

// NHN 제약 (v2.3 문서).
const (
	MaxRecipients        = 1000 // recipientList
	MaxTemplateCodeLen   = 20
	MaxRecipientNoLen    = 15
	MaxScheduleDaysAhead = 60
	TemplatePageSize     = 1000
	// maxTemplatePages — 페이징 무한루프 방지. 1페이지 1,000건이므로 실 계정에서 닿을 수 없다.
	maxTemplatePages = 50
)

// 단건 조회의 결과 코드. MRC01=성공, MRC02=실패 (v2.3 문서).
const (
	ResultCodeSuccess = "MRC01"
	ResultCodeFail    = "MRC02"
)

// messageStatus 값.
const (
	StatusCompleted = "COMPLETED"
	StatusFailed    = "FAILED"
	StatusCancel    = "CANCEL"
)

// templateStatus 값 → alimtalk.TemplateStatus.
const (
	TemplateStatusRequested = "REQ" // 심사요청
	TemplateStatusApproved  = "APR" // 승인
	TemplateStatusRejected  = "REJ" // 반려
)

// 대체발송 채널 이름. Event.DeliveredVia와 message.lifecycle.v1의 channel 값이다.
const (
	ChannelSMS = "sms"
	ChannelLMS = "lms"
)

// 대체발송 단가(KRW). NHN은 조회 응답에 건당 원가를 싣지 않으므로 상수로 둔다.
// 알림톡 단가(manifest.cost)와 달라야 채널별 원가 집계가 맞는다 — SMS 전환이 공짜로 보이면 안 된다.
// TODO(실계정): NHN 공표 단가로 교체한다.
const (
	FallbackCostSMS = 20.0
	FallbackCostLMS = 50.0
)

// defaultRateLimitRetryAfter — 429인데 Retry-After가 없을 때의 대기 시간.
// 0을 그대로 흘리면 백오프가 지수 재시도로 되돌아가 한도 초과가 길게 이어진다.
const defaultRateLimitRetryAfter = 5 * time.Second

// defaultTimeout — manifest.runtime.timeout_ms가 없을 때의 HTTP 타임아웃.
const defaultTimeout = 10 * time.Second

//go:embed manifest.json
var manifestJSON []byte

// EmbeddedManifest — 파일 경로 없이 이 벤더를 세울 수 있게 내장 manifest를 판다.
func EmbeddedManifest() (connector.Manifest, error) {
	m, err := connector.Parse(manifestJSON)
	if err != nil {
		return connector.Manifest{}, fmt.Errorf("nhn: 내장 manifest 파싱: %w", err)
	}
	return m, nil
}

func init() { alimtalk.Register(ConnectorID, New) }

// Vendor — NHN Cloud 알림톡 벤더. 무상태 싱글턴이라 앱별 비밀은 요청마다 실려 온다.
type Vendor struct {
	manifest connector.Manifest
	clk      clock.Clock
	hc       *http.Client
}

var _ alimtalk.Vendor = (*Vendor)(nil)

// New — alimtalk.Factory 구현. 실시계와 manifest 타임아웃을 쓴다.
func New(m connector.Manifest) (alimtalk.Vendor, error) { return NewWithClock(m, clock.Real{}, nil) }

// NewWithClock — 시계·HTTP 클라이언트 주입 생성자. hc가 nil이면 manifest 타임아웃으로 하나 만든다.
func NewWithClock(m connector.Manifest, clk clock.Clock, hc *http.Client) (*Vendor, error) {
	if clk == nil {
		return nil, fmt.Errorf("nhn: clock이 nil")
	}
	if m.ID == "" {
		var err error
		if m, err = EmbeddedManifest(); err != nil {
			return nil, err
		}
	}
	if m.ID != ConnectorID {
		return nil, fmt.Errorf("nhn: manifest id가 %q여야 한다 (got %q)", ConnectorID, m.ID)
	}
	if m.Channel != alimtalk.ChannelID {
		return nil, fmt.Errorf("nhn: channel이 %q여야 한다 (got %q)", alimtalk.ChannelID, m.Channel)
	}
	if hc == nil {
		hc = &http.Client{Timeout: timeoutOf(m)}
	}
	return &Vendor{manifest: m, clk: clk, hc: hc}, nil
}

func timeoutOf(m connector.Manifest) time.Duration {
	if m.Runtime.TimeoutMS > 0 {
		return time.Duration(m.Runtime.TimeoutMS) * time.Millisecond
	}
	return defaultTimeout
}

// Manifest — 생성 시 받은 선언을 그대로 돌려준다.
func (v *Vendor) Manifest() connector.Manifest { return v.manifest }

// Classify — 이 벤더가 내는 오류는 전부 channel.SendError라 공용 분류기로 충분하다.
// 전송 계층 오류(SendError가 아닌 것)는 channel.Classify가 retryable로 본다.
func (v *Vendor) Classify(err error) channel.FailureClass { return channel.Classify(err) }

// credential — manifest.credentials.schema의 Go 투영.
//
// sender_key·base_url은 비밀이 아니라 config(channel_connectors.config)에도 올 수 있다.
// 크리덴셜 쪽 값이 우선이고, 없으면 config에서 채운다.
type credential struct {
	AppKey    string `json:"app_key"`
	SecretKey string `json:"secret_key"`
	SenderKey string `json:"sender_key,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
	// SMSFallbackSender — 비밀이 아니라 크리덴셜 스키마에는 없고 config에서만 채워진다.
	SMSFallbackSender string `json:"-"`
}

// config — 비밀이 아닌 앱 단위 설정.
type vendorConfig struct {
	SenderKey         string `json:"sender_key,omitempty"`
	BaseURL           string `json:"base_url,omitempty"`
	SMSFallbackSender string `json:"sms_fallback_sender,omitempty"`
}

// parseCredential — 크리덴셜+config를 합쳐 검증한다.
//
// app_key·secret_key 누락은 반드시 credential_auth다. permanent_content로 새면
// 크리덴셜 정지가 걸리지 않아 운영자가 발송이 멈춘 뒤에야 알게 된다.
func parseCredential(cred alimtalk.Credential) (credential, error) {
	if cred.ConnectorID != "" && cred.ConnectorID != ConnectorID {
		return credential{}, channel.NewSendError(channel.FailureCredentialAuth,
			"크리덴셜의 커넥터가 %q라 이 벤더(%s)와 맞지 않습니다", cred.ConnectorID, ConnectorID)
	}
	if len(cred.JSON) == 0 {
		return credential{}, channel.NewSendError(channel.FailureCredentialAuth, "크리덴셜이 비어 있습니다")
	}
	var c credential
	if err := json.Unmarshal(cred.JSON, &c); err != nil {
		return credential{}, channel.NewSendError(channel.FailureCredentialAuth, "크리덴셜 JSON 파싱 실패: %v", err)
	}
	if len(cred.Config) > 0 {
		var cfg vendorConfig
		if err := json.Unmarshal(cred.Config, &cfg); err != nil {
			return credential{}, channel.NewSendError(channel.FailurePermanentContent, "커넥터 config JSON 파싱 실패: %v", err)
		}
		if c.SenderKey == "" {
			c.SenderKey = cfg.SenderKey
		}
		if c.BaseURL == "" {
			c.BaseURL = cfg.BaseURL
		}
		c.SMSFallbackSender = cfg.SMSFallbackSender
	}
	if strings.TrimSpace(c.AppKey) == "" {
		return credential{}, channel.NewSendError(channel.FailureCredentialAuth,
			"NHN Cloud AppKey(app_key)가 비어 있습니다")
	}
	if strings.TrimSpace(c.SecretKey) == "" {
		return credential{}, channel.NewSendError(channel.FailureCredentialAuth,
			"NHN Cloud SecretKey(secret_key)가 비어 있습니다")
	}
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	return c, nil
}

// Validate — 크리덴셜 실검증. 템플릿 목록 조회(무해한 읽기)로 확인한다.
//
// 발신프로필 키가 크리덴셜·config 어디에도 없으면 조회할 대상이 없다. 이때 "설정 오류"로
// 돌려주면 verifier가 verified로 저장하므로(judge가 permanent_content를 인증 통과로 읽는다)
// credential_auth로 끊는다.
func (v *Vendor) Validate(ctx context.Context, cred alimtalk.Credential) error {
	c, err := parseCredential(cred)
	if err != nil {
		return err
	}
	if strings.TrimSpace(c.SenderKey) == "" {
		return channel.NewSendError(channel.FailureCredentialAuth,
			"발신프로필 키(sender_key)가 없습니다 — 크리덴셜이나 커넥터 설정에 카카오 발신프로필 키를 넣어야 검증할 수 있습니다")
	}
	_, _, err = v.fetchTemplatePage(ctx, c, c.SenderKey, 1)
	return err
}

// baseURLOf — 크리덴셜에서 정규화된 베이스 URL.
func (c credential) appkeyPath(suffix string) string {
	return fmt.Sprintf("%s/alimtalk/%s/appkeys/%s%s", c.BaseURL, APIVersion, url.PathEscape(c.AppKey), suffix)
}

// header — 모든 NHN 응답의 공통 머리. 성패의 1차 근거는 HTTP 상태가 아니라 여기다.
type nhnHeader struct {
	ResultCode    flexInt `json:"resultCode"`
	ResultMessage string  `json:"resultMessage"`
	IsSuccessful  bool    `json:"isSuccessful"`
}

// flexInt — 숫자로도 문자열로도 오는 결과 코드. NHN 문서 예제가 두 표기를 섞어 쓰고
// 서비스별로도 갈려서, 파싱 실패로 전체 응답을 잃지 않게 둘 다 받는다.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		*f = 0
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		str = strings.TrimSpace(str)
		if str == "" {
			*f = 0
			return nil
		}
		n, err := strconv.Atoi(str)
		if err != nil {
			// MRC01처럼 숫자가 아닌 코드는 0으로 두고 문자열 필드가 따로 받는다.
			*f = 0
			return nil
		}
		*f = flexInt(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexInt(n)
	return nil
}

// doJSON — NHN 호출 한 번 + HTTP 상태 분류까지.
//
// HTTP 계층에서 이미 결론이 난 오류(401·429·5xx)는 여기서 분류까지 마쳐 돌려주고,
// 본문을 읽어야 아는 것은 호출부가 checkHeader로 판정한다.
func (v *Vendor) doJSON(ctx context.Context, c credential, method, endpoint string, body any, extraHeaders map[string]string) ([]byte, error) {
	resp, raw, err := v.doRaw(ctx, c, method, endpoint, body, extraHeaders)
	if err != nil {
		return raw, err
	}
	if err := v.classifyHTTP(resp, raw); err != nil {
		return raw, err
	}
	return raw, nil
}

// doRaw — 상태 코드까지 그대로 돌려주는 저수준 호출.
//
// 폴링은 404를 "실패"가 아니라 "보존 기간이 지나 결과를 알 수 없다"(Event.Expired)로 읽어야 해서
// 상태 코드를 직접 봐야 한다. doJSON의 분류를 그대로 쓰면 404가 invalid_target으로 굳어
// 알 수 없는 건이 실패로 집계된다.
//
// 반환 오류는 전송 계층 실패뿐이다(재시도 대상).
func (v *Vendor) doRaw(ctx context.Context, c credential, method, endpoint string, body any, extraHeaders map[string]string) (*http.Response, []byte, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, nil, channel.NewSendError(channel.FailurePermanentContent, "요청 본문 직렬화 실패: %v", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return nil, nil, channel.NewSendError(channel.FailurePermanentContent, "요청 생성 실패: %v", err)
	}
	req.Header.Set(SecretKeyHeader, c.SecretKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	}
	for k, val := range extraHeaders {
		req.Header.Set(k, val)
	}

	resp, err := v.hc.Do(req)
	if err != nil {
		// 전송 실패는 재시도 대상이다. 멱등 헤더가 있어 재발송해도 중복이 나지 않는다.
		return nil, nil, channel.NewSendError(channel.FailureRetryable, "NHN 호출 실패: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if readErr != nil {
		return nil, nil, channel.NewSendError(channel.FailureRetryable, "NHN 응답 읽기 실패: %v", readErr)
	}
	return resp, raw, nil
}

// classifyHTTP — HTTP 상태에서 곧바로 결론이 나는 오류를 분류한다.
// 2xx면 nil을 돌려주고 본문 판정은 호출부에 넘긴다.
func (v *Vendor) classifyHTTP(resp *http.Response, raw []byte) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	h, _ := decodeHeader(raw)
	detail := h.ResultMessage
	if detail == "" {
		detail = snippet(raw)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return channel.NewSendError(channel.FailureCredentialAuth,
			"NHN이 인증을 거절했습니다 (HTTP %d): %s", resp.StatusCode, detail)
	case resp.StatusCode == http.StatusTooManyRequests:
		return channel.NewRateLimitError(v.retryAfterOf(resp.Header.Get("Retry-After")),
			"NHN 발송 한도를 초과했습니다 (HTTP 429): %s", detail)
	case resp.StatusCode >= 500:
		return channel.NewSendError(channel.FailureRetryable,
			"NHN 일시 장애 (HTTP %d): %s", resp.StatusCode, detail)
	case resp.StatusCode == http.StatusNotFound:
		return channel.NewSendError(channel.FailureInvalidTarget,
			"NHN에서 대상을 찾을 수 없습니다 (HTTP 404): %s", detail)
	default:
		// 400·409 등. 본문 메시지가 인증·권한을 가리키면 credential_auth로 올려야 한다.
		// 여기서 permanent_content로 흘리면 verifier가 틀린 키를 verified로 저장한다.
		return channel.NewSendError(classifyMessage(int(h.ResultCode), detail, channel.FailurePermanentContent),
			"NHN이 요청을 거절했습니다 (HTTP %d, resultCode=%d): %s", resp.StatusCode, int(h.ResultCode), detail)
	}
}

// checkHeader — 2xx 응답의 header.isSuccessful을 본다.
//
// NHN은 HTTP 200으로 실패를 알려주는 경우가 있고, 그중에는 잘못된 SecretKey·권한 없는
// 발신프로필처럼 반드시 credential_auth여야 하는 것이 섞여 있다. 상태 코드만 보면 놓친다.
func checkHeader(raw []byte) (nhnHeader, error) {
	h, err := decodeHeader(raw)
	if err != nil {
		return h, channel.NewSendError(channel.FailureRetryable, "NHN 응답 파싱 실패: %v (%s)", err, snippet(raw))
	}
	if h.IsSuccessful {
		return h, nil
	}
	return h, channel.NewSendError(classifyMessage(int(h.ResultCode), h.ResultMessage, channel.FailurePermanentContent),
		"NHN이 요청을 거절했습니다 (resultCode=%d): %s", int(h.ResultCode), h.ResultMessage)
}

func decodeHeader(raw []byte) (nhnHeader, error) {
	var envelope struct {
		Header nhnHeader `json:"header"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nhnHeader{}, err
	}
	return envelope.Header, nil
}

func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// retryAfterOf — Retry-After 헤더(초 또는 HTTP-date)를 대기 시간으로 바꾼다.
// 값이 없거나 0 이하면 기본값을 준다 — 0을 흘리면 공급자가 준 대기 시간을 버리는 셈이 된다.
// HTTP-date 해석에 주입된 시계를 쓴다(CLAUDE.md 규칙 3 — time.Now()/time.Until 직접 호출 금지).
func (v *Vendor) retryAfterOf(h string) time.Duration {
	if h = strings.TrimSpace(h); h != "" {
		if secs, err := strconv.Atoi(h); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
		if t, err := http.ParseTime(h); err == nil {
			if d := t.Sub(v.clk.Now()); d > 0 {
				return d
			}
		}
	}
	return defaultRateLimitRetryAfter
}
