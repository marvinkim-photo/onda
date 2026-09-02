/**
 * 알림톡 설정 화면의 순수 헬퍼 — 리포트 지원 범위·템플릿 유형·웹훅 URL.
 * 컴포넌트와 분리해 단위 테스트가 React·API 클라이언트 없이 돌게 한다(email-provider-links.ts와 동일).
 */

/** 커넥터가 보고할 수 있는 상태의 전체 집합. manifest.lifecycle.reports가 이 중 일부를 선언한다. */
export const LIFECYCLE_REPORTS = ["accepted", "sent", "delivered", "read", "failed"] as const;
export type LifecycleReport = (typeof LIFECYCLE_REPORTS)[number];

export const REPORT_LABELS: Record<LifecycleReport, string> = {
  accepted: "접수",
  sent: "발송",
  delivered: "도달",
  read: "열람",
  failed: "실패",
};

export interface ReportSupport {
  supported: string[];
  unsupported: string[];
}

/**
 * 벤더가 무엇을 보고하고 무엇을 보고하지 않는지.
 *
 * "미지원"과 "0건"은 다르다 — 알림톡은 대부분 열람을 보고하지 않으므로, 열람 0을 성과로 읽으면 안 된다.
 * 매니페스트에 없는 상태도 그대로 지원 목록에 남긴다(모르는 상태를 숨기면 표시가 거짓이 된다).
 */
export function reportSupport(reports: readonly string[]): ReportSupport {
  const declared = new Set(reports);
  const supported = reports.map((r) => REPORT_LABELS[r as LifecycleReport] ?? r);
  const unsupported = LIFECYCLE_REPORTS.filter((r) => !declared.has(r)).map((r) => REPORT_LABELS[r]);
  return { supported, unsupported };
}

/** "접수 · 발송 · 도달까지 보고 · 열람 미지원" 같은 한 줄 요약. */
export function reportSummary(reports: readonly string[]): string {
  const { supported, unsupported } = reportSupport(reports);
  if (supported.length === 0) return "보고하는 상태가 선언되지 않았습니다 — 발송 결과를 수집할 수 없습니다.";
  const head = `${supported.join(" · ")} 보고`;
  return unsupported.length === 0 ? head : `${head} · ${unsupported.join(" · ")} 미지원`;
}

/** 콘솔이 안내하는 커넥터 웹훅 URL — 벤더 콘솔에 등록할 주소. */
export function connectorWebhookUrl(apiUrl: string, appId: string, connectorId: string): string {
  return `${apiUrl.replace(/\/+$/, "")}/v1/webhooks/connectors/${appId}/${connectorId}`;
}

/** 카카오 알림톡 템플릿 유형. AD·MI는 광고성이라 발송 규제가 다르다. */
export const MESSAGE_TYPE_LABELS: Record<string, string> = {
  BA: "기본형",
  EX: "부가정보형",
  AD: "광고추가형",
  MI: "복합형",
};

export function messageTypeLabel(messageType: string): string {
  const code = messageType.trim().toUpperCase();
  if (!code) return "미상";
  return MESSAGE_TYPE_LABELS[code] ? `${code} ${MESSAGE_TYPE_LABELS[code]}` : code;
}

/**
 * 광고성 템플릿인가 — AD(광고추가형)·MI(복합형).
 * 법적 구분이다: 야간(21시~익일 08시) 발송이 제한되고 수신거부가 적용된다. 표시용 장식이 아니다.
 */
export function isAdMessageType(messageType: string): boolean {
  const code = messageType.trim().toUpperCase();
  return code === "AD" || code === "MI";
}

export const AD_TEMPLATE_NOTICE =
  "광고성 템플릿입니다 — 야간(21시~08시) 발송이 제한되고 수신거부가 적용됩니다.";

/** 워커가 정규화하는 템플릿 상태(vendor.go: approved · pending · rejected). */
export const TEMPLATE_STATUS_LABELS: Record<string, string> = {
  approved: "승인",
  pending: "심사 중",
  rejected: "반려",
  unknown: "상태 미상",
};

export function templateStatusLabel(status: string): string {
  return TEMPLATE_STATUS_LABELS[status] ?? status;
}

export function isTemplateApproved(status: string): boolean {
  return status === "approved";
}

/**
 * 벤더 목록에서 사라진 템플릿에 워커가 찍는 표식 (templatesync/store.go: VendorStatusMissing).
 * 행을 지우지 않는 이유: 미동기화 템플릿은 발송 전 검증을 건너뛰므로, 삭제가 곧 가드를
 * 조용히 끄는 일이 된다. 저니가 아직 참조하고 있을 수도 있어 눈에 보여야 한다.
 */
export const VENDOR_STATUS_MISSING = "ONDA_MISSING_IN_VENDOR";

export function isMissingInVendor(vendorStatus: string | undefined): boolean {
  return vendorStatus === VENDOR_STATUS_MISSING;
}

/**
 * 템플릿의 사용 가능 상태를 한 마디로. 벤더에서 사라진 것은 반려와 구분한다 —
 * 반려는 심사 결과이고, 소실은 벤더 쪽에서 없어진 것이라 대응이 다르다.
 */
export function templateStateLabel(status: string, vendorStatus?: string): string {
  if (isMissingInVendor(vendorStatus)) return "벤더에서 사라짐";
  return templateStatusLabel(status);
}

/** 승인되지 않은 템플릿을 저니에서 고를 수 없는 이유 — 노드 select의 disabled 사유로 쓴다. */
export function templateBlockReason(status: string, vendorStatus?: string): string | null {
  if (isTemplateApproved(status)) return null;
  if (isMissingInVendor(vendorStatus)) return "벤더에서 사라짐";
  const label = templateStatusLabel(status);
  return vendorStatus ? `${label} (벤더 상태 ${vendorStatus})` : label;
}

export const MISSING_IN_VENDOR_NOTICE =
  "벤더 템플릿 목록에서 사라졌습니다 — 벤더 쪽에서 삭제됐거나 발신프로필이 바뀐 것입니다. 이 템플릿으로는 발송할 수 없습니다.";

/**
 * 서버가 보낸 한국어 메시지를 그대로 꺼낸다.
 *
 * ApiError.message는 "API 501"이라 사용자에게 쓸모가 없다. 실제 사유는 body.message에 있고,
 * Nest의 검증 실패는 그것이 문자열 배열로 온다. 벤더 미지원·미구현 안내를 우리가 다시 쓰지 않고
 * 서버 문구를 그대로 보여 주기 위한 헬퍼다.
 */
export function serverMessage(error: unknown, fallback: string): string {
  const body = (error as { body?: unknown } | null)?.body;
  const message = (body as { message?: unknown } | null)?.message;
  if (typeof message === "string" && message.trim()) return message;
  if (Array.isArray(message)) {
    const joined = message.filter((m): m is string => typeof m === "string").join(" · ");
    if (joined) return joined;
  }
  return fallback;
}
