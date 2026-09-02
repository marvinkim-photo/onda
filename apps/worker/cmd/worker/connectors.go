package main

// 코어 커넥터 등록.
//
// in-process 커넥터는 자기 패키지 init()에서 alimtalk.Register를 부르므로, 여기서
// 블랭크 임포트해야 레지스트리가 구현을 찾는다. 임포트하지 않으면 매니페스트만 있고
// 구현이 없는 상태가 되어 워커가 기동에서 실패한다(조용히 넘어가지 않는다).
//
// 목(mock) 커넥터는 여기 없다 — 운영 이미지에 딸려 들어가면 안 되므로
// 빌드 태그가 걸린 connectors_mock.go에만 있다.
import _ "github.com/ondahq/onda/apps/worker/internal/channel/alimtalk/nhn"
