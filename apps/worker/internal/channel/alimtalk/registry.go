package alimtalk

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/ondahq/onda/apps/worker/internal/connector"
)

// ErrUnsupported — 벤더가 지원하지 않는 경로. 폴링 전용 벤더의 ParseCallback,
// 콜백 전용 벤더의 PollResults가 반환한다.
var ErrUnsupported = errors.New("alimtalk: 이 벤더가 지원하지 않는 기능")

// ErrNotFound — 레지스트리에 없는 커넥터.
var ErrNotFound = errors.New("alimtalk: 등록되지 않은 커넥터")

// Factory — manifest를 받아 벤더 인스턴스를 만든다.
// in-process 벤더는 init()에서 Register로 팩토리를 등록하고,
// remote_http 벤더는 레지스트리가 HTTP 어댑터 팩토리를 자동으로 붙인다(P3).
type Factory func(m connector.Manifest) (Vendor, error)

var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}
)

// Register — in-process 벤더 팩토리 등록. 패키지 init()에서 호출한다.
// 같은 id를 두 번 등록하면 배선 실수이므로 panic한다.
func Register(id string, f Factory) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	if _, dup := factories[id]; dup {
		panic("alimtalk: 커넥터 중복 등록: " + id)
	}
	factories[id] = f
}

// Registry — connector_id → Vendor 해석기.
type Registry struct {
	vendors map[string]Vendor
}

// NewRegistry — manifest 목록으로 레지스트리를 만든다.
// 알림톡 채널이 아닌 manifest는 조용히 건너뛴다(한 디렉터리에 여러 채널이 섞일 수 있다).
func NewRegistry(manifests []connector.Manifest) (*Registry, error) {
	r := &Registry{vendors: map[string]Vendor{}}
	factoriesMu.RLock()
	defer factoriesMu.RUnlock()
	for _, m := range manifests {
		if m.Channel != ChannelID {
			continue
		}
		switch m.Runtime.Type {
		case "in_process_go":
			f, ok := factories[m.ID]
			if !ok {
				return nil, fmt.Errorf("alimtalk: %s manifest는 있으나 in-process 구현이 등록되지 않았다", m.ID)
			}
			v, err := f(m)
			if err != nil {
				return nil, fmt.Errorf("alimtalk: %s 생성: %w", m.ID, err)
			}
			r.vendors[m.ID] = v
		case "remote_http":
			// P3에서 httpVendor 어댑터를 붙인다. 그전까지는 배선 실수를 조기에 드러낸다.
			return nil, fmt.Errorf("alimtalk: %s는 remote_http 커넥터 — 아직 지원하지 않는다", m.ID)
		default:
			return nil, fmt.Errorf("alimtalk: %s runtime.type 오류: %q", m.ID, m.Runtime.Type)
		}
	}
	return r, nil
}

// Get — 커넥터 해석.
func (r *Registry) Get(connectorID string) (Vendor, error) {
	v, ok := r.vendors[connectorID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, connectorID)
	}
	return v, nil
}

// IDs — 등록된 커넥터 id (정렬됨). 콘솔의 벤더 선택 목록과 기동 로그에 쓴다.
func (r *Registry) IDs() []string {
	out := make([]string, 0, len(r.vendors))
	for id := range r.vendors {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Manifests — 등록된 커넥터의 manifest (id 정렬). 콘솔이 벤더 선택·설정 폼을 그리는 데 쓴다.
func (r *Registry) Manifests() []connector.Manifest {
	out := make([]connector.Manifest, 0, len(r.vendors))
	for _, v := range r.vendors {
		out = append(out, v.Manifest())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
