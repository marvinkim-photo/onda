package nhn

import (
	"strings"

	"github.com/ondahq/onda/apps/worker/internal/channel"
)

// 공급자 결과를 재시도 정책으로 옮기는 곳.
//
// NHN은 실패를 HTTP 상태가 아니라 본문의 결과 코드·메시지로 알려주고, 코드 표가 문서에
// 전량 공개돼 있지 않다(실 계정에서 확인해야 하는 목록이 이 파일의 TODO다).
// 그래서 분류는 두 단계다.
//
//  1. 확실히 아는 코드는 표(headerCodeClass)로 못 박는다.
//  2. 나머지는 결과 메시지의 낱말로 판정한다. 낱말 판정은 정확하지 않지만,
//     "모르면 permanent_content"보다 낫다 — 인증 실패를 permanent_content로 흘리면
//     channel.Verifier.judge가 그것을 "인증은 통과"로 읽어 틀린 키가 verified로 저장된다.
//
// 판정 순서는 credential_auth → rate_limited → retryable → invalid_target → 기본값이다.
// 인증을 가장 먼저 보는 이유는 위와 같고, invalid_target을 늦게 보는 이유는 "수신번호"라는
// 낱말이 본문 오류 메시지에도 섞여 들어오기 때문이다.

// headerCodeClass — 확인된 header.resultCode 매핑.
//
// TODO(실 NHN 샌드박스 계정 확보 시 최우선 검증): v2.3 문서는 resultCode를 int로만
// 정의하고 값 목록을 주지 않는다. 아래 두 값은 NHN Cloud 공통 규약(0=성공,
// -9999=시스템 오류)만 확실하고, 그 외 코드는 관측한 적이 없어 넣지 않았다.
// 실계정에서 코드 표를 뽑으면 낱말 판정 대신 이 표가 1차 근거가 되어야 한다.
var headerCodeClass = map[int]channel.FailureClass{
	-9999: channel.FailureRetryable, // NHN Cloud 공통 시스템 오류
}

// 낱말 사전. 한국어 응답이 기본이지만 영어로 오는 경로도 있어 둘 다 담는다.
var (
	authWords = []string{
		"인증", "권한", "허용되지 않", "유효하지 않은 키", "appkey", "app key",
		"secretkey", "secret key", "unauthorized", "forbidden", "not allowed",
		"invalid key", "발신프로필", "발신 프로필", "senderkey", "sender key",
		"등록되지 않은 발신", "차단된 계정", "서비스 이용", "ip 주소", "ip address",
	}
	rateWords = []string{
		"요청 한도", "전송 한도", "발송 한도", "일 한도", "초당", "요청량",
		"rate limit", "too many request", "quota", "throttl",
	}
	retryWords = []string{
		"일시", "잠시 후", "시스템 오류", "내부 오류", "서버 오류", "처리 중 오류",
		"timeout", "time out", "internal", "temporar", "unavailable", "try again",
	}
	targetWords = []string{
		"수신번호", "수신 번호", "휴대폰번호", "휴대폰 번호", "전화번호", "수신자",
		"recipient", "phone", "mobile number", "미가입", "차단한 수신", "탈퇴",
	}
	// contentWords — 본문·템플릿 문제. 조회 시점의 실패는 기본이 invalid_target이지만
	// (접수 때 이미 본문 검사를 통과했으므로 대개 수신자 사유다), 본문을 명시적으로
	// 가리키는 사유는 재시도·수신자 정리가 아니라 템플릿 수정으로 풀어야 한다.
	contentWords = []string{
		"템플릿", "본문", "치환", "변수", "승인", "메시지 내용", "바이트",
		"template", "content", "parameter", "variable",
	}
	// expiredWords — 조회 보존 기간(90일)이 지나 결과를 알 수 없다.
	// 실패가 아니라 "못 알아냈다"라서 Event.Expired로 간다(Terminal과 구분).
	expiredWords = []string{
		"조회 기간", "보관 기간", "보존 기간", "기간 만료", "기간이 만료", "조회할 수 없",
		"expired", "retention", "no longer available",
	}
)

// classifyMessage — 결과 코드·메시지를 실패 분류로 환원한다.
// def는 아무 낱말도 걸리지 않았을 때의 기본값이다.
func classifyMessage(code int, msg string, def channel.FailureClass) channel.FailureClass {
	if c, ok := headerCodeClass[code]; ok {
		return c
	}
	l := strings.ToLower(msg)
	switch {
	case containsAny(l, authWords):
		return channel.FailureCredentialAuth
	case containsAny(l, rateWords):
		return channel.FailureRateLimited
	case containsAny(l, retryWords):
		return channel.FailureRetryable
	case containsAny(l, targetWords):
		return channel.FailureInvalidTarget
	case containsAny(l, contentWords):
		return channel.FailurePermanentContent
	default:
		return def
	}
}

// classifyRecipient — recipientList 항목별 결과(sendResults[].resultCode != 0)의 분류.
//
// 요청 자체는 받아들여졌는데 이 수신자만 떨어진 것이므로 수신자를 가리키면 invalid_target,
// 아니면 본문·템플릿 문제로 본다. 인증·한도 낱말이 여기 섞여 오면 상위 판정을 따른다.
func classifyRecipient(code int, msg string) channel.FailureClass {
	if containsAny(strings.ToLower(msg), targetWords) {
		return channel.FailureInvalidTarget
	}
	return classifyMessage(code, msg, channel.FailurePermanentContent)
}

// isExpired — 조회 결과가 "보존 기간이 지나 알 수 없다"인가.
func isExpired(msg string) bool { return containsAny(strings.ToLower(msg), expiredWords) }

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
