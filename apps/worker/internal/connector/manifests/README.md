# 커넥터 매니페스트 (개발용 자리)

**이 디렉터리는 로드 경로가 아니다.** 여기에 `*.json`을 놓아도 워커는 읽지 않는다.

워커가 실제로 읽는 곳은 환경변수 `ONDA_CONNECTOR_MANIFESTS`이고, 기본값은 `/etc/onda/connectors`다
(`cmd/worker/main.go`의 `connectorManifestDir`). 셀프호스팅에서 그 경로에 마운트되는 것은
저장소의 `deploy/connectors/`이므로, 커넥터를 켜고 끄는 일은 전부 거기서 한다.
설치 방법과 목 커넥터 활성화 절차는 `deploy/connectors/README.md`에 있다.

이 디렉터리는 로컬에서 `ONDA_CONNECTOR_MANIFESTS`를 여기로 가리켜 커넥터 로딩을
시험해 보고 싶을 때 쓰는 빈 자리로만 남긴다. 커밋되는 매니페스트를 여기에 두지 않는다 —
로드 경로가 두 곳으로 보이면 어느 쪽이 켜져 있는지 아무도 확신하지 못한다.

기동 시 동작(경로가 어디든 동일):

- 디렉터리가 없으면 빈 목록으로 본다. 커넥터를 안 쓰는 배포가 기동에 실패하면 안 된다.
- `id`가 중복되면 기동이 실패한다.
- `runtime.type = in_process_go`인데 Go 구현이 등록돼 있지 않으면 기동이 실패한다.
  구현은 자기 패키지 `init()`에서 `alimtalk.Register`를 부르고, `cmd/worker`가 블랭크 임포트한다.
  목 벤더는 `GO_TAGS=onda_mock` 빌드에서만 임포트된다(`cmd/worker/connectors_mock.go`).

스키마 단일 출처는 `packages/queue-schemas/schemas/connector.manifest.v0.schema.json`,
Go 투영은 `apps/worker/internal/connector/manifest.go`다.
