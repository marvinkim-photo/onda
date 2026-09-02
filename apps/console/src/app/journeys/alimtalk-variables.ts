import type { MessageNode } from "@onda/journey-model";

/**
 * 저니 알림톡 노드의 순수 모델 — 치환자 추출·매핑 검사·미리보기 렌더, 그리고 채널 전환 불변식.
 * 콘솔 vitest가 `@/` 별칭을 해석하지 못하므로 컴포넌트와 분리한다(email-provider-links.ts와 동일).
 */

/**
 * 카카오 치환자 표기 `#{변수명}`. 저니의 `{{변수}}`와 다르다 —
 * 워커 template.go의 varPattern과 같은 정규식이어야 미리보기와 실제 발송이 갈리지 않는다.
 */
const VAR_PATTERN = /#\{([^}]{1,50})\}/g;

/** 승인 본문에서 치환자 이름을 등장 순서대로, 중복 없이 뽑는다 (워커 alimtalk.Variables와 동일 규약). */
export function extractVariables(content: string): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const match of content.matchAll(VAR_PATTERN)) {
    const name = match[1]!.trim();
    if (!name || seen.has(name)) continue;
    seen.add(name);
    out.push(name);
  }
  return out;
}

export interface TemplateLike {
  content?: string;
  variables?: readonly string[];
}

/**
 * 이 템플릿이 요구하는 치환자.
 * 동기화가 채워 준 `variables`를 우선 믿고, 비어 있으면 본문에서 직접 뽑는다
 * (벤더가 목록을 주지 않는 경우에도 매핑 UI가 비지 않게).
 */
export function templateVariables(template: TemplateLike | null | undefined): string[] {
  if (!template) return [];
  const declared = (template.variables ?? []).map((v) => v.trim()).filter(Boolean);
  if (declared.length > 0) return Array.from(new Set(declared));
  return extractVariables(template.content ?? "");
}

/**
 * 값이 비어 있는 치환자.
 *
 * 알림톡은 미치환 값을 빈 문자열로 넘길 수 없다 — 워커 Render가 오류로 막는다.
 * 저장은 되지만 발송 시점에 전부 실패하므로, 편집 화면에서 미리 드러내야 한다.
 */
export function unmappedVariables(
  variables: readonly string[],
  mapping: Readonly<Record<string, string>> | undefined,
): string[] {
  return variables.filter((name) => !(mapping?.[name] ?? "").trim());
}

/** 템플릿이 더 이상 요구하지 않는데 매핑에 남아 있는 값 — 템플릿을 바꾸면 생긴다. */
export function staleVariables(
  variables: readonly string[],
  mapping: Readonly<Record<string, string>> | undefined,
): string[] {
  const wanted = new Set(variables);
  return Object.keys(mapping ?? {}).filter((name) => !wanted.has(name));
}

/**
 * 미리보기 — 승인 본문에 매핑 값을 끼운다.
 * 값이 없는 치환자는 `#{이름}` 그대로 남겨 어디가 비었는지 보이게 한다.
 * `{{프로필속성}}` 값은 치환하지 않고 그대로 보여 준다 — 실제 값은 발송 시점에 정해진다.
 */
export function renderTemplatePreview(
  content: string,
  mapping: Readonly<Record<string, string>> | undefined,
): string {
  return content.replace(VAR_PATTERN, (token, rawName: string) => {
    const value = (mapping?.[rawName.trim()] ?? "").trim();
    return value || token;
  });
}

/** 값이 `{{프로필속성}}` 한 개로만 이루어졌는가 — 도움말에서 리터럴과 구분해 보여 준다. */
export function isProfileReference(value: string): boolean {
  return /^\{\{\s*[a-zA-Z0-9_]+\s*\}\}$/.test(value.trim());
}

export type MessageChannel = "push" | "email" | "alimtalk";

/**
 * 채널 전환 — 메시지 노드는 push·email·alimtalk 중 **정확히 하나**만 채워야 한다
 * (journey-model의 messageChannel이 그렇지 않으면 null을 돌려주고 발행 검증이 막는다).
 * 다른 채널의 키를 남기지 않도록 노드를 통째로 새로 만든다.
 */
export function withMessageChannel(node: MessageNode, channel: MessageChannel): MessageNode {
  switch (channel) {
    case "push":
      return { id: node.id, type: "message", push: node.push ?? { title: "", body: "" } };
    case "email":
      return { id: node.id, type: "message", email: node.email ?? { subject: "", html: "" } };
    case "alimtalk":
      return {
        id: node.id,
        type: "message",
        alimtalk: node.alimtalk ?? { sender_id: "", template_code: "" },
      };
  }
}
