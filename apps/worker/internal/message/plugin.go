package message

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
)

// plugin.go — channel.Verifier에 알림톡을 꽂기 위한 얇은 어댑터.
//
// Verifier는 크리덴셜 kind로 ChannelPlugin을 찾아 ValidateCredentials만 부른다.
// 알림톡 발송은 이 패키지의 Worker가 하므로 Send는 지원하지 않는다 — 어댑터의 존재 이유는
// "등록한 크리덴셜이 실제로 검증되게" 하는 것뿐이다. 검증 없이 verified가 안 되면
// resolver가 영영 크리덴셜을 못 찾아 알림톡이 조용히 전량 실패한다.
type CredentialPlugin struct {
	reg *alimtalk.Registry
}

func NewCredentialPlugin(reg *alimtalk.Registry) *CredentialPlugin {
	return &CredentialPlugin{reg: reg}
}

var _ channel.ChannelPlugin = (*CredentialPlugin)(nil)

func (p *CredentialPlugin) Kind() channel.ChannelKind {
	return channel.ChannelKind(alimtalk.CredentialKind)
}
func (p *CredentialPlugin) TargetType() channel.TargetType { return channel.TargetPhone }

// ValidateCredentials — 어느 벤더로 검증할지는 크리덴셜 본문의 connector_id가 정한다.
// credentials.kind는 채널 단위("alimtalk")라 벤더를 담지 못하기 때문이다(벤더별 enum 증식 회피).
// 등록된 벤더가 하나뿐이면 그것으로 본다 — 단일 벤더 셀프호스팅의 흔한 경우다.
func (p *CredentialPlugin) ValidateCredentials(ctx context.Context, creds channel.Credentials) error {
	var probe struct {
		ConnectorID string `json:"connector_id"`
	}
	_ = json.Unmarshal(creds.JSON, &probe)
	id := strings.TrimSpace(probe.ConnectorID)
	if id == "" {
		ids := p.reg.IDs()
		if len(ids) != 1 {
			// 판정 유보가 아니라 error로 보낸다: 사람이 고치기 전엔 영원히 같은 결과다.
			return channel.NewSendError(channel.FailureCredentialAuth,
				"크리덴셜에 connector_id가 없고 등록된 알림톡 벤더가 %d개 — 어느 벤더로 검증할지 알 수 없다", len(ids))
		}
		id = ids[0]
	}
	v, err := p.reg.Get(id)
	if err != nil {
		return channel.NewSendError(channel.FailureCredentialAuth, "%s", err.Error())
	}
	return v.Validate(ctx, alimtalk.Credential{ConnectorID: id, JSON: creds.JSON})
}

// Send — 이 어댑터는 발송 경로가 아니다. send.message 워커가 담당한다.
func (p *CredentialPlugin) Send(context.Context, channel.SendRequest) (channel.SendResult, error) {
	return channel.SendResult{}, channel.NewSendError(channel.FailurePermanentContent,
		"알림톡 발송은 send.message 워커가 담당한다 — ChannelPlugin.Send 경로 미지원")
}

func (p *CredentialPlugin) ClassifyError(err error) channel.FailureClass {
	return channel.Classify(err)
}

func (p *CredentialPlugin) HandleCallback(context.Context, []byte) ([]channel.DeliveryUpdate, error) {
	// 벤더 웹훅은 원문 그대로 alimtalk.Vendor.ParseCallback이 해석한다(API 경계).
	return nil, channel.NewSendError(channel.FailurePermanentContent,
		"알림톡 콜백은 alimtalk.Vendor.ParseCallback이 처리한다")
}
