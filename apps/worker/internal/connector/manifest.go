// Package connector — 커넥터 manifest(onda.connector.json) 로더.
// 스키마 단일 출처는 packages/queue-schemas/schemas/connector.manifest.v0.schema.json이며,
// 이 파일의 구조체는 그 스키마의 Go 투영이다. 계약 테스트가 둘의 enum 일치를 강제한다.
//
// manifest는 "벤더가 자신을 선언하는 파일"이다. 엔진은 이 선언만 보고
// 설치·크리덴셜 폼·capability 검증·정책 렌더·수명주기 수집 방식을 정한다.
package connector

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// IDPattern — connector_id 규칙. message.lifecycle.v1의 connector_id 패턴과 동일해야 한다.
var IDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)

var versionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// Substitution — 변수 치환을 누가 하는가. 벤더 변이의 첫 번째 축.
//
//	variables: 엔진이 키-값을 넘기고 공급자가 렌더한다 (NHN templateParameter, Solapi variables)
//	rendered:  엔진이 승인 본문에 치환을 마친 완성 텍스트를 넘긴다 (알리고 message_N)
//	both:      둘 다 받는다
type Substitution string

const (
	SubstitutionVariables Substitution = "variables"
	SubstitutionRendered  Substitution = "rendered"
	SubstitutionBoth      Substitution = "both"
)

// LifecycleMode — 발송 결과를 밀어주는가 당겨야 하는가. 벤더 변이의 세 번째 축.
type LifecycleMode string

const (
	LifecycleCallback LifecycleMode = "callback" // 공급자가 웹훅으로 밀어준다
	LifecyclePolling  LifecycleMode = "polling"  // 조회 API를 우리가 당겨야 한다
	LifecycleBoth     LifecycleMode = "both"
	LifecycleNone     LifecycleMode = "none" // 접수 응답이 최종 (SMTP 같은 동기 채널)
)

type Manifest struct {
	ManifestVersion int          `json:"manifest_version"`
	ID              string       `json:"id"`
	Name            string       `json:"name"`
	Description     string       `json:"description,omitempty"`
	Version         string       `json:"version"`
	Channel         string       `json:"channel"`
	Vendor          Vendor       `json:"vendor"`
	License         string       `json:"license"`
	Tier            string       `json:"tier,omitempty"`
	Runtime         Runtime      `json:"runtime"`
	TargetTypes     []string     `json:"target_types"`
	Capabilities    Capabilities `json:"capabilities"`
	Credentials     SchemaBlock  `json:"credentials"`
	Config          *SchemaBlock `json:"config,omitempty"`
	Lifecycle       Lifecycle    `json:"lifecycle"`
	Limits          *Limits      `json:"limits,omitempty"`
	Compliance      *Compliance  `json:"compliance,omitempty"`
	Cost            *Cost        `json:"cost,omitempty"`
	ContractTests   []string     `json:"contract_tests,omitempty"`
}

type Vendor struct {
	Name    string `json:"name"`
	URL     string `json:"url,omitempty"`
	Support string `json:"support,omitempty"`
}

type Runtime struct {
	Type      string `json:"type"` // in_process_go | remote_http
	GoPackage string `json:"go_package,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	Auth      string `json:"auth,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

type Capabilities struct {
	Content        []string `json:"content"`
	Silent         bool     `json:"silent,omitempty"`
	FallbackSource bool     `json:"fallback_source,omitempty"`
	ScheduledSend  bool     `json:"scheduled_send,omitempty"`
	BatchMax       int      `json:"batch_max,omitempty"`
	AsyncReceipt   bool     `json:"async_receipt,omitempty"`
	TestSend       bool     `json:"test_send,omitempty"`
	// 아래 셋은 v0 스키마의 확장 — 알림톡 벤더 변이 네 축 중 셋을 선언한다.
	Substitution   Substitution  `json:"substitution,omitempty"`
	VendorFallback bool          `json:"vendor_fallback,omitempty"`
	LifecycleMode  LifecycleMode `json:"lifecycle_mode,omitempty"`
}

type SchemaBlock struct {
	Schema   json.RawMessage `json:"schema"`
	Validate *bool           `json:"validate,omitempty"`
}

type Lifecycle struct {
	Reports  []string      `json:"reports"`
	Callback *CallbackSpec `json:"callback,omitempty"`
}

type CallbackSpec struct {
	Path   string `json:"path"`
	Verify string `json:"verify,omitempty"` // hmac_sha256 | ip_allowlist | token | none
}

type Limits struct {
	RPS   int          `json:"rps,omitempty"`
	Daily int          `json:"daily,omitempty"`
	Retry *RetryPolicy `json:"retry,omitempty"`
}

type RetryPolicy struct {
	MaxAttempts    int  `json:"max_attempts,omitempty"`
	BaseBackoffMS  int  `json:"base_backoff_ms,omitempty"`
	CircuitBreaker bool `json:"circuit_breaker,omitempty"`
}

type Compliance struct {
	Regions                  []string `json:"regions,omitempty"`
	MarketingAllowed         *bool    `json:"marketing_allowed,omitempty"`
	RequiresTemplateApproval bool     `json:"requires_template_approval,omitempty"`
	AdLabel                  string   `json:"ad_label,omitempty"`
	Unsubscribe              string   `json:"unsubscribe,omitempty"`
	QuietHoursDefault        *string  `json:"quiet_hours_default,omitempty"`
	PIIInTransit             []string `json:"pii_in_transit,omitempty"`
}

type Cost struct {
	Currency   string  `json:"currency,omitempty"`
	PerMessage float64 `json:"per_message,omitempty"`
}

// Reports — 이 커넥터가 실제로 보고할 수 있는 상태인지. 리포트가 "미지원"과 "0"을 구분하는 근거.
func (m Manifest) Reports(status string) bool {
	for _, s := range m.Lifecycle.Reports {
		if s == status {
			return true
		}
	}
	return false
}

// SubstitutionMode — 미선언 시 variables(NHN 기준)로 본다.
func (m Manifest) SubstitutionMode() Substitution {
	if m.Capabilities.Substitution == "" {
		return SubstitutionVariables
	}
	return m.Capabilities.Substitution
}

// Mode — 미선언 시 async_receipt 여부로 추론한다.
func (m Manifest) Mode() LifecycleMode {
	if m.Capabilities.LifecycleMode != "" {
		return m.Capabilities.LifecycleMode
	}
	if m.Capabilities.AsyncReceipt {
		return LifecycleCallback
	}
	return LifecycleNone
}

func (m Manifest) NeedsPolling() bool {
	mode := m.Mode()
	return mode == LifecyclePolling || mode == LifecycleBoth
}

func (m Manifest) NeedsCallback() bool {
	mode := m.Mode()
	return mode == LifecycleCallback || mode == LifecycleBoth
}

var (
	validRuntime   = map[string]bool{"in_process_go": true, "remote_http": true}
	validSubst     = map[Substitution]bool{"": true, SubstitutionVariables: true, SubstitutionRendered: true, SubstitutionBoth: true}
	validMode      = map[LifecycleMode]bool{"": true, LifecycleCallback: true, LifecyclePolling: true, LifecycleBoth: true, LifecycleNone: true}
	validReport    = map[string]bool{"accepted": true, "sent": true, "delivered": true, "opened": true, "clicked": true, "failed": true, "unsubscribed": true, "bounced": true}
	validVerify    = map[string]bool{"": true, "hmac_sha256": true, "ip_allowlist": true, "token": true, "none": true}
	validTargetTyp = map[string]bool{"device_token": true, "phone": true, "email": true, "kakao_user": true, "line_user": true, "http_url": true, "in_app_user": true}
)

// Validate — 구조 검증. JSON Schema 전량 검증은 TS 쪽 계약 테스트가 담당하고,
// 여기서는 엔진이 실제로 의존하는 불변식만 확인한다(기동 실패로 조기에 잡기 위함).
func (m Manifest) Validate() error {
	switch {
	case m.ManifestVersion != 0:
		return fmt.Errorf("manifest_version은 0이어야 한다 (got %d)", m.ManifestVersion)
	case !IDPattern.MatchString(m.ID):
		return fmt.Errorf("id 형식 오류: %q", m.ID)
	case m.Name == "":
		return fmt.Errorf("%s: name 누락", m.ID)
	case !versionPattern.MatchString(m.Version):
		return fmt.Errorf("%s: version은 SemVer여야 한다 (got %q)", m.ID, m.Version)
	case m.Channel == "":
		return fmt.Errorf("%s: channel 누락", m.ID)
	case m.License == "":
		return fmt.Errorf("%s: license 누락", m.ID)
	case !validRuntime[m.Runtime.Type]:
		return fmt.Errorf("%s: runtime.type 오류: %q", m.ID, m.Runtime.Type)
	case len(m.TargetTypes) == 0:
		return fmt.Errorf("%s: target_types 누락", m.ID)
	case len(m.Capabilities.Content) == 0:
		return fmt.Errorf("%s: capabilities.content 누락", m.ID)
	case len(m.Lifecycle.Reports) == 0:
		return fmt.Errorf("%s: lifecycle.reports 누락", m.ID)
	case len(m.Credentials.Schema) == 0:
		return fmt.Errorf("%s: credentials.schema 누락", m.ID)
	case !validSubst[m.Capabilities.Substitution]:
		return fmt.Errorf("%s: capabilities.substitution 오류: %q", m.ID, m.Capabilities.Substitution)
	case !validMode[m.Capabilities.LifecycleMode]:
		return fmt.Errorf("%s: capabilities.lifecycle_mode 오류: %q", m.ID, m.Capabilities.LifecycleMode)
	}
	for _, t := range m.TargetTypes {
		if !validTargetTyp[t] {
			return fmt.Errorf("%s: target_types 오류: %q", m.ID, t)
		}
	}
	for _, r := range m.Lifecycle.Reports {
		if !validReport[r] {
			return fmt.Errorf("%s: lifecycle.reports 오류: %q", m.ID, r)
		}
	}
	if m.Lifecycle.Callback != nil {
		if m.Lifecycle.Callback.Path == "" {
			return fmt.Errorf("%s: lifecycle.callback.path 누락", m.ID)
		}
		if !validVerify[m.Lifecycle.Callback.Verify] {
			return fmt.Errorf("%s: lifecycle.callback.verify 오류: %q", m.ID, m.Lifecycle.Callback.Verify)
		}
	}
	if m.Runtime.Type == "remote_http" && (m.Runtime.Endpoint == "" || m.Runtime.Auth == "") {
		return fmt.Errorf("%s: remote_http는 endpoint·auth가 필수", m.ID)
	}
	// 콜백을 받겠다고 선언했으면 수신 경로가 있어야 한다.
	if m.NeedsCallback() && m.Lifecycle.Callback == nil {
		return fmt.Errorf("%s: lifecycle_mode가 콜백인데 lifecycle.callback이 없다", m.ID)
	}
	return nil
}

// Parse — 바이트에서 manifest를 읽고 검증한다.
func Parse(raw []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("manifest 파싱: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// LoadDir — 디렉터리의 *.json manifest를 모두 읽는다. id 중복은 오류.
// 반환은 id 오름차순이라 기동 로그가 결정적이다.
func LoadDir(dir string) ([]Manifest, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	seen := map[string]string{}
	out := make([]Manifest, 0, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p) //nolint:gosec // 운영자가 배치한 매니페스트 디렉터리
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		m, err := Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if prev, dup := seen[m.ID]; dup {
			return nil, fmt.Errorf("커넥터 id 중복: %s (%s, %s)", m.ID, prev, p)
		}
		seen[m.ID] = p
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
