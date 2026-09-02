# 커넥터 매니페스트 (셀프호스팅)

이 디렉터리는 워커 컨테이너의 `/etc/onda/connectors`로 마운트된다.
여기에 `*.json` 매니페스트를 놓는 것이 곧 **그 커넥터를 켠다**는 뜻이다.

- `.disabled` 확장자는 로드되지 않는다(`*.json`만 읽는다). 켜려면 확장자를 `.json`으로 바꾼다.
- `runtime.type = in_process_go` 커넥터는 워커 바이너리에 구현이 포함돼 있어야 한다.
  목(mock) 커넥터는 `GO_TAGS=onda_mock`으로 빌드해야만 들어간다 — 운영 이미지에는 넣지 않는다.

E2E에서 목 커넥터를 켜는 법:

```bash
mv alimtalk_mock.json.disabled alimtalk_mock.json
GO_TAGS=onda_mock docker compose -f deploy/compose.yaml --profile full --profile app up -d --build worker
```
