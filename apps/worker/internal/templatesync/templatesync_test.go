package templatesync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
	"github.com/ondahq/onda/apps/worker/internal/clock"
	"github.com/ondahq/onda/apps/worker/internal/connector"
	"github.com/ondahq/onda/apps/worker/internal/message"
	libqueue "github.com/ondahq/onda/packages/libqueue-go"
)

const (
	tenantID = "44444444-4444-4444-8444-444444444444"
	appID    = "55555555-5555-4555-8555-555555555555"
	senderID = "66666666-6666-4666-8666-666666666666"
	connID   = "t_sync"
)

// --- 가짜 벤더 -------------------------------------------------------------

type fakeVendor struct {
	calls     int
	senderKey string
	tmpls     []alimtalk.Template
	// errs — 호출 순서대로 돌려줄 오류(nil이면 성공). 길이를 넘으면 성공.
	errs  []error
	class channel.FailureClass
}

func (f *fakeVendor) Manifest() connector.Manifest                        { return connector.Manifest{ID: connID} }
func (f *fakeVendor) Validate(context.Context, alimtalk.Credential) error { return nil }
func (f *fakeVendor) Send(context.Context, alimtalk.SendRequest) (alimtalk.Receipt, error) {
	return alimtalk.Receipt{}, nil
}
func (f *fakeVendor) Classify(error) channel.FailureClass { return f.class }
func (f *fakeVendor) ParseCallback(context.Context, alimtalk.RawCallback) ([]alimtalk.Event, error) {
	return nil, alimtalk.ErrUnsupported
}
func (f *fakeVendor) PollResults(context.Context, alimtalk.Credential, []alimtalk.Receipt) ([]alimtalk.Event, error) {
	return nil, alimtalk.ErrUnsupported
}
func (f *fakeVendor) ListTemplates(_ context.Context, _ alimtalk.Credential, senderKey string) ([]alimtalk.Template, error) {
	f.senderKey = senderKey
	f.calls++
	if f.calls <= len(f.errs) {
		if err := f.errs[f.calls-1]; err != nil {
			return nil, err
		}
	}
	return f.tmpls, nil
}

// fakeVendors — connector_id → 벤더.
type fakeVendors map[string]alimtalk.Vendor

func (f fakeVendors) Get(id string) (alimtalk.Vendor, error) {
	v, ok := f[id]
	if !ok {
		return nil, alimtalk.ErrNotFound
	}
	return v, nil
}

// --- 가짜 배선 해석기 -------------------------------------------------------

type fakeResolver struct {
	b     message.Binding
	found bool
	note  string
	err   error
}

func (f *fakeResolver) Resolve(context.Context, string, string, string) (message.Binding, bool, string, error) {
	return f.b, f.found, f.note, f.err
}

// --- 가짜 PG ----------------------------------------------------------------

type fakeStore struct {
	upsertScope Scope
	rows        []Row
	upsertErr   error
	missingArgs []string
	missingCall int
	missingErr  error
}

func (s *fakeStore) Upsert(_ context.Context, scope Scope, rows []Row, _ time.Time) (int, error) {
	s.upsertScope = scope
	s.rows = rows
	if s.upsertErr != nil {
		return 0, s.upsertErr
	}
	return len(rows), nil
}

func (s *fakeStore) MarkMissing(_ context.Context, _ Scope, codes []string, _ time.Time) (int, error) {
	s.missingCall++
	s.missingArgs = codes
	if s.missingErr != nil {
		return 0, s.missingErr
	}
	return 1, nil
}

// --- 하네스 -----------------------------------------------------------------

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newSyncer(t *testing.T, v *fakeVendor, res *fakeResolver, store *fakeStore) (*Syncer, *int) {
	t.Helper()
	clk := &clock.Fake{Current: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)}
	slept := 0
	s := NewSyncer(res, fakeVendors{connID: v}, store, clk, discardLogger())
	s.sleep = func(context.Context, time.Duration) error { slept++; return nil }
	return s, &slept
}

func wired() *fakeResolver {
	return &fakeResolver{
		b:     message.Binding{ConnectorID: connID, Config: []byte(`{}`), Credential: []byte(`{"api_key":"k"}`)},
		found: true,
	}
}

func envFor(t *testing.T, p Payload) *libqueue.Envelope {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("payload 직렬화: %v", err)
	}
	return &libqueue.Envelope{
		ID: "msg-1", Type: "alimtalk.template.sync", SchemaVer: 1,
		TenantID: tenantID, AppID: appID, TraceID: "trace-1", Payload: raw,
	}
}

func validPayload() Payload {
	return Payload{
		AppID: appID, SenderID: senderID, SenderKey: "sk-1", ConnectorID: connID,
		RequestedBy: nil, RequestedAt: "2026-09-02T00:00:00Z",
	}
}

// mutate — 유효 payload에서 한 곳만 망가뜨린다.
func mutate(f func(*Payload)) Payload {
	p := validPayload()
	f(&p)
	return p
}

func approved(code, content string) alimtalk.Template {
	return alimtalk.Template{
		Code: code, Name: code, Content: content, MessageType: "BA", EmphasizeType: "NONE",
		Status: alimtalk.TemplateApproved, VendorStatus: "APR",
	}
}

// --- 테스트 -----------------------------------------------------------------

func TestSyncUpsertsFetchedTemplates(t *testing.T) {
	v := &fakeVendor{tmpls: []alimtalk.Template{
		approved("TPL_A", "#{고객명}님 주문 #{주문번호}이 접수되었습니다."),
		{Code: "TPL_B", Content: "심사중", Status: alimtalk.TemplatePending, VendorStatus: "REQ"},
	}}
	store := &fakeStore{}
	s, _ := newSyncer(t, v, wired(), store)

	sum, err := s.Sync(context.Background(), envFor(t, validPayload()))
	if err != nil {
		t.Fatalf("동기화 실패: %v", err)
	}
	switch {
	case sum.Fetched != 2:
		t.Fatalf("fetched 2 기대, got %d", sum.Fetched)
	case sum.Upserted != 2:
		t.Fatalf("upserted 2 기대, got %d", sum.Upserted)
	case sum.Approved != 1:
		t.Fatalf("approved 1 기대, got %d", sum.Approved)
	case v.senderKey != "sk-1":
		t.Fatalf("벤더에 sender_key가 전달되지 않았다: %q", v.senderKey)
	}
	if store.upsertScope != (Scope{TenantID: tenantID, AppID: appID, SenderID: senderID}) {
		t.Fatalf("upsert 스코프가 다르다: %+v", store.upsertScope)
	}
	// 치환자는 승인 본문에서 뽑는다 — 콘솔 변수 매핑 UI가 읽는 컬럼이다.
	if got := store.rows[0].Variables; len(got) != 2 || got[0] != "고객명" || got[1] != "주문번호" {
		t.Fatalf("치환자 추출이 다르다: %v", got)
	}
	if store.rows[0].Buttons != "[]" || store.rows[0].QuickReplies != "[]" {
		t.Fatalf("빈 버튼은 jsonb 빈 배열이어야 한다: %q %q", store.rows[0].Buttons, store.rows[0].QuickReplies)
	}
	if store.rows[1].Status != "pending" {
		t.Fatalf("status pending 기대, got %q", store.rows[1].Status)
	}
}

// 사라진 템플릿은 지우지 않고 표시만 한다 — 저니가 참조 중일 수 있다.
func TestSyncMarksMissingOnlyWhenListNonEmpty(t *testing.T) {
	store := &fakeStore{}
	v := &fakeVendor{tmpls: []alimtalk.Template{approved("TPL_A", "본문")}}
	s, _ := newSyncer(t, v, wired(), store)
	sum, err := s.Sync(context.Background(), envFor(t, validPayload()))
	if err != nil {
		t.Fatalf("동기화 실패: %v", err)
	}
	if store.missingCall != 1 || len(store.missingArgs) != 1 || store.missingArgs[0] != "TPL_A" {
		t.Fatalf("남길 코드 목록이 다르다: call=%d args=%v", store.missingCall, store.missingArgs)
	}
	if sum.Missing != 1 {
		t.Fatalf("missing 1 기대, got %d", sum.Missing)
	}

	// 빈 목록을 "전부 사라졌다"로 읽으면 멀쩡한 저니가 통째로 막힌다.
	empty := &fakeStore{}
	s2, _ := newSyncer(t, &fakeVendor{}, wired(), empty)
	if _, err := s2.Sync(context.Background(), envFor(t, validPayload())); err != nil {
		t.Fatalf("동기화 실패: %v", err)
	}
	if empty.missingCall != 0 {
		t.Fatalf("빈 응답에서는 사라짐 표시를 하면 안 된다 (call=%d)", empty.missingCall)
	}
}

func TestSyncBadPayloadIsAckedNotRetried(t *testing.T) {
	store := &fakeStore{}
	s, _ := newSyncer(t, &fakeVendor{}, wired(), store)

	cases := map[string]*libqueue.Envelope{
		"json 불량":         {ID: "m", TenantID: tenantID, AppID: appID, Payload: json.RawMessage(`{`)},
		"sender_key 누락":   envFor(t, mutate(func(p *Payload) { p.SenderKey = "" })),
		"sender_id 비UUID": envFor(t, mutate(func(p *Payload) { p.SenderID = "nope" })),
		"connector_id 형식": envFor(t, mutate(func(p *Payload) { p.ConnectorID = "NHN" })),
		"app_id 불일치":      envFor(t, mutate(func(p *Payload) { p.AppID = "77777777-7777-4777-8777-777777777777" })),
	}
	for name, env := range cases {
		sum, err := s.Sync(context.Background(), env)
		if err != nil {
			t.Fatalf("%s: 불량 payload는 재시도 대상이 아니다: %v", name, err)
		}
		if sum.Skipped != "invalid_payload" {
			t.Fatalf("%s: skipped invalid_payload 기대, got %q", name, sum.Skipped)
		}
	}
	if store.rows != nil {
		t.Fatal("불량 payload가 PG에 닿았다")
	}
}

func TestSyncSkipsWhenUnwired(t *testing.T) {
	store := &fakeStore{}
	res := &fakeResolver{found: false, note: "채널 kakao_alimtalk 커넥터 배선 없음(channel_connectors)"}
	s, _ := newSyncer(t, &fakeVendor{}, res, store)

	sum, err := s.Sync(context.Background(), envFor(t, validPayload()))
	if err != nil {
		t.Fatalf("미배선은 재시도 대상이 아니다: %v", err)
	}
	if sum.Skipped != "unwired" || store.rows != nil {
		t.Fatalf("미배선 처리가 다르다: %+v rows=%v", sum, store.rows)
	}
}

func TestSyncRetriesTransientVendorError(t *testing.T) {
	transient := errors.New("502 bad gateway")
	v := &fakeVendor{
		errs:  []error{transient, nil},
		tmpls: []alimtalk.Template{approved("TPL_A", "본문")},
		class: channel.FailureRetryable,
	}
	s, slept := newSyncer(t, v, wired(), &fakeStore{})
	sum, err := s.Sync(context.Background(), envFor(t, validPayload()))
	if err != nil {
		t.Fatalf("두 번째 시도에서 성공해야 한다: %v", err)
	}
	if v.calls != 2 || *slept != 1 || sum.Fetched != 1 {
		t.Fatalf("재시도 동작이 다르다: calls=%d slept=%d sum=%+v", v.calls, *slept, sum)
	}
}

// 재시도를 다 써도 일시 오류면 ack하지 않는다 — 리클레임이 다시 집는다.
func TestSyncExhaustedRetriesIsRetryable(t *testing.T) {
	transient := errors.New("timeout")
	v := &fakeVendor{errs: []error{transient, transient, transient}, class: channel.FailureRetryable}
	store := &fakeStore{}
	s, slept := newSyncer(t, v, wired(), store)

	if _, err := s.Sync(context.Background(), envFor(t, validPayload())); err == nil {
		t.Fatal("일시 오류 소진은 재시도(error)여야 한다")
	}
	if v.calls != maxVendorAttempts || *slept != maxVendorAttempts-1 {
		t.Fatalf("시도 횟수가 다르다: calls=%d slept=%d", v.calls, *slept)
	}
	if store.rows != nil {
		t.Fatal("실패한 조회 결과가 PG에 반영됐다")
	}
}

// 크리덴셜 오류는 재시도해도 같다 — 큐에 남겨 두면 리클레임이 영원히 돈다.
func TestSyncPermanentVendorErrorIsAcked(t *testing.T) {
	v := &fakeVendor{errs: []error{errors.New("401 unauthorized")}, class: channel.FailureCredentialAuth}
	s, slept := newSyncer(t, v, wired(), &fakeStore{})

	sum, err := s.Sync(context.Background(), envFor(t, validPayload()))
	if err != nil {
		t.Fatalf("영구 오류는 재시도 대상이 아니다: %v", err)
	}
	if sum.Skipped != "permanent_error" || v.calls != 1 || *slept != 0 {
		t.Fatalf("영구 오류 처리가 다르다: sum=%+v calls=%d slept=%d", sum, v.calls, *slept)
	}
}

func TestSyncUnsupportedVendorIsAcked(t *testing.T) {
	v := &fakeVendor{errs: []error{alimtalk.ErrUnsupported}, class: channel.FailureRetryable}
	s, _ := newSyncer(t, v, wired(), &fakeStore{})
	sum, err := s.Sync(context.Background(), envFor(t, validPayload()))
	if err != nil {
		t.Fatalf("미지원은 재시도 대상이 아니다: %v", err)
	}
	if sum.Skipped != "unsupported" || v.calls != 1 {
		t.Fatalf("미지원 처리가 다르다: sum=%+v calls=%d", sum, v.calls)
	}
}

// 발행 시점 커넥터와 현재 배선이 다르면 배선을 따른다 — 지금 발송에 쓰이는 벤더의 템플릿이어야 한다.
func TestSyncFollowsBindingOnConnectorDrift(t *testing.T) {
	v := &fakeVendor{tmpls: []alimtalk.Template{approved("TPL_A", "본문")}}
	p := validPayload()
	p.ConnectorID = "t_old_vendor"
	s, _ := newSyncer(t, v, wired(), &fakeStore{})

	sum, err := s.Sync(context.Background(), envFor(t, p))
	if err != nil {
		t.Fatalf("동기화 실패: %v", err)
	}
	if v.calls != 1 || sum.Fetched != 1 {
		t.Fatalf("배선 커넥터로 조회해야 한다: calls=%d sum=%+v", v.calls, sum)
	}
}

func TestSyncUnknownConnectorIsAcked(t *testing.T) {
	res := wired()
	res.b.ConnectorID = "t_not_registered"
	s, _ := newSyncer(t, &fakeVendor{}, res, &fakeStore{})
	sum, err := s.Sync(context.Background(), envFor(t, validPayload()))
	if err != nil {
		t.Fatalf("미등록 커넥터는 재시도 대상이 아니다: %v", err)
	}
	if sum.Skipped != "unknown_connector" {
		t.Fatalf("skipped unknown_connector 기대, got %q", sum.Skipped)
	}
}

func TestSyncStoreErrorIsRetryable(t *testing.T) {
	v := &fakeVendor{tmpls: []alimtalk.Template{approved("TPL_A", "본문")}}
	s, _ := newSyncer(t, v, wired(), &fakeStore{upsertErr: errors.New("pg down")})
	if _, err := s.Sync(context.Background(), envFor(t, validPayload())); err == nil {
		t.Fatal("적재 실패는 재시도(error)여야 한다")
	}
}

func TestSyncResolverErrorIsRetryable(t *testing.T) {
	res := &fakeResolver{err: errors.New("pg down")}
	s, _ := newSyncer(t, &fakeVendor{}, res, &fakeStore{})
	if _, err := s.Sync(context.Background(), envFor(t, validPayload())); err == nil {
		t.Fatal("배선 조회 실패는 재시도(error)여야 한다")
	}
}

func TestBuildRowNormalizesStatusAndJSON(t *testing.T) {
	r, err := BuildRow(alimtalk.Template{
		Code: "T", Content: "#{a} #{a} #{b}",
		Buttons: []alimtalk.Button{{Type: "WL", Name: "확인", LinkMo: "https://onda.dev"}},
	})
	if err != nil {
		t.Fatalf("변환 실패: %v", err)
	}
	if r.Status != "unknown" {
		t.Fatalf("상태 미상은 unknown이어야 한다: %q", r.Status)
	}
	if len(r.Variables) != 2 {
		t.Fatalf("중복 치환자는 한 번만: %v", r.Variables)
	}
	var buttons []alimtalk.Button
	if err := json.Unmarshal([]byte(r.Buttons), &buttons); err != nil || len(buttons) != 1 {
		t.Fatalf("버튼 jsonb 직렬화가 다르다: %q (%v)", r.Buttons, err)
	}
	if r.QuickReplies != "[]" {
		t.Fatalf("빈 바로연결은 [] 이어야 한다: %q", r.QuickReplies)
	}
}

// upsert는 tenant_id를 조건에 달아 남의 행을 덮어쓰지 못하게 한다 (CLAUDE.md 규칙 5).
func TestSQLCarriesTenantFilter(t *testing.T) {
	for name, sql := range map[string]string{"upsert": upsertSQL, "markMissing": markMissingSQL} {
		if !strings.Contains(sql, "tenant_id") {
			t.Fatalf("%s SQL에 tenant_id 조건이 없다", name)
		}
	}
}
