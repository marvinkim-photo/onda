//go:build onda_mock

// 목(mock) 알림톡 커넥터를 워커 바이너리에 포함한다.
//
// 기본 빌드에는 들어가지 않는다. 운영 이미지에 목 발송기가 딸려 들어가 실수로 선택되는 사고를
// 막기 위해 빌드 태그로만 켠다. 켜는 방법:
//
//	go build -tags onda_mock ./apps/worker/cmd/worker
//	docker compose build --build-arg GO_TAGS=onda_mock worker
//
// 태그를 켜도 매니페스트를 배치해야 실제로 등록된다 — 두 단계 모두 의도적으로 필요하다
// (`ONDA_CONNECTOR_MANIFESTS` 디렉터리에 mock 매니페스트를 놓아야 한다).
package main

import _ "github.com/ondahq/onda/apps/worker/internal/channel/alimtalk/mock"
