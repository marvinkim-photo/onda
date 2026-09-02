import { describe, expect, it } from "vitest";
import {
  VENDOR_STATUS_MISSING,
  connectorWebhookUrl,
  isAdMessageType,
  isMissingInVendor,
  isTemplateApproved,
  messageTypeLabel,
  reportSummary,
  reportSupport,
  serverMessage,
  templateBlockReason,
  templateStateLabel,
  templateStatusLabel,
} from "./alimtalk-labels";

describe("reportSupport", () => {
  it("알림톡 벤더가 열람을 보고하지 않음을 드러낸다", () => {
    const { supported, unsupported } = reportSupport(["accepted", "sent", "delivered", "failed"]);
    expect(supported).toEqual(["접수", "발송", "도달", "실패"]);
    expect(unsupported).toEqual(["열람"]);
  });

  it("선언에 없는 상태는 그대로 남긴다 (모르는 상태를 숨기면 표시가 거짓이 된다)", () => {
    expect(reportSupport(["accepted", "bounced"]).supported).toEqual(["접수", "bounced"]);
  });
});

describe("reportSummary", () => {
  it("보고 범위와 미지원을 한 줄로 정직하게 쓴다", () => {
    expect(reportSummary(["accepted", "sent", "delivered", "failed"])).toBe(
      "접수 · 발송 · 도달 · 실패 보고 · 열람 미지원",
    );
  });

  it("전부 보고하면 미지원 문구를 붙이지 않는다", () => {
    expect(reportSummary(["accepted", "sent", "delivered", "read", "failed"])).toBe(
      "접수 · 발송 · 도달 · 열람 · 실패 보고",
    );
  });

  it("선언이 비면 결과를 수집할 수 없다고 경고한다", () => {
    expect(reportSummary([])).toContain("수집할 수 없습니다");
  });
});

describe("connectorWebhookUrl", () => {
  const appId = "22222222-2222-4222-8222-222222222222";

  it("API 주소 뒤에 /v1/webhooks/connectors/{appId}/{connectorId}를 붙인다", () => {
    expect(connectorWebhookUrl("https://api.example.com", appId, "alimtalk_mock")).toBe(
      `https://api.example.com/v1/webhooks/connectors/${appId}/alimtalk_mock`,
    );
  });

  it("끝의 슬래시가 중복되지 않는다", () => {
    expect(connectorWebhookUrl("https://api.example.com//", appId, "x")).toBe(
      `https://api.example.com/v1/webhooks/connectors/${appId}/x`,
    );
  });
});

describe("messageTypeLabel", () => {
  it("카카오 코드에 한국어 이름을 붙인다", () => {
    expect(messageTypeLabel("BA")).toBe("BA 기본형");
    expect(messageTypeLabel("ex")).toBe("EX 부가정보형");
    expect(messageTypeLabel("AD")).toBe("AD 광고추가형");
    expect(messageTypeLabel("MI")).toBe("MI 복합형");
  });

  it("모르는 코드는 코드 그대로, 빈 값은 미상", () => {
    expect(messageTypeLabel("ZZ")).toBe("ZZ");
    expect(messageTypeLabel("")).toBe("미상");
  });
});

describe("isAdMessageType", () => {
  it("광고추가형·복합형만 광고성이다 (야간 발송 제한·수신거부 대상)", () => {
    expect(isAdMessageType("AD")).toBe(true);
    expect(isAdMessageType("MI")).toBe(true);
    expect(isAdMessageType("mi")).toBe(true);
    expect(isAdMessageType("BA")).toBe(false);
    expect(isAdMessageType("EX")).toBe(false);
    expect(isAdMessageType("")).toBe(false);
  });
});

describe("템플릿 상태", () => {
  it("워커가 정규화하는 상태에 한국어 라벨이 있다", () => {
    expect(templateStatusLabel("approved")).toBe("승인");
    expect(templateStatusLabel("pending")).toBe("심사 중");
    expect(templateStatusLabel("rejected")).toBe("반려");
    expect(templateStatusLabel("weird")).toBe("weird");
  });

  it("승인된 템플릿만 저니에서 고를 수 있다", () => {
    expect(isTemplateApproved("approved")).toBe(true);
    expect(isTemplateApproved("pending")).toBe(false);
    expect(templateBlockReason("approved")).toBeNull();
    expect(templateBlockReason("rejected", "REJ_02")).toBe("반려 (벤더 상태 REJ_02)");
    expect(templateBlockReason("pending")).toBe("심사 중");
  });
});

describe("벤더에서 사라진 템플릿", () => {
  it("워커가 쓰는 표식과 같은 문자열을 본다 (templatesync/store.go)", () => {
    expect(VENDOR_STATUS_MISSING).toBe("ONDA_MISSING_IN_VENDOR");
    expect(isMissingInVendor("ONDA_MISSING_IN_VENDOR")).toBe(true);
    expect(isMissingInVendor("REJ_02")).toBe(false);
    expect(isMissingInVendor(undefined)).toBe(false);
  });

  it("소실을 반려와 구분해 보여 준다 — 심사 결과가 아니라 벤더 쪽 삭제다", () => {
    expect(templateStateLabel("rejected", VENDOR_STATUS_MISSING)).toBe("벤더에서 사라짐");
    expect(templateStateLabel("rejected", "REJ_02")).toBe("반려");
    expect(templateStateLabel("approved")).toBe("승인");
  });

  it("저니 select의 사유도 소실로 적는다 (반려 문구를 쓰면 대응이 달라진다)", () => {
    expect(templateBlockReason("rejected", VENDOR_STATUS_MISSING)).toBe("벤더에서 사라짐");
  });
});

describe("serverMessage", () => {
  it("서버가 보낸 한국어 문구를 그대로 쓴다 (ApiError.message는 'API 501'이라 쓸모없다)", () => {
    const err = { status: 501, body: { statusCode: 501, message: "이 벤더는 템플릿 동기화를 지원하지 않습니다" } };
    expect(serverMessage(err, "대체 문구")).toBe("이 벤더는 템플릿 동기화를 지원하지 않습니다");
  });

  it("Nest 검증 실패의 문자열 배열을 이어 붙인다", () => {
    expect(serverMessage({ status: 400, body: { message: ["a", "b"] } }, "x")).toBe("a · b");
  });

  it("꺼낼 문구가 없으면 대체 문구를 쓴다", () => {
    expect(serverMessage(new Error("boom"), "대체 문구")).toBe("대체 문구");
    expect(serverMessage(null, "대체 문구")).toBe("대체 문구");
    expect(serverMessage({ body: { message: "   " } }, "대체 문구")).toBe("대체 문구");
  });
});
