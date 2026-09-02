package message

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
	"github.com/ondahq/onda/apps/worker/internal/clock"
	"github.com/ondahq/onda/apps/worker/internal/connector"
	libqueue "github.com/ondahq/onda/packages/libqueue-go"
)

// --- 가짜 벤더 -------------------------------------------------------------
//
// Registry는 매니페스트 + init() 등록 팩토리로만 벤더를 만든다(그게 실제 배선이다).
// 테스트도 같은 경로를 쓰고, 만들어진 인스턴스를 id로 되찾아 관찰한다.

type fakeVendor struct {
	m         connector.Manifest
	lastReq   alimtalk.SendRequest
	sends     int
	sendErr   error
	class     channel.FailureClass
	pollEvent []alimtalk.Event
	pollErr   error
	validated int
}

func (f *fakeVendor) Manifest() connector.Manifest { return f.m }
func (f *fakeVendor) Validate(context.Context, alimtalk.Credential) error {
	f.validated++
	return nil
}

func (f *fakeVendor) Send(_ context.Context, req alimtalk.SendRequest) (alimtalk.Receipt, error) {
	f.lastReq = req
	f.sends++
	if f.sendErr != nil {
		return alimtalk.Receipt{}, f.sendErr
	}
	return alimtalk.Receipt{ProviderMessageID: "prov-1", MessageID: req.MessageID}, nil
}

func (f *fakeVendor) Classify(error) channel.FailureClass { return f.class }

func (f *fakeVendor) ParseCallback(context.Context, alimtalk.RawCallback) ([]alimtalk.Event, error) {
	return nil, alimtalk.ErrUnsupported
}

func (f *fakeVendor) PollResults(context.Context, alimtalk.Credential, []alimtalk.Receipt) ([]alimtalk.Event, error) {
	return f.pollEvent, f.pollErr
}

func (f *fakeVendor) ListTemplates(context.Context, alimtalk.Credential, string) ([]alimtalk.Template, error) {
	return nil, nil
}

// built — 팩토리가 만든 인스턴스. NewRegistry 이후 테스트가 여기서 꺼내 본다.
var built = map[string]*fakeVendor{}

func init() {
	for _, id := range []string{"t_variables", "t_rendered", "t_both", "t_polling", "t_fallback"} {
		alimtalk.Register(id, func(m connector.Manifest) (alimtalk.Vendor, error) {
			v := &fakeVendor{m: m, class: channel.FailureRetryable}
			built[m.ID] = v
			return v, nil
		})
	}
}

func manifest(id string, mut func(*connector.Manifest)) connector.Manifest {
	m := connector.Manifest{
		ID: id, Name: id, Version: "1.0.0", Channel: alimtalk.ChannelID, License: "Apache-2.0",
		Runtime:      connector.Runtime{Type: "in_process_go"},
		TargetTypes:  []string{"phone"},
		Capabilities: connector.Capabilities{Content: []string{"template"}},
		Credentials:  connector.SchemaBlock{Schema: json.RawMessage(`{"type":"object"}`)},
		Lifecycle:    connector.Lifecycle{Reports: []string{"sent", "delivered", "failed"}},
	}
	if mut != nil {
		mut(&m)
	}
	return m
}

func testRegistry(t *testing.T, ms ...connector.Manifest) *alimtalk.Registry {
	t.Helper()
	reg, err := alimtalk.NewRegistry(ms)
	if err != nil {
		t.Fatalf("레지스트리 생성: %v", err)
	}
	return reg
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- payload ---------------------------------------------------------------

func validPayload() Payload {
	return Payload{
		IdempotencyKey: "idem-1",
		MessageID:      "11111111-1111-4111-8111-111111111111",
		Channel:        alimtalk.ChannelID,
		Connector:      Connector{ID: "t_variables", Version: "1.0.0"},
		UserID:         "22222222-2222-4222-8222-222222222222",
		Target: Target{
			Type: "phone", EndpointID: "33333333-3333-4333-8333-333333333333",
			Value: "+821012345678",
		},
		Content: Content{Template: &Template{
			Code: "TPL_1", Variables: map[string]string{"name": "김철수"},
			SenderKey: "sk-1", RenderedPreview: "김철수님 주문이 접수됐습니다.",
		}},
		Category: "transactional",
		Consent:  Consent{Basis: "transactional"},
	}
}

func TestPayloadValidate(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Payload)
		ok   bool
	}{
		{"정상", func(*Payload) {}, true},
		{"idempotency_key 누락", func(p *Payload) { p.IdempotencyKey = "" }, false},
		{"message_id 누락", func(p *Payload) { p.MessageID = "" }, false},
		{"endpoint_id 누락", func(p *Payload) { p.Target.EndpointID = "" }, false},
		{"consent.basis 누락", func(p *Payload) { p.Consent.Basis = "" }, false},
		{"consent.basis 불명", func(p *Payload) { p.Consent.Basis = "vibes" }, false},
		{"content 비어 있음", func(p *Payload) { p.Content = Content{} }, false},
		{"connector.id 형식 오류", func(p *Payload) { p.Connector.ID = "Bad-ID" }, false},
		{"user_id 누락", func(p *Payload) { p.UserID = "" }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := validPayload()
			c.mut(&p)
			err := p.Validate()
			if c.ok != (err == nil) {
				t.Fatalf("ok=%v 기대, err=%v", c.ok, err)
			}
		})
	}
}

// 모르는 필드가 있어도 파싱은 성공해야 한다 — 스키마가 앞서가도 워커가 멈추면 안 된다.
func TestParseToleratesUnknownFields(t *testing.T) {
	p := validPayload()
	raw, _ := json.Marshal(p)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["future_field"] = "값"
	raw, _ = json.Marshal(m)
	if _, err := Parse(raw); err != nil {
		t.Fatalf("미지 필드 허용 기대, got %v", err)
	}
}

// --- 치환 모드 -------------------------------------------------------------

func newTestWorker(t *testing.T, reg *alimtalk.Registry) (*Worker, *[]lifecycleEvent) {
	t.Helper()
	var captured []lifecycleEvent
	clk := &clock.Fake{Current: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)}
	w := &Worker{
		res:    newResolver(nil, nil, clk),
		reg:    reg,
		emit:   &emitter{logger: discardLogger(), sent: func(_ *libqueue.Envelope, ev lifecycleEvent) { captured = append(captured, ev) }},
		clk:    clk,
		logger: discardLogger(),
	}
	return w, &captured
}

func jobFor(t *testing.T, reg *alimtalk.Registry, id string, p Payload) *Job {
	t.Helper()
	v, err := reg.Get(id)
	if err != nil {
		t.Fatalf("벤더 해석: %v", err)
	}
	return &Job{P: &p, ConnectorID: id, Vendor: v, Manifest: v.Manifest()}
}

// manifest.capabilities.substitution이 무엇을 싣는지 정한다.
// rendered를 요구하는 벤더에 빈 본문을 보내면 승인 템플릿과 불일치라 사고다 — 여기서 끊는다.
func TestSubstitutionMode(t *testing.T) {
	reg := testRegistry(t,
		manifest("t_variables", func(m *connector.Manifest) {
			m.Capabilities.Substitution = connector.SubstitutionVariables
		}),
		manifest("t_rendered", func(m *connector.Manifest) {
			m.Capabilities.Substitution = connector.SubstitutionRendered
		}),
		manifest("t_both", func(m *connector.Manifest) {
			m.Capabilities.Substitution = connector.SubstitutionBoth
		}),
	)
	w, _ := newTestWorker(t, reg)

	t.Run("variables는 키-값만", func(t *testing.T) {
		job := jobFor(t, reg, "t_variables", validPayload())
		req, err := w.buildRequest(job)
		if err != nil {
			t.Fatalf("빌드 실패: %v", err)
		}
		if req.Variables["name"] != "김철수" || req.RenderedText != "" {
			t.Fatalf("variables만 기대, got vars=%v rendered=%q", req.Variables, req.RenderedText)
		}
	})

	t.Run("rendered는 완성 본문만", func(t *testing.T) {
		job := jobFor(t, reg, "t_rendered", validPayload())
		req, err := w.buildRequest(job)
		if err != nil {
			t.Fatalf("빌드 실패: %v", err)
		}
		if req.RenderedText == "" || req.Variables != nil {
			t.Fatalf("rendered만 기대, got vars=%v rendered=%q", req.Variables, req.RenderedText)
		}
	})

	t.Run("both는 둘 다", func(t *testing.T) {
		job := jobFor(t, reg, "t_both", validPayload())
		req, err := w.buildRequest(job)
		if err != nil {
			t.Fatalf("빌드 실패: %v", err)
		}
		if req.RenderedText == "" || req.Variables == nil {
			t.Fatalf("둘 다 기대, got vars=%v rendered=%q", req.Variables, req.RenderedText)
		}
	})

	t.Run("rendered 요구인데 미리보기 없음 → permanent_content", func(t *testing.T) {
		p := validPayload()
		p.Content.Template.RenderedPreview = ""
		job := jobFor(t, reg, "t_rendered", p)
		_, err := w.buildRequest(job)
		if channel.Classify(err) != channel.FailurePermanentContent {
			t.Fatalf("permanent_content 기대, got %v", err)
		}
	})

	t.Run("미선언 매니페스트는 variables로 본다", func(t *testing.T) {
		reg2 := testRegistry(t, manifest("t_fallback", nil))
		job := jobFor(t, reg2, "t_fallback", validPayload())
		req, err := w.buildRequest(job)
		if err != nil {
			t.Fatalf("빌드 실패: %v", err)
		}
		if req.Variables == nil || req.RenderedText != "" {
			t.Fatalf("variables 기본값 기대, got vars=%v rendered=%q", req.Variables, req.RenderedText)
		}
	})
}

// 벤더 대체발송은 선언한 벤더에만 넘긴다. 미선언 벤더에 넘기면 조용히 사라진다.
func TestVendorFallbackOnlyWhenDeclared(t *testing.T) {
	reg := testRegistry(t,
		manifest("t_variables", nil),
		manifest("t_fallback", func(m *connector.Manifest) { m.Capabilities.VendorFallback = true }),
	)
	w, _ := newTestWorker(t, reg)
	p := validPayload()
	p.Fallback = []Step{{
		Channel: "lms",
		Content: Content{Text: &TextContent{Title: "제목", Body: "본문", Sender: "025550000"}},
		On:      []string{"invalid_target"},
	}}

	req, err := w.buildRequest(jobFor(t, reg, "t_fallback", p))
	if err != nil {
		t.Fatalf("빌드 실패: %v", err)
	}
	if req.Fallback == nil || req.Fallback.Type != "LMS" || req.Fallback.Text != "본문" {
		t.Fatalf("LMS 대체발송 기대, got %+v", req.Fallback)
	}

	req, err = w.buildRequest(jobFor(t, reg, "t_variables", p))
	if err != nil {
		t.Fatalf("빌드 실패: %v", err)
	}
	if req.Fallback != nil {
		t.Fatalf("미선언 벤더에는 대체발송을 넘기지 않아야 한다, got %+v", req.Fallback)
	}
}

func TestMapButtons(t *testing.T) {
	in := []Button{
		{Type: "web_link", Name: "주문 확인", URLMobile: "https://m.example.com"},
		{Type: "app_link", Name: "앱으로", SchemeIOS: "app://a", SchemeAndroid: "app://b"},
	}
	out, err := mapButtons(in)
	if err != nil {
		t.Fatalf("변환 실패: %v", err)
	}
	if out[0].Type != "WL" || out[0].LinkMo != "https://m.example.com" {
		t.Fatalf("WL 매핑 실패: %+v", out[0])
	}
	if out[1].Type != "AL" || out[1].LinkIOS != "app://a" {
		t.Fatalf("AL 매핑 실패: %+v", out[1])
	}
	if _, err := mapButtons([]Button{{Type: "teleport", Name: "x"}}); err == nil {
		t.Fatal("알 수 없는 버튼 타입은 오류여야 한다")
	}
}

func TestConfigStringFallsBackForSenderKey(t *testing.T) {
	reg := testRegistry(t, manifest("t_variables", nil))
	w, _ := newTestWorker(t, reg)
	p := validPayload()
	p.Content.Template.SenderKey = ""
	job := jobFor(t, reg, "t_variables", p)
	job.Config = []byte(`{"sender_key":"cfg-key"}`)
	req, err := w.buildRequest(job)
	if err != nil {
		t.Fatalf("빌드 실패: %v", err)
	}
	if req.SenderKey != "cfg-key" {
		t.Fatalf("커넥터 config의 sender_key 기대, got %q", req.SenderKey)
	}

	job.Config = nil
	if _, err := w.buildRequest(job); channel.Classify(err) != channel.FailurePermanentContent {
		t.Fatalf("발신프로필 키 없으면 permanent_content 기대, got %v", err)
	}
}

// --- 분류 ------------------------------------------------------------------

// SendHandler.Classify에는 job이 없다. 그래서 Send가 벤더 분류를 SendError에 실어 둔다.
func TestSendCarriesVendorClass(t *testing.T) {
	table := []channel.FailureClass{
		channel.FailureRetryable, channel.FailureRateLimited,
		channel.FailurePermanentContent, channel.FailureInvalidTarget, channel.FailureCredentialAuth,
	}
	for _, want := range table {
		reg := testRegistry(t, manifest("t_variables", nil))
		v := built["t_variables"]
		v.sendErr = errors.New("공급자 오류")
		v.class = want
		w, _ := newTestWorker(t, reg)
		job := jobFor(t, reg, "t_variables", validPayload())

		_, err := w.Send(context.Background(), &libqueue.Envelope{}, job, channel.Credentials{})
		if got := w.Classify(err); got != want {
			t.Fatalf("%v 기대, got %v", want, got)
		}
	}
}

// 크리덴셜은 요청마다 실어 넘긴다 — 벤더는 매니페스트로만 만들어지는 무상태 싱글턴이다.
func TestSendPassesCredential(t *testing.T) {
	reg := testRegistry(t, manifest("t_variables", nil))
	v := built["t_variables"]
	v.sendErr = nil
	w, _ := newTestWorker(t, reg)
	job := jobFor(t, reg, "t_variables", validPayload())
	job.Config = []byte(`{"a":1}`)

	id, err := w.Send(context.Background(), &libqueue.Envelope{}, job,
		channel.Credentials{Kind: "alimtalk", JSON: []byte(`{"secret":"s"}`)})
	if err != nil || id != "prov-1" {
		t.Fatalf("발송 성공·provider id 기대, got id=%q err=%v", id, err)
	}
	if string(v.lastReq.Credential.JSON) != `{"secret":"s"}` || v.lastReq.Credential.ConnectorID != "t_variables" {
		t.Fatalf("크리덴셜 전달 실패: %+v", v.lastReq.Credential)
	}
	if string(v.lastReq.Credential.Config) != `{"a":1}` {
		t.Fatalf("커넥터 config 전달 실패: %s", v.lastReq.Credential.Config)
	}
}

func TestSendRejectsForeignChannel(t *testing.T) {
	reg := testRegistry(t, manifest("t_variables", nil))
	w, _ := newTestWorker(t, reg)
	p := validPayload()
	p.Channel = "sms"
	job := jobFor(t, reg, "t_variables", p)
	if _, err := w.Send(context.Background(), &libqueue.Envelope{}, job, channel.Credentials{}); channel.Classify(err) != channel.FailurePermanentContent {
		t.Fatalf("미지원 채널은 permanent_content 기대, got %v", err)
	}
}

// --- 수명주기 --------------------------------------------------------------

func testEnv() *libqueue.Envelope {
	return &libqueue.Envelope{
		TenantID: "44444444-4444-4444-8444-444444444444",
		AppID:    "55555555-5555-4555-8555-555555555555",
		TraceID:  "trace-1",
	}
}

func TestOnTerminalEmitsSent(t *testing.T) {
	reg := testRegistry(t, manifest("t_variables", nil))
	w, captured := newTestWorker(t, reg)
	job := jobFor(t, reg, "t_variables", validPayload())
	at := w.clk.Now()

	w.OnTerminal(context.Background(), testEnv(), job, channel.SendOutcome{
		MessageID: job.P.MessageID, Status: "sent", ProviderID: "prov-1", At: at,
	})

	if len(*captured) != 1 {
		t.Fatalf("lifecycle 1건 기대, got %d", len(*captured))
	}
	ev := (*captured)[0]
	switch {
	case ev.Status != "sent":
		t.Fatalf("status sent 기대, got %q", ev.Status)
	case ev.Source != "connector":
		t.Fatalf("source connector 기대, got %q", ev.Source)
	case ev.Channel != alimtalk.ChannelID:
		t.Fatalf("channel %s 기대, got %q", alimtalk.ChannelID, ev.Channel)
	case ev.ConnectorID != "t_variables":
		t.Fatalf("connector_id 기대, got %q", ev.ConnectorID)
	case ev.ProviderMessageID == nil || *ev.ProviderMessageID != "prov-1":
		t.Fatalf("provider_message_id 기대, got %v", ev.ProviderMessageID)
	case ev.EndpointID == nil || *ev.EndpointID != job.P.Target.EndpointID:
		t.Fatalf("endpoint_id 기대, got %v", ev.EndpointID)
	case ev.UserID == nil || *ev.UserID != job.P.UserID:
		t.Fatalf("user_id 기대, got %v", ev.UserID)
	case ev.FallbackIndex == nil || *ev.FallbackIndex != 0:
		t.Fatalf("fallback_index 0 기대, got %v", ev.FallbackIndex)
	case ev.Attempt == nil || *ev.Attempt != 1:
		t.Fatalf("attempt 1 기대, got %v", ev.Attempt)
	case ev.FailureClass != nil:
		t.Fatalf("성공에 failure_class는 없어야 한다: %v", *ev.FailureClass)
	}
	if _, err := time.Parse(time.RFC3339Nano, ev.OccurredAt); err != nil {
		t.Fatalf("occurred_at RFC3339 기대: %v", err)
	}
}

// SendLoop이 만드는 내부 분류는 스키마 enum에 없다. 그대로 실으면 소비자가 통째로 버린다.
func TestOnTerminalNormalizesFailureClass(t *testing.T) {
	reg := testRegistry(t, manifest("t_variables", nil))
	cases := []struct{ in, want string }{
		{"invalid_target", "invalid_target"},
		{"credential_missing", "credential_auth"},
		{"retryable_exhausted", "retry_exhausted"},
		{"rate_limited_exhausted", "retry_exhausted"},
		{"별세계", "unsupported"},
	}
	for _, c := range cases {
		w, captured := newTestWorker(t, reg)
		job := jobFor(t, reg, "t_variables", validPayload())
		w.OnTerminal(context.Background(), testEnv(), job, channel.SendOutcome{
			Status: "failed", FailureClass: c.in, FailureDetail: "사유", Attempts: 5, At: w.clk.Now(),
		})
		ev := (*captured)[0]
		if ev.FailureClass == nil || *ev.FailureClass != c.want {
			t.Fatalf("%s → %s 기대, got %v", c.in, c.want, ev.FailureClass)
		}
		if ev.Attempt == nil || *ev.Attempt != 5 {
			t.Fatalf("attempt 5 기대, got %v", ev.Attempt)
		}
	}
}

// duplicate는 새 사실이 아니다 — 재기록 때마다 lifecycle을 또 쏘면 리포트가 부풀어 오른다.
func TestOnTerminalIgnoresDuplicate(t *testing.T) {
	reg := testRegistry(t, manifest("t_variables", nil))
	w, captured := newTestWorker(t, reg)
	job := jobFor(t, reg, "t_variables", validPayload())
	w.OnTerminal(context.Background(), testEnv(), job, channel.SendOutcome{Status: "duplicate", At: w.clk.Now()})
	if len(*captured) != 0 {
		t.Fatalf("duplicate에는 발행하지 않아야 한다, got %d", len(*captured))
	}
}

// --- 크리덴셜 미해석 --------------------------------------------------------

func TestResolveCredentialMissing(t *testing.T) {
	reg := testRegistry(t, manifest("t_variables", nil))
	w, captured := newTestWorker(t, reg)
	env := testEnv()
	p := validPayload()
	// 배선 없음을 캐시에 심어 pg 없이 재현한다.
	w.res.seed(env.TenantID, env.AppID, p.Channel, binding{}, false, "채널 kakao_alimtalk 커넥터 배선 없음(channel_connectors)")

	job := &Job{P: &p}
	_, found, err := w.Resolve(context.Background(), env, job)
	if err != nil || found {
		t.Fatalf("미해석(found=false, err=nil) 기대, got found=%v err=%v", found, err)
	}
	if job.Note == "" {
		t.Fatal("사유(Note)가 남아야 콘솔에서 원인을 안다")
	}

	// SendLoop은 이 경우 credential_missing으로 종결한다.
	w.OnTerminal(context.Background(), env, job, channel.SendOutcome{
		Status: "failed", FailureClass: "credential_missing",
		FailureDetail: "크리덴셜 미등록/미검증", At: w.clk.Now(),
	})
	ev := (*captured)[0]
	if ev.FailureClass == nil || *ev.FailureClass != "credential_auth" {
		t.Fatalf("credential_auth 기대, got %v", ev.FailureClass)
	}
	if ev.FailureDetail == nil || *ev.FailureDetail == "" {
		t.Fatal("사유가 detail에 실려야 한다")
	}
	if ev.ConnectorID != "t_variables" {
		t.Fatalf("해석 실패에도 payload의 커넥터를 남겨야 한다, got %q", ev.ConnectorID)
	}
}

// 커넥터는 저니가 못박은 payload의 connector.id가 우선이다.
// 배선은 못박힌 커넥터가 없거나 사라졌을 때의 되돌림 경로다.
func TestResolvePrefersPinnedConnector(t *testing.T) {
	reg := testRegistry(t, manifest("t_variables", nil), manifest("t_rendered", nil))
	w, _ := newTestWorker(t, reg)
	env := testEnv()

	t.Run("못박힌 커넥터를 쓴다", func(t *testing.T) {
		p := validPayload()
		p.Connector.ID = "t_rendered"
		w.res.seed(env.TenantID, env.AppID, p.Channel,
			binding{ConnectorID: "t_variables", Credential: []byte("{}")}, true, "")
		job := &Job{P: &p}
		if _, found, err := w.Resolve(context.Background(), env, job); err != nil || !found {
			t.Fatalf("해석 성공 기대, got found=%v err=%v", found, err)
		}
		if job.ConnectorID != "t_rendered" || job.Manifest.ID != "t_rendered" {
			t.Fatalf("못박힌 t_rendered 기대, got %q", job.ConnectorID)
		}
	})

	t.Run("못박힌 커넥터가 없으면 배선으로 되돌린다", func(t *testing.T) {
		p := validPayload()
		p.Connector.ID = "t_polling" // 레지스트리에 없다
		w.res.seed(env.TenantID, env.AppID, p.Channel,
			binding{ConnectorID: "t_variables", Credential: []byte("{}")}, true, "")
		job := &Job{P: &p}
		if _, found, err := w.Resolve(context.Background(), env, job); err != nil || !found {
			t.Fatalf("배선으로 되돌아가 성공해야 한다, got found=%v err=%v", found, err)
		}
		if job.ConnectorID != "t_variables" {
			t.Fatalf("배선 t_variables 기대, got %q", job.ConnectorID)
		}
	})

	t.Run("둘 다 없으면 종결", func(t *testing.T) {
		p := validPayload()
		p.Connector.ID = "t_polling"
		w.res.seed(env.TenantID, env.AppID, p.Channel,
			binding{ConnectorID: "t_both", Credential: []byte("{}")}, true, "")
		job := &Job{P: &p}
		if _, found, _ := w.Resolve(context.Background(), env, job); found {
			t.Fatal("해석할 벤더가 없으면 found=false여야 한다")
		}
		if job.Note == "" {
			t.Fatal("사유가 남아야 한다")
		}
	})
}

func TestResolveSuccess(t *testing.T) {
	reg := testRegistry(t, manifest("t_variables", nil))
	w, _ := newTestWorker(t, reg)
	env := testEnv()
	p := validPayload()
	w.res.seed(env.TenantID, env.AppID, p.Channel, binding{
		ConnectorID: "t_variables", Config: []byte(`{"sender_key":"cfg"}`), Credential: []byte(`{"k":"v"}`),
	}, true, "")

	job := &Job{P: &p}
	creds, found, err := w.Resolve(context.Background(), env, job)
	if err != nil || !found {
		t.Fatalf("해석 성공 기대, got found=%v err=%v", found, err)
	}
	if creds.Kind != "alimtalk" || string(creds.JSON) != `{"k":"v"}` {
		t.Fatalf("크리덴셜 전달 실패: %+v", creds)
	}
	if job.Vendor == nil || job.ConnectorID != "t_variables" || job.Manifest.ID != "t_variables" {
		t.Fatalf("벤더 해석 결과가 job에 실려야 Send가 이어받는다: %+v", job)
	}
}

// --- message_log 행 ---------------------------------------------------------

func TestRowShape(t *testing.T) {
	reg := testRegistry(t, manifest("t_variables", nil))
	w, _ := newTestWorker(t, reg)
	env := testEnv()
	job := jobFor(t, reg, "t_variables", validPayload())
	at := w.clk.Now()

	row := w.Row(env, job, channel.SendOutcome{Status: "sent", ProviderID: "prov-1", At: at})
	if len(row) != 16 {
		t.Fatalf("message_log 16개 값 기대, got %d", len(row))
	}
	want := []any{
		env.TenantID, env.AppID, job.P.MessageID, job.P.IdempotencyKey,
		zeroUUID, uint32(0), uint16(0), "",
		job.P.UserID, job.P.Target.EndpointID, alimtalk.ChannelID,
		"sent", "", "", at, "prov-1",
	}
	for i := range want {
		if row[i] != want[i] {
			t.Fatalf("row[%d]: %v 기대, got %v", i, want[i], row[i])
		}
	}
}

// endpoint_id는 device_id 자리에 들어간다(멱등 키의 마지막 요소·엔드포인트 판별자).
// UUID가 아니면 행을 잃느니 zero UUID로 적재한다.
func TestRowNonUUIDEndpoint(t *testing.T) {
	reg := testRegistry(t, manifest("t_variables", nil))
	w, _ := newTestWorker(t, reg)
	p := validPayload()
	p.Target.EndpointID = "endpoint-abc"
	p.UserID = "user-abc"
	job := jobFor(t, reg, "t_variables", p)

	row := w.Row(testEnv(), job, channel.SendOutcome{Status: "sent", At: w.clk.Now()})
	if row[8] != zeroUUID || row[9] != zeroUUID {
		t.Fatalf("비UUID는 zero UUID로 적재해야 한다, got user=%v device=%v", row[8], row[9])
	}
}

func TestRowJourneyFields(t *testing.T) {
	reg := testRegistry(t, manifest("t_variables", nil))
	w, _ := newTestWorker(t, reg)
	p := validPayload()
	jid := "66666666-6666-4666-8666-666666666666"
	ver, node := 3, 7
	ref := "camp-1"
	p.JourneyID, p.JourneyVersion, p.NodeIndex, p.CampaignRef = &jid, &ver, &node, &ref
	job := jobFor(t, reg, "t_variables", p)

	row := w.Row(testEnv(), job, channel.SendOutcome{Status: "sent", At: w.clk.Now()})
	if row[4] != jid || row[5] != uint32(3) || row[6] != uint16(7) || row[7] != "camp-1" {
		t.Fatalf("저니 필드 매핑 실패: %v %v %v %v", row[4], row[5], row[6], row[7])
	}
}

// 해석 실패의 사유는 SendLoop의 고정 문구보다 유용하다 — 로그에도 사유를 남긴다.
func TestRowUsesResolveNote(t *testing.T) {
	reg := testRegistry(t, manifest("t_variables", nil))
	w, _ := newTestWorker(t, reg)
	p := validPayload()
	job := &Job{P: &p, Note: "채널 배선 없음"}
	row := w.Row(testEnv(), job, channel.SendOutcome{
		Status: "failed", FailureClass: "credential_missing", FailureDetail: "크리덴셜 미등록/미검증", At: w.clk.Now(),
	})
	if row[13] != "채널 배선 없음" {
		t.Fatalf("사유가 실려야 한다, got %v", row[13])
	}
}

func TestKeyPrefixDoesNotCollide(t *testing.T) {
	w := &Worker{}
	if w.KeyPrefix() != "send:message" {
		t.Fatalf("send:message 기대, got %q", w.KeyPrefix())
	}
}

func TestParseRejectsBadPayload(t *testing.T) {
	w, _ := newTestWorker(t, testRegistry(t))
	env := &libqueue.Envelope{ID: "e1", Payload: json.RawMessage(`{"idempotency_key":""}`)}
	if _, _, _, ok := w.Parse(env); ok {
		t.Fatal("불량 payload는 ok=false여야 SendLoop이 ACK 후 버린다")
	}
	p := validPayload()
	raw, _ := json.Marshal(p)
	job, idem, mid, ok := w.Parse(&libqueue.Envelope{Payload: raw})
	if !ok || idem != p.IdempotencyKey || mid != p.MessageID || job.P == nil {
		t.Fatalf("정상 파싱 기대: ok=%v idem=%q mid=%q", ok, idem, mid)
	}
}

// --- 크리덴셜 검증 어댑터 ---------------------------------------------------

func TestCredentialPluginPicksVendor(t *testing.T) {
	reg := testRegistry(t, manifest("t_variables", nil))
	p := NewCredentialPlugin(reg)
	// 벤더가 하나뿐이면 connector_id 없이도 검증한다.
	if err := p.ValidateCredentials(context.Background(), channel.Credentials{JSON: []byte(`{}`)}); err != nil {
		t.Fatalf("단일 벤더 검증 기대, got %v", err)
	}
	if built["t_variables"].validated == 0 {
		t.Fatal("벤더 Validate에 위임해야 한다")
	}

	reg2 := testRegistry(t, manifest("t_variables", nil), manifest("t_rendered", nil))
	p2 := NewCredentialPlugin(reg2)
	err := p2.ValidateCredentials(context.Background(), channel.Credentials{JSON: []byte(`{}`)})
	if channel.Classify(err) != channel.FailureCredentialAuth {
		t.Fatalf("벤더 다수 + connector_id 없음은 credential_auth 기대, got %v", err)
	}
	// connector_id를 실으면 그 벤더로 간다.
	if err := p2.ValidateCredentials(context.Background(),
		channel.Credentials{JSON: []byte(`{"connector_id":"t_rendered"}`)}); err != nil {
		t.Fatalf("지정 벤더 검증 기대, got %v", err)
	}
}

func TestCredentialPluginSendUnsupported(t *testing.T) {
	p := NewCredentialPlugin(testRegistry(t))
	if _, err := p.Send(context.Background(), channel.SendRequest{}); err == nil {
		t.Fatal("이 어댑터의 Send는 지원 경로가 아니다")
	}
}
