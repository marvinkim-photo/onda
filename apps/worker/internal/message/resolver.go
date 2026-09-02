package message

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/clock"
)

// credCacheTTL — 워커 메모리 복호 캐시 (channel/worker.go와 동일한 10분).
// 크리덴셜을 매 건 복호화하면 KMS·CPU가 발송량에 비례해 든다.
const credCacheTTL = 10 * time.Minute

// binding — 앱의 채널→커넥터 배선 한 건(복호화된 크리덴셜 포함).
type binding struct {
	ConnectorID string
	Config      []byte // channel_connectors.config — 비밀 아닌 앱 단위 설정
	Credential  []byte // 복호화된 credentials.ciphertext
}

type cachedBinding struct {
	b        binding
	found    bool
	note     string // 미해석 사유(로그·lifecycle detail용)
	loadedAt time.Time
}

// resolver — (tenant, app, channel) → 커넥터 배선 + 크리덴셜. 발송 워커와 결과 폴러가 공유한다.
//
// 조회는 두 테이블이다:
//   - channel_connectors: 이 앱이 이 채널을 어느 커넥터로 보내는가 + 벤더 설정
//   - credentials:        그 커넥터의 비밀 (kind는 채널 단위 — 벤더별로 enum을 늘리지 않는다)
type resolver struct {
	pg        *pgxpool.Pool
	masterKey []byte
	clk       clock.Clock

	mu    sync.Mutex
	cache map[string]cachedBinding // key: tenantID + "/" + appID + "/" + channel
}

func newResolver(pg *pgxpool.Pool, masterKey []byte, clk clock.Clock) *resolver {
	return &resolver{pg: pg, masterKey: masterKey, clk: clk, cache: map[string]cachedBinding{}}
}

// credentialKind — 채널 → credentials.kind. 벤더가 아니라 채널 단위로 잡는다.
// 벤더 식별은 channel_connectors.connector_id가 하므로 제3자 벤더가 enum 마이그레이션 없이 들어온다.
func credentialKind(ch string) string {
	switch ch {
	case "kakao_alimtalk":
		return "alimtalk"
	default:
		return ch
	}
}

// resolve — 배선과 크리덴셜을 한 번에. found=false면 SendLoop이 credential_missing으로 종결한다.
// note는 왜 못 찾았는지(또는 무엇이 어긋났는지)를 담아 로그·lifecycle detail로 흘린다.
func (r *resolver) resolve(ctx context.Context, tenantID, appID, ch string) (binding, bool, string, error) {
	key := tenantID + "/" + appID + "/" + ch
	now := r.clk.Now()

	r.mu.Lock()
	if c, ok := r.cache[key]; ok && now.Sub(c.loadedAt) < credCacheTTL {
		r.mu.Unlock()
		return c.b, c.found, c.note, nil
	}
	r.mu.Unlock()

	b, found, note, err := r.load(ctx, tenantID, appID, ch)
	if err != nil {
		return binding{}, false, "", err
	}
	r.mu.Lock()
	r.cache[key] = cachedBinding{b: b, found: found, note: note, loadedAt: now}
	r.mu.Unlock()
	return b, found, note, nil
}

func (r *resolver) load(ctx context.Context, tenantID, appID, ch string) (binding, bool, string, error) {
	if r.pg == nil {
		return binding{}, false, "pg 미주입", nil
	}
	var b binding
	err := r.pg.QueryRow(ctx, `
		SELECT connector_id, COALESCE(config, '{}'::jsonb)::text
		  FROM channel_connectors
		 WHERE tenant_id = $1 AND app_id = $2 AND channel = $3 AND enabled`,
		tenantID, appID, ch).Scan(&b.ConnectorID, &b.Config)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return binding{}, false, fmt.Sprintf("채널 %s 커넥터 배선 없음(channel_connectors)", ch), nil
		}
		// 테이블 자체가 아직 없는 배포(마이그레이션 미적용)도 여기로 온다.
		// 크래시 대신 오류로 올려 SendLoop이 retryable로 재시도하게 한다.
		return binding{}, false, "", fmt.Errorf("channel_connectors 조회: %w", err)
	}

	kind := credentialKind(ch)
	var ciphertext, dekWrapped []byte
	err = r.pg.QueryRow(ctx, `
		SELECT ciphertext, dek_wrapped FROM credentials
		 WHERE app_id = $1 AND kind = $2 AND status = 'verified'`, appID, kind).Scan(&ciphertext, &dekWrapped)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return binding{}, false, fmt.Sprintf("%s 크리덴셜 미등록/미검증", kind), nil
		}
		return binding{}, false, "", fmt.Errorf("credentials 조회: %w", err)
	}
	plain, err := channel.DecryptEnvelope(r.masterKey, ciphertext, dekWrapped)
	if err != nil {
		// 마스터키 불일치는 재시도해도 같다. 그러나 조용히 전량 실패하느니 오류로 드러낸다.
		return binding{}, false, "", fmt.Errorf("크리덴셜 복호화: %w", err)
	}
	b.Credential = plain
	return b, true, "", nil
}

func (r *resolver) invalidate(tenantID, appID, ch string) {
	r.mu.Lock()
	delete(r.cache, tenantID+"/"+appID+"/"+ch)
	r.mu.Unlock()
}

// seed — 테스트가 pg 없이 배선을 심을 때 쓴다.
func (r *resolver) seed(tenantID, appID, ch string, b binding, found bool, note string) {
	r.mu.Lock()
	r.cache[tenantID+"/"+appID+"/"+ch] = cachedBinding{b: b, found: found, note: note, loadedAt: r.clk.Now()}
	r.mu.Unlock()
}

// --- 다른 패키지용 공개 표면 -------------------------------------------------
//
// 배선 해석(channel_connectors + credentials + 복호화)은 발송 워커·결과 폴러·템플릿 동기화가
// 모두 필요로 한다. 복사하면 크리덴셜 캐시 TTL·미해석 사유 문구가 셋으로 갈라지므로,
// 내부 resolver를 그대로 감싼 얇은 공개 타입만 덧붙인다(기존 호출부는 손대지 않는다).

// Binding — 해석된 커넥터 배선 한 건(복호화된 크리덴셜 포함).
type Binding struct {
	ConnectorID string
	// Config — channel_connectors.config (비밀 아닌 앱 단위 설정).
	Config []byte
	// Credential — 복호화된 credentials.ciphertext. 로그에 남기면 안 된다.
	Credential []byte
}

// Resolver — (tenant, app, channel) → Binding. 내부 캐시(10분)를 공유한다.
type Resolver struct{ r *resolver }

// NewResolver — pg가 nil이면 항상 미해석("pg 미주입")을 돌려준다(테스트용).
func NewResolver(pg *pgxpool.Pool, masterKey []byte, clk clock.Clock) *Resolver {
	return &Resolver{r: newResolver(pg, masterKey, clk)}
}

// Resolve — found=false는 "설정이 안 됐다"이지 "장애"가 아니다. note가 그 사유다.
func (r *Resolver) Resolve(ctx context.Context, tenantID, appID, ch string) (Binding, bool, string, error) {
	b, found, note, err := r.r.resolve(ctx, tenantID, appID, ch)
	if err != nil || !found {
		return Binding{}, false, note, err
	}
	return Binding{ConnectorID: b.ConnectorID, Config: b.Config, Credential: b.Credential}, true, note, nil
}

// CredentialKind — 채널 → credentials.kind (kakao_alimtalk → alimtalk).
func CredentialKind(ch string) string { return credentialKind(ch) }
