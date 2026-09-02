# NHN Cloud 알림톡 벤더 — 가정과 미검증 지점

**실계정 확보 시 아래 번호 순서대로 확인한다. 위쪽일수록 틀렸을 때 피해가 크다.**

> **이 구현은 2026-09-02 기준 NHN Cloud KakaoTalk Bizmessage v2.3 공식 문서만 보고 작성됐다.
> 실제 계정으로 호출해 검증한 적이 단 한 번도 없다.**
> 모든 테스트는 우리가 "문서를 이렇게 읽었다"를 그대로 구현한 httptest 가짜 서버(`fakeserver_test.go`)를 문다.
> 즉 테스트가 초록이라는 사실은 **우리 해석 안에서 일관되다**는 뜻이지, NHN이 실제로 그렇게 동작한다는 증거가 아니다.
> 이 문서를 읽는 사람이 이 구현을 검증된 것으로 오해하면 안 된다.

각 항목은 이렇게 읽는다.

- **문서가 말하지 않는 것** — 공식 문서에서 확인할 수 없었던 것
- **우리 가정** — 그래서 무엇을 골랐는가
- **틀렸을 때 증상** — 운영에서 어떤 모습으로 드러나는가 (이상한 증상을 보고 이 문서를 역으로 찾아오기 위한 항목)
- **의존하는 코드·테스트**
- **실계정 확인 방법**

---

## 1. `header.resultCode` 숫자 표가 없다 — 그래서 낱말로 판정한다

- **문서가 말하지 않는 것**: v2.3은 `header.resultCode`를 int로 정의만 하고 값 목록을 주지 않는다. 어떤 코드가 인증 실패이고 어떤 코드가 본문 반려인지 알 수 없다.
- **우리 가정**: 확실한 코드 하나(`-9999` = NHN Cloud 공통 시스템 오류)만 표에 넣고, 나머지는 `resultMessage`의 **낱말**로 분류한다. 판정 순서는 credential_auth → rate_limited → retryable → invalid_target → permanent_content. 인증을 가장 먼저 보는 이유는 인증 실패가 permanent_content로 새면 `channel.Verifier.judge`가 그걸 "인증은 통과"로 읽어 **틀린 키가 verified로 저장**되기 때문이다.
- **틀렸을 때 증상**:
  - 오분류가 조용히 일어난다. 사고로 드러나는 대표적 모습 세 가지다.
  - (a) 크리덴셜이 만료·회수됐는데 커넥터가 계속 `verified`로 보이고, 발송만 전량 실패한다. 크리덴셜 정지가 걸리지 않아 운영자는 발송이 멈춘 뒤에야 안다.
  - (b) 영구 실패(본문 반려)가 retryable로 분류돼 같은 메시지를 재시도 한도까지 계속 쏜다. 큐가 밀리고 과금이 샌다.
  - (c) 일시 장애가 permanent_content로 분류돼 재시도 없이 버려진다. 발송 누락이 조용히 생긴다.
  - NHN이 응답 문구를 바꾸기만 해도 (한글 → 영문, 문구 개정) 같은 증상이 재발한다. 낱말 판정의 근본 약점이다.
- **의존하는 코드·테스트**: `errors.go`의 `headerCodeClass`·`classifyMessage`·`classifyRecipient`·`authWords`/`rateWords`/`retryWords`/`targetWords`/`contentWords`. `nhn.go`의 `classifyHTTP`·`checkHeader`. 테스트는 `nhn_test.go`의 `TestClassifyMessage`, `TestSendErrorClasses`.
- **실계정 확인 방법**: 의도적으로 틀린 `SecretKey`, 없는 `templateCode`, 권한 없는 `senderKey`, 미승인 템플릿, 잘못된 수신번호로 각각 발송해 `header.resultCode`와 `resultMessage`를 수집한다. NHN Cloud 콘솔의 API 오류 코드 페이지도 함께 뜬다.
- **표를 확보한 뒤 해야 할 일**: **낱말 판정을 1차 근거에서 내린다.** `headerCodeClass`를 실제 코드 표로 채우고, `classifyMessage`의 낱말 판정은 표에 없는 코드에만 걸리는 2차 안전망으로 남긴다. 지금은 순서가 반대다(표에 하나뿐이라 사실상 낱말이 1차다).

## 2. 멱등 헤더를 재사용하면 같은 `requestId`가 오는가 — 계약 테스트가 통째로 이 위에 서 있다

- **문서가 말하지 않는 것**: `X-NC-API-IDEMPOTENCY-KEY`는 "10분 내 중복 발송을 방지한다"고만 적혀 있다. 두 번째 요청에 **무엇을 응답하는지** 명시가 없다. 앞선 요청과 같은 `requestId`+`sendResults`를 그대로 돌려주는지, 빈 응답이나 별도 오류 코드를 주는지 알 수 없다.
- **우리 가정**: 같은 키의 재요청은 **앞선 요청과 동일한 `requestId`와 `recipientSeq`를 돌려준다**. 그래서 벤더는 재발송에 아무 특별 처리를 하지 않고 응답을 그대로 읽는다.
- **틀렸을 때 증상**:
  - 워커 재기동·큐 중복 소비로 같은 `message_id`가 두 번 흐르면, 두 접수의 `ProviderMessageID`가 갈린다. 1차 접수는 폴러의 대기 목록에 남지만 NHN 쪽에는 대응하는 발송이 없어 **영원히 종결되지 않는다.** 미종결 접수가 조용히 쌓이고, 도달률 분모가 계속 커져 통계가 서서히 낮아진다.
  - NHN이 재요청에 오류를 준다면 정상 재시도가 실패로 잡힌다. `-` 계열 코드가 무엇이냐에 따라 1번의 오분류와 겹쳐 retryable 무한 재시도로 갈 수도 있다.
- **의존하는 코드·테스트**:
  - `send.go`의 `Send` — `map[string]string{IdempotencyHeader: req.MessageID}`.
  - **계약 테스트 `idempotent_resend`가 이 가정 하나에 통째로 서 있다.** `conformance/cases.go`의 `caseIdempotentResend`는 같은 `MessageID`로 두 번 보내 `first.ProviderMessageID == second.ProviderMessageID`를 요구한다. 우리 벤더는 이 동등성을 **스스로 만들어내지 않는다.** 공급자가 같은 `requestId`를 돌려준다는 가정에만 의존한다. 가정이 틀리면 이 계약은 실계정에서 즉시 깨진다.
  - 가짜 서버 `fakeserver_test.go`의 `fakeNHN.idem` 맵이 바로 이 가정을 구현한 것이다. 즉 **테스트가 통과하는 이유는 우리가 그렇게 만들었기 때문**이지 NHN이 그렇게 하기 때문이 아니다.
  - 단위 테스트는 `nhn_test.go`의 `TestIdempotentResend`.
- **실계정 확인 방법**: 같은 `X-NC-API-IDEMPOTENCY-KEY`로 발송을 두 번 POST하고 두 응답의 `message.requestId`와 `sendResults[0].recipientSeq`를 비교한다. 10분이 지난 뒤 세 번째로 같은 키를 보내 새 `requestId`가 나오는지(멱등 창이 실제로 10분인지)도 함께 본다.
- **틀렸을 때의 대안**: 벤더가 `message_id → ProviderMessageID`를 자체 보관하거나, 워커 계층의 발송 멱등 선점(CLAUDE.md 규칙 6)에 더 강하게 기대야 한다. 그 경우 이 벤더는 무상태 싱글턴을 유지할 수 없다.

## 3. 대체발송 결과를 조회 응답에서 어떻게 알려주는지 모른다

- **문서가 말하지 않는 것**: `resendParameter`로 대체발송을 요청하는 방법은 문서에 있지만, **단건 조회 응답에서 대체발송이 실제로 나갔는지 알려주는 필드**가 무엇인지 명시가 없다. 대체발송이 성공했을 때 `messageStatus`가 `COMPLETED`인지 `FAILED`(알림톡 기준)인지도 불명확하다.
- **우리 가정**: 조회 응답에 `resendStatus`(`COMPLETED`/`SUCCESS`)와 `resendType`(`SMS`/`LMS`)이 온다고 보고, **`resendStatus`가 성공이면 `messageStatus`와 무관하게** 대체발송 도달로 판정한다(`Event{Status:"sent", DeliveredVia:"sms"|"lms"}`).
- **틀렸을 때 증상**:
  - 필드명이 다르면 대체발송이 전혀 감지되지 않는다. 문자로 잘 도달한 건이 **알림톡 실패로만 집계**된다. 채널별 도달률에서 알림톡이 실제보다 나쁘게 보이고, SMS 전환 비용이 장부에 아예 안 잡혀 **SMS 폴백이 공짜처럼 보인다.** 이게 `Event.DeliveredVia`가 존재하는 이유 자체다.
  - 반대로 `resendStatus`가 "대체발송을 시도했다"만 뜻하고 도달을 보장하지 않는다면, 실패한 문자를 `sent`로 보고해 **도달률이 부풀려진다.**
  - 대체발송 성공 시 `messageStatus`가 `COMPLETED`로 온다면, `resendStatus`를 못 읽는 순간 이 건이 **알림톡 delivered로 잡힌다.** 위 두 경우보다 더 나쁘다 — 틀린 채널에 도달을 귀속시킨다.
- **의존하는 코드·테스트**: `poll.go`의 `messageResult.ResendStatus`/`ResendType`, `deliveredVia`, `eventOf`(resend를 `messageStatus`보다 먼저 본다), `costFor`. 테스트는 `poll_test.go`의 `TestPollFallbackDeliveredVia`, `TestPollFallbackAbsentFails`, 계약 테스트 `fallback_trigger`.
- **실계정 확인 방법**: 카카오톡 미가입 번호(또는 채널 차단 번호)로 `resendParameter`를 실어 발송한 뒤 단건 조회 응답 **전문을 그대로 덤프**한다. 대체발송 없이 같은 번호로 한 번 더 보내 두 응답을 diff하면 어떤 필드가 대체발송 때문에 생기는지 바로 보인다.

## 4. 단건 조회 응답에서 결과를 감싸는 키가 무엇인지 확실하지 않다

- **문서가 말하지 않는 것**: 단건 조회 예시가 결과 본문을 `message` 아래에 두는데, NHN Cloud의 다른 메시지 상품은 `messageSearchResultResponse` 같은 다른 이름을 쓰는 사례가 있다. v2.3 알림톡의 정본이 무엇인지 단정할 수 없다.
- **우리 가정**: `message` 아래를 먼저 보고, 없으면 최상위에서 `messageStatus`를 찾는다.
- **틀렸을 때 증상**: 조회는 HTTP 200에 `isSuccessful=true`로 오는데 파싱이 실패해 `"NHN 조회 응답에 messageStatus가 없습니다"` 오류가 난다. 이 오류는 retryable이라 **폴러가 같은 건을 영원히 다시 묻는다.** 모든 발송이 미종결로 남고 조회 API 호출량만 계속 늘어난다. 증상이 특정 건에 국한되지 않고 **전량**이라는 점이 단서다.
- **의존하는 코드·테스트**: `poll.go`의 `queryResponse`·`pollOne`의 `q.Message == nil` 대체 경로. 테스트는 `poll_test.go`의 `TestPollDelivered` 계열(가짜 서버가 `message`로 응답한다).
- **실계정 확인 방법**: 아무 발송이나 한 건 하고 단건 조회 응답 원문을 그대로 로그로 남긴다. 최상위 키 이름만 보면 끝난다.

## 5. 템플릿 목록 응답에서 목록을 감싸는 키가 무엇인지 확실하지 않다

- **문서가 말하지 않는 것**: 문서 예시가 `templateListResponse` 아래에 두는 것과 `message` 아래에 두는 것이 섞여 있다.
- **우리 가정**: `templateListResponse` → `message` → 최상위(`templates`) 순으로 셋 다 받는다. 셋 중 어느 것이 와도 읽힌다.
- **틀렸을 때 증상**: 셋 다 아니면 **템플릿이 0건으로 조용히 동기화된다.** 오류가 아니라 빈 목록이라 아무도 알아채지 못하고, `alimtalk_templates` 테이블이 비어 콘솔의 템플릿 선택 목록이 텅 빈다. 더 나쁜 경우는 이미 동기화된 템플릿이 "벤더에서 사라짐"으로 판정돼 **일괄 삭제**되는 것이다(동기화 로직이 어떻게 diff하느냐에 달렸다 — `templatesync` 패키지 담당자와 함께 확인할 것).
  또한 `Validate`가 이 호출을 쓰므로, 응답이 200이기만 하면 크리덴셜 검증은 **빈 목록으로도 통과한다.**
- **의존하는 코드·테스트**: `templates.go`의 `templateListEnvelope`·`block()`. 테스트는 `templates_test.go`의 `TestListTemplatesWrapperVariants`.
- **실계정 확인 방법**: 템플릿 목록을 한 번 호출해 응답 원문의 최상위 키를 본다. 승인 템플릿이 1건 이상 있는 발신프로필로 해야 빈 목록과 구분된다.

## 6. 보존 기간이 지난 건의 조회가 404인지 200+실패인지 모른다

- **문서가 말하지 않는 것**: NHN이 발송 이력을 90일 보관한다는 것은 알지만, 그보다 오래된 건을 조회하면 HTTP 404인지 200 + `isSuccessful=false`인지 명시가 없다.
- **우리 가정**: 둘 다 받는다. HTTP 404이거나, 200 + 실패 메시지에 만료 낱말(`조회 기간`, `기간 만료`, `expired` 등)이 있으면 `Event{Expired:true, Terminal:false}`로 낸다. `Expired`는 "결과가 확정됐다"(Terminal)가 아니라 **"결과를 끝내 알아내지 못했다"**이며, 폴러는 대기 목록에서 지우되 도달 집계에는 넣지 않아야 한다.
- **틀렸을 때 증상**:
  - 만료가 우리가 못 읽는 형태(예: 200 + `isSuccessful=true` + 빈 `messageStatus`)로 오면, 4번과 같은 증상이 된다 — 폴러가 **영원히 다시 묻는다.**
  - 반대로 만료가 아닌 일반 404(잘못된 `requestId` 등)를 만료로 오독하면, 실제로는 조회 가능한 건을 "알 수 없음"으로 버린다. 도달률 분모에서 빠져 **통계가 실제보다 좋아 보인다.**
  - 참고: 발송 경로(`Send`)의 404는 만료가 아니라 `invalid_target`으로 분류한다(`classifyHTTP`). 같은 상태 코드를 경로에 따라 다르게 읽고 있다는 점을 기억할 것.
- **의존하는 코드·테스트**: `poll.go`의 `pollOne`(404 분기)·`expiredEvent`, `errors.go`의 `expiredWords`·`isExpired`. 테스트는 `poll_test.go`의 `TestPollExpired`, `TestPollExpiredByMessage`.
- **실계정 확인 방법**: 90일이 지나야 자연 재현되므로, 대신 **존재하지 않는 `requestId`**로 조회해 응답 형태를 본다. NHN 지원에 보존 기간 만료 시 응답 형태를 직접 문의하는 편이 빠르다.

## 7. 진행 중 `messageStatus` 값이 무엇인지 모른다

- **문서가 말하지 않는 것**: 문서에서 확인한 값은 `COMPLETED`·`FAILED`·`CANCEL` 셋뿐이다. 접수 직후 조회하면 무엇이 오는지(`READY`? `SENDING`? `WAIT`?) 목록이 없다.
- **우리 가정**: 이 셋이 아니면 **아직 결과가 아니다**로 보고 이벤트를 **하나도 내지 않는다**(`eventOf`가 `nil`을 돌려준다). 폴러가 대기 목록에 남겨 다음 주기에 다시 묻는다.
- **틀렸을 때 증상**:
  - 종결 상태 중 우리가 모르는 값이 있으면(예: `SUCCESS`, `DONE`), 그 건은 **영원히 미종결로 남는다.** 폴링이 무한 반복되고 조회 API 호출량이 계속 는다. 4번·6번과 증상이 같아 원인 구분이 어렵다 — 로그에서 `messageStatus` 실제 값을 찍어봐야 갈린다.
  - 반대로 진행 중 상태를 종결로 오독하면 아직 도달하지 않은 건을 `delivered`로 보고한다.
- **의존하는 코드·테스트**: `poll.go`의 `eventOf` `default:` 분기, `nhn.go`의 `StatusCompleted`/`StatusFailed`/`StatusCancel`. 테스트는 `poll_test.go`의 `TestEventOfMapping`(READY·SENDING이 `nil`임을 확인).
- **실계정 확인 방법**: 발송 직후 1초 간격으로 단건 조회를 반복해 `messageStatus`가 거치는 값을 전부 기록한다. 예약 발송(`requestDate`)을 걸면 발송 전 상태도 관찰할 수 있다.

## 8. `sendResults[].resultCode`가 숫자인지 문자열인지 확실하지 않다

- **문서가 말하지 않는 것**: 문서 예시가 `0`과 `"0"`을 섞어 쓴다. `header.resultCode`도 마찬가지다.
- **우리 가정**: `flexInt` 타입으로 숫자·문자열 양쪽을 받는다. 숫자로 파싱되지 않는 문자열(`MRC01` 같은 것)은 `0`으로 두고 별도 문자열 필드가 받는다.
- **틀렸을 때 증상**: 가정이 관대해서 틀릴 여지가 거의 없다. 다만 **숫자가 아닌 코드가 `0`(성공)으로 접힌다**는 점이 위험하다. NHN이 `sendResults[].resultCode`에 `"E001"` 같은 문자열 오류 코드를 쓴다면, 실패한 수신자가 **성공 접수로 통과**한다. 발송되지 않은 건이 접수로 잡히고 폴링에서만 뒤늦게 실패로 드러난다(그나마 폴링이 잡아준다면).
- **의존하는 코드·테스트**: `nhn.go`의 `flexInt.UnmarshalJSON`, `send.go`의 `sendResult.ResultCode` 비교(`int(r.ResultCode) != 0`).
- **실계정 확인 방법**: 잘못된 수신번호를 섞어 발송하고 `sendResults[].resultCode`의 JSON 타입과 값 형태를 본다. 문자열 코드 체계라면 `flexInt` 대신 문자열 비교로 바꿔야 한다.

## 9. 날짜 형식과 시간대가 확실하지 않다

- **문서가 말하지 않는 것**: `receiveDate`·`requestDate`·`createDate`의 정확한 포맷과 시간대가 명시되지 않았다. 초·밀리초 유무도 예시마다 다르다.
- **우리 가정**: KST(고정 +09:00) 벽시계이고, `2006-01-02 15:04:05.0` → `2006-01-02 15:04:05` → `2006-01-02 15:04` → RFC3339 순으로 시도한다. 하나도 안 맞으면 **주입된 시계의 현재 시각**을 쓴다(제로 시각은 절대 내보내지 않는다 — 수명주기 순서를 정할 수 없게 된다). `tzdata` 유무에 영향받지 않도록 `time.LoadLocation` 대신 고정 오프셋(`kst`)을 쓴다.
- **틀렸을 때 증상**:
  - 시간대가 UTC라면 모든 이벤트 시각이 **9시간 앞선다.** 수명주기 이벤트가 발송 시각보다 먼저 일어난 것처럼 보이고, 시간 단위 리포트의 경계가 어긋난다. "저녁 발송이 다음 날로 잡힌다" 같은 모양으로 드러난다.
  - 포맷이 하나도 안 맞으면 **모든 이벤트의 `OccurredAt`이 폴링 시각**이 된다. 조용히 그럴듯해 보이지만, 실제 도달 시각 정보가 통째로 사라지고 도달 지연 분석이 불가능해진다. 이벤트 시각이 폴링 주기와 정확히 일치하는 패턴이 단서다.
- **의존하는 코드·테스트**: `poll.go`의 `nhnTimeLayouts`·`parseNHNTime`·`occurredAt`, `send.go`의 `kst`. 테스트는 `poll_test.go`의 `TestPollDelivered`(KST 파싱 검증), `templates_test.go`의 `TestListTemplatesMapping`(`UpdatedAt`).
- **실계정 확인 방법**: 발송 시각을 기록해 두고 조회 응답의 `receiveDate`와 대조한다. 9시간 차이가 나면 UTC다.

## 10. 해외 수신번호 표기 규칙이 없다

- **문서가 말하지 않는 것**: `recipientNo`는 최대 15자라고만 되어 있고, 국제 번호를 어떻게 표기하는지(국가번호 포함? `+` 포함? `00` 접두?) 명시가 없다.
- **우리 가정**: 우리 프로필의 phone은 E.164(`+8210…`)인데, `+82`로 시작하면 국내 표기(`010…`)로 바꾸고 그 외는 **숫자만 남긴다**(`+1415…` → `1415…`).
- **틀렸을 때 증상**: 해외 번호 발송이 전량 실패한다. 국내 서비스라 당장은 안 드러나다가, 해외 체류 고객이나 해외 번호로 가입한 사용자에게서만 실패가 몰린다. `invalid_target`으로 분류돼 **해당 프로필의 수신 번호가 무효 처리**될 수 있다는 점이 더 위험하다 — 번호는 멀쩡한데 표기만 틀린 것이다.
  참고로 알림톡 자체가 국내 카카오톡 사용자 대상이라 해외 번호 발송의 실효성이 애초에 낮다. 그래도 "표기 때문에 실패"와 "카카오톡 미가입이라 실패"는 구분돼야 한다.
- **의존하는 코드·테스트**: `send.go`의 `normalizeRecipient`. 테스트는 `nhn_test.go`의 `TestNormalizeRecipient`.
- **실계정 확인 방법**: 해외 번호로 발송해 `sendResults[].resultMessage`를 본다. 애초에 해외 발송을 막는다면 벤더에서 `+82`가 아닌 번호를 `invalid_target`으로 명시적으로 끊는 편이 낫다.

## 11. `resendTitle`을 SMS 대체발송에 실으면 거절되는지 모른다

- **문서가 말하지 않는 것**: `resendTitle`은 제목이므로 LMS 전용으로 보이지만, SMS에 실었을 때 무시되는지 거절되는지 명시가 없다.
- **우리 가정**: `resendType == "LMS"`일 때만 `resendTitle`을 싣는다. SMS면 `Fallback.Title`이 있어도 버린다.
- **틀렸을 때 증상**: 우리 쪽이 보수적이라 발송이 실패할 일은 없다. 다만 NHN이 SMS에서도 제목을 허용한다면 **운영자가 콘솔에 입력한 제목이 조용히 사라진다.** "SMS 대체발송에 제목을 넣었는데 안 나온다"는 문의로 드러난다.
- **의존하는 코드·테스트**: `send.go`의 `resendOf`. 테스트는 `nhn_test.go`의 `TestSendRequestShape`(LMS에 제목이 실리는 것만 확인한다).
- **실계정 확인 방법**: SMS 대체발송에 `resendTitle`을 실어 보내 거절되는지, 무시되는지, 본문에 붙는지 본다.

## 12. `templateStatus`에 REQ/APR/REJ 외의 값이 있는지 모른다

- **문서가 말하지 않는 것**: 문서에 나온 값은 `REQ`(심사요청)·`APR`(승인)·`REJ`(반려) 셋이다. 심사 취소, 중단, 삭제 대기 같은 중간 상태가 더 있는지 알 수 없다.
- **우리 가정**: 이 셋 외의 값은 전부 `pending`으로 접는다. **`approved`로 낙관하지 않는다.** 원본 코드는 `Template.VendorStatus`에 그대로 보존한다.
- **틀렸을 때 증상**:
  - 실제로 발송 가능한 상태가 새로 생기면 그 템플릿이 콘솔에서 **선택 불가**로 보인다. "카카오에서는 승인인데 Onda에서는 심사 중으로 보인다"는 문의로 드러난다. 안전한 쪽 실패다.
  - 반대 방향(미승인을 승인으로 오독)은 이 가정 때문에 일어나지 않는다. 그게 이 선택의 이유다.
- **의존하는 코드·테스트**: `templates.go`의 `templateStatusOf`, `nhn.go`의 `TemplateStatusRequested`/`Approved`/`Rejected`. 테스트는 `templates_test.go`의 `TestTemplateStatusUnknownIsPending`.
- **실계정 확인 방법**: 템플릿을 등록·심사요청·반려·삭제 각 단계를 거치며 목록 조회의 `templateStatus`를 기록한다. 반려 사유는 `comments[]`에 오는데, 우리 `Template`에는 실을 자리가 없어 지금은 파싱만 하고 버린다(13번 참조).

## 13. 템플릿 목록에 `updateDate`/`createDate`가 오는지 모른다

- **문서가 말하지 않는 것**: 템플릿 목록 항목의 필드 목록에 갱신 시각이 있는지 확실하지 않다.
- **우리 가정**: `updateDate` → `createDate` 순으로 찾고, 없으면 **조회 시각**(주입된 시계)을 쓴다. 제로 시각은 내보내지 않는다.
- **틀렸을 때 증상**: 모든 템플릿의 `UpdatedAt`이 매 동기화마다 갱신된다. 동기화 로직이 `UpdatedAt`으로 변경을 감지한다면 **매번 전량이 변경된 것으로 보여** 불필요한 쓰기가 일어나고, 변경 이력이 무의미해진다. 반대로 diff를 내용 기준으로 한다면 무해하다. `templatesync` 담당자와 함께 확인할 것.
  덧붙여 반려 사유(`comments[]`)는 파싱은 하지만 `alimtalk.Template`에 실을 필드가 없어 **버려진다.** 운영자가 콘솔에서 반려 사유를 볼 수 없다는 뜻이고, 이건 문서 모호가 아니라 우리 계약의 빈자리다.
- **의존하는 코드·테스트**: `templates.go`의 `templateUpdatedAt`·`nhnComment`. 테스트는 `templates_test.go`의 `TestListTemplatesMapping`.
- **실계정 확인 방법**: 목록 응답 원문에서 날짜 필드 이름을 확인한다.

## 14. 429에 `Retry-After` 헤더를 실제로 주는지 모른다

- **문서가 말하지 않는 것**: 한도 초과 응답에 `Retry-After`가 실리는지 명시가 없다.
- **우리 가정**: 있으면 쓰고(초 또는 HTTP-date), 없거나 0 이하면 **5초**(`defaultRateLimitRetryAfter`)를 쓴다. 0을 흘리면 백오프가 지수 재시도로 되돌아가 한도 초과가 길게 이어지고, 계약 테스트 `send_rate_limited`가 양수를 요구한다.
- **틀렸을 때 증상**: 5초가 NHN의 실제 한도 창보다 짧으면 재시도가 계속 429를 맞아 **한도 초과가 길어진다.** 큐가 밀리고 지연이 누적된다. 반대로 너무 길면 처리량이 불필요하게 낮아진다. 5초는 근거 없는 값이다.
  또한 `manifest.json`의 `limits`에 `rps`를 넣지 않았다(NHN 공표 수치를 못 찾았다). 엔진이 rps로 발신 속도를 조절한다면 지금은 조절 근거가 없다.
- **의존하는 코드·테스트**: `nhn.go`의 `(*Vendor).retryAfterOf`·`defaultRateLimitRetryAfter`·`classifyHTTP`. 테스트는 `nhn_test.go`의 `TestSendRateLimitWithoutRetryAfterHeader`, `TestSendErrorClasses`.
- **실계정 확인 방법**: 한도까지 밀어 429를 유도하고 응답 헤더 전체를 덤프한다. NHN 콘솔·계약서에서 초당·일일 한도 수치를 받아 `limits.rps`/`limits.daily`도 함께 채운다.

## 15. 베이스 URL 두 개 중 어느 쪽이 정본인지 모른다

- **문서가 말하지 않는 것**: `https://api-alimtalk.cloud.toast.com`과 `https://kakaotalk-bizmessage.api.nhncloudservice.com`이 둘 다 공표돼 있다. 어느 쪽이 정본이고 어느 쪽이 레거시인지, 한쪽이 언제 사라지는지 알 수 없다.
- **우리 가정**: `api-alimtalk.cloud.toast.com`을 기본값으로 두고, 크리덴셜·config의 `base_url`로 덮어쓸 수 있게 했다(계약 테스트가 httptest 서버를 무는 유일한 통로이기도 하다).
- **틀렸을 때 증상**: `toast.com` 도메인이 폐지되면 **전량 전송 실패**로 나타난다. `retryable`로 분류돼 재시도만 반복하다 재시도 한도에서 죽는다. 원인이 DNS·TLS 계층이라 응답 본문에 단서가 없다. 증상이 갑작스럽고 전량이면 이 항목을 의심할 것. 임시 대응은 `base_url` 재설정이라 코드 배포 없이 복구할 수 있다.
- **의존하는 코드·테스트**: `nhn.go`의 `DefaultBaseURL`·`credential.appkeyPath`. 테스트는 전부 `base_url`을 주입하므로 기본값 자체는 테스트가 검증하지 않는다.
- **실계정 확인 방법**: 두 호스트에 같은 요청을 보내 응답을 비교하고, NHN Cloud 콘솔의 현행 API 엔드포인트 안내를 확인한다.

## 16. 단가가 전부 추정치다

- **문서가 말하지 않는 것**: 조회 응답에 **건당 원가가 없다.** 알림톡·SMS·LMS 단가는 계약과 요금제에 따라 달라지는데 API가 알려주지 않는다.
- **우리 가정**: `manifest.json`의 `cost.per_message = 8.0 KRW`(알림톡), 코드 상수 `FallbackCostSMS = 20.0`, `FallbackCostLMS = 50.0`. 전부 근거 없는 자리표시자다. 다만 **셋이 서로 다르다는 사실**은 중요하다 — 같은 값으로 두면 SMS 전환이 원가에 안 잡힌다.
- **틀렸을 때 증상**: 채널별 원가 리포트가 통째로 틀린다. 틀린 방향이 일정하므로 **비교는 되지만 절대값은 못 믿는다.** 이 숫자로 예산을 잡거나 채널을 고르면 잘못된 결정이 나온다. 특히 알림톡 대비 SMS 배수(현재 2.5배)가 실제와 다르면 대체발송 정책 판단이 어긋난다.
- **의존하는 코드·테스트**: `nhn.go`의 `FallbackCostSMS`/`FallbackCostLMS`, `poll.go`의 `costFor`, `manifest.json`의 `cost`. 테스트는 `poll_test.go`의 `TestPollFallbackDeliveredVia`(값 자체가 아니라 "알림톡 단가와 다르다"를 확인한다).
- **실계정 확인 방법**: NHN Cloud 요금제·계약서에서 실 단가를 받아 세 값을 교체한다. API로는 알 수 없으므로 계약 정보를 사람이 넣어야 한다. 요금제별로 다르다면 `config` 스키마에 단가 필드를 두는 편이 낫다.

## 17. 401과 403을 어떻게 나눠 쓰는지 모른다

- **문서가 말하지 않는 것**: 잘못된 `SecretKey`가 401인지 403인지, 권한 없는 `senderKey`가 어느 쪽인지 명시가 없다.
- **우리 가정**: 둘 다 `credential_auth`로 똑같이 취급한다. 구분이 필요 없게 만들었다.
- **틀렸을 때 증상**: 거의 없다. 다만 401/403이 아닌 다른 코드(예: 400)로 인증 실패를 알려준다면 1번의 낱말 판정에 의존하게 되고, 그 낱말이 안 맞으면 **틀린 키가 verified로 저장된다.** `classifyHTTP`의 `default` 분기가 그 안전망이다.
- **의존하는 코드·테스트**: `nhn.go`의 `classifyHTTP`. 테스트는 `nhn_test.go`의 `TestSendErrorClasses`(403), `TestSendCredentialErrors`(401).
- **실계정 확인 방법**: 틀린 `SecretKey`, 남의 `senderKey`, 만료된 키로 각각 호출해 상태 코드를 기록한다.

## 18. `quickReplies`를 발송에서 지원하지 않기로 했다

- **문서가 말하지 않는 것**: 문서상 `recipientList[].quickReplies`가 존재한다. 즉 NHN은 지원할 가능성이 높다.
- **우리 가정**: `manifest.capabilities.content`에 `quick_replies`를 **선언하지 않았고**, 실린 요청은 `permanent_content`로 거절한다. 선언과 구현이 어긋나는 것보다 못 하는 것을 못 한다고 말하는 편이 낫다는 판단이다.
- **틀렸을 때 증상**: 틀린 게 아니라 **덜 만든 것**이다. 바로연결이 붙은 템플릿으로 발송하면 거절된다. 운영자에게는 "이 벤더는 바로연결을 지원하지 않습니다"로 정확히 보이므로 조용한 실패는 아니다. 계약 테스트 `unsupported_content`가 이 선언을 검사하고 있어, 나중에 지원을 켜려면 manifest와 `checkCapabilities`를 함께 고쳐야 한다(둘 중 하나만 고치면 계약 테스트가 잡는다).
- **의존하는 코드·테스트**: `manifest.json`의 `capabilities.content`, `send.go`의 `checkCapabilities`·`declaresContent`. 테스트는 `nhn_test.go`의 `TestSendRejectsUndeclaredContent`, 계약 테스트 `unsupported_content`.
- **실계정 확인 방법**: `quickReplies`를 실어 발송해 실제로 노출되는지 확인한 뒤 지원 여부를 정한다.

## 19. `scheduled_send`를 지시 명세에 없는데 켰다

- **경위**: 팀리드가 준 manifest 항목 목록에 `scheduled_send`가 없었다. 그런데 v2.3 스펙에 `requestDate`(60일 이내 예약)가 명시돼 있어 구현하고 `true`로 선언했다.
- **우리 가정**: `ScheduledAt`을 KST `yyyy-MM-dd HH:mm`으로 변환해 `requestDate`에 싣는다. 과거 시각과 60일 초과는 `permanent_content`로 거절한다.
- **틀렸을 때 증상**: 이 선언은 엔진이 `SendRequest.ScheduledAt`을 채울지 말지를 결정한다. 예약 발송이 실제로는 동작하지 않는데 켜져 있으면, **예약한 메시지가 즉시 나가거나 아예 안 나간다.** 9번(시간대)이 함께 틀리면 **9시간 어긋난 시각에 발송**된다 — 야간 발송 제한 위반으로 이어질 수 있는 유일한 항목이라 특히 주의할 것.
- **의존하는 코드·테스트**: `manifest.json`의 `capabilities.scheduled_send`, `send.go`의 `requestDate`·`buildSendBody`·`checkCapabilities`. 테스트는 `nhn_test.go`의 `TestSendScheduled`.
- **되돌리는 방법**: manifest의 한 줄과 `buildSendBody`의 `req.ScheduledAt` 분기만 빼면 된다. 팀리드 판단으로 끄기로 하면 그렇게 한다.
- **실계정 확인 방법**: 10분 뒤로 예약 발송하고 실제 도달 시각을 잰다. KST/UTC 문제가 여기서 함께 드러난다.

## 20. AC(채널추가) 버튼의 광고형 제한을 벤더에서 검사하지 않는다

- **경위**: 문서 모호가 아니라 우리 설계상의 빈자리다. AC 버튼은 광고추가형(AD)·복합형(MI) 템플릿에서만 허용되는데, 그 판정에는 승인 템플릿이 필요하다. 발송 시점의 벤더에는 템플릿이 없다(`SendRequest`에 안 실린다).
- **우리 가정**: `alimtalk.ValidateButtons(req.Buttons, true)`를 `isAd=true`로 호출해 이 규칙만 건너뛴다. 타입·이름 길이·필수 링크 같은 벤더 무관 규칙은 그대로 검사한다. AC 제한은 템플릿을 아는 상위(`alimtalk.ValidateSend`)가 이미 걸었다고 **믿는다.**
- **틀렸을 때 증상**: 상위 검증을 거치지 않는 발송 경로(테스트 발송, 관리 API 직접 호출 등)가 생기면 AC 버튼이 정보성 템플릿에 붙어 나간다. NHN 또는 카카오가 거절하므로 조용히 잘못 나가지는 않겠지만, 우리가 아니라 공급자가 잡는다는 뜻이라 **거절 이유가 운영자에게 늦게, 덜 친절하게** 전달된다.
- **의존하는 코드·테스트**: `send.go`의 `buildSendBody` 안의 `ValidateButtons(req.Buttons, true)` 호출. 상위는 `alimtalk/template.go`의 `ValidateSend`.
- **고치는 방법**: `SendRequest`에 템플릿 메시지 유형을 싣거나, 벤더가 템플릿 캐시를 참조하게 해야 한다. 둘 다 계약 변경이라 지금 범위 밖이다.

## 21. `recipientGroupingKey`에 우리 `message_id`를 싣기로 했다

- **경위**: 문서는 이 필드를 "수신자 그룹 구분용"으로만 설명한다. 길이 제한·허용 문자·중복 허용 여부가 명시돼 있지 않다.
- **우리 가정**: 우리 `message_id`(UUID)를 그대로 싣는다. NHN이 조회 응답에 되돌려주므로, 나중에 대량 조회로 전환할 때 우리 발송을 되찾는 단서가 된다.
- **틀렸을 때 증상**: 길이·문자 제한에 걸리면 **발송 요청 전체가 거절**된다. 1번의 낱말 판정에 따라 `permanent_content`로 잡힐 텐데, 사유 메시지가 `recipientGroupingKey`를 가리킬 것이므로 원인 추적은 어렵지 않다. 다만 **모든 발송이 실패**하는 형태라 눈에 띄게 아플 것이다.
- **의존하는 코드·테스트**: `send.go`의 `buildSendBody`. 테스트는 `nhn_test.go`의 `TestSendRequestShape`.
- **실계정 확인 방법**: UUID를 실어 한 건 보내고 거절되는지, 조회 응답에 그대로 돌아오는지 본다.

## 22. 결과 수집에 대량 조회 대신 단건 조회를 N회 쓴다

- **경위**: 문서에 대량(목록) 조회 API도 있지만, "수신자 한 명의 결과"를 주는 문서화된 경로는 단건 조회다. 대량 조회의 응답이 수신자별 결과를 어떤 모양으로 주는지 확신이 없었다.
- **우리 가정**: 미종결 접수 하나당 GET 한 번. 크리덴셜·한도 오류는 뒤 건도 똑같이 실패할 게 뻔하므로 즉시 멈추고, 그 외 개별 실패는 건너뛰고 계속한다.
- **틀렸을 때 증상**: 틀렸다기보다 **비싸다.** 발송량이 늘면 조회 호출이 발송량에 비례해 늘어 한도(14번)에 먼저 부딪힌다. "발송은 되는데 결과 수집이 밀린다", "폴링이 429를 자주 맞는다"로 드러난다. `manifest.capabilities.batch_max`를 1,000으로 선언해 두었지만 **현재 `Send`는 수신자 1명만 보낸다**는 점도 같이 기억할 것 — 선언은 스펙의 상한이고 구현은 아직 단건이다.
- **의존하는 코드·테스트**: `poll.go`의 `PollResults`·`pollOne`. 테스트는 `poll_test.go`의 `TestPollSkipsForeignReceipt`, `TestPollStopsOnCredentialAuth`.
- **실계정 확인 방법**: 대량 조회 API를 호출해 수신자별 `resultCode`·`messageStatus`가 그대로 오는지, 페이징이 어떻게 되는지 확인한다. 온다면 폴링을 대량 조회로 바꾸는 편이 낫다.
