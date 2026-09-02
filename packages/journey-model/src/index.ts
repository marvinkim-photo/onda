import type { SegmentDSL } from "@onda/segment-dsl";

/** v1 remains an immutable linear definition. v2 stores an exclusive DAG. */
export type MessageCategory = "marketing" | "transactional";
export interface JourneyDefinition {
  schema_version?: 1 | 2;
  entry: EntryRule;
  nodes: JourneyNode[];
  start_node_id?: string | null;
  edges?: JourneyEdge[];
  exit: ExitRule;
  settings: JourneySettings;
}
export interface JourneyGraphDefinition extends JourneyDefinition {
  schema_version: 2;
  nodes: Array<JourneyNode & { id: string }>;
  start_node_id: string | null;
  edges: JourneyEdge[];
}
export interface EntryRule {
  type: "blast" | "trigger";
  segment_id?: string;
  trigger_event?: string;
}
interface NodeIdentity { id?: string }
/** 이메일 발송기(provider). 미지정 = 앱의 활성(최근 검증) 발송기. credentials.kind와 동일 값. */
export const EMAIL_PROVIDERS = ["email_smtp", "email_nhn", "email_resend"] as const;
export type EmailProvider = (typeof EMAIL_PROVIDERS)[number];
export type JourneyNode = MessageNode | DelayNode | BranchNode | EventWaitNode | ABSplitNode;
/**
 * 알림톡 노드. 승인된 템플릿을 고르고 치환자만 매핑한다.
 * 본문을 저니에서 쓰지 않는 이유: 알림톡은 카카오 심사를 통과한 템플릿과 정확히 일치해야 한다.
 * variables 값은 {{프로필속성}} 표기를 쓸 수 있고 발송 시 렌더된다.
 * 발송기(벤더)는 노드가 아니라 앱의 채널 배선(channel_connectors)이 정한다 — 벤더를 바꿔도 저니는 그대로다.
 */
export interface AlimtalkContent {
  sender_id: string;      // alimtalk_senders.id — 발신프로필
  template_code: string;  // 승인 템플릿 코드
  variables?: Record<string, string>;
  /** 대체발송. 벤더가 지원하면 벤더에 위임하고, 아니면 엔진이 폴백한다. */
  fallback?: { type: "SMS" | "LMS"; text: string; title?: string };
}
export interface MessageNode extends NodeIdentity {
  type: "message";
  // 채널 선택: push · email · alimtalk 중 정확히 하나.
  push?: { title: string; body: string; image_url?: string; deep_link?: string };
  email?: { subject: string; html: string; provider?: EmailProvider };
  alimtalk?: AlimtalkContent;
}
/** 메시지 노드의 채널. 정확히 하나가 채워져 있어야 한다. */
export function messageChannel(node: MessageNode): "push" | "email" | "alimtalk" | null {
  const set = [node.push && "push", node.email && "email", node.alimtalk && "alimtalk"].filter(Boolean);
  return set.length === 1 ? (set[0] as "push" | "email" | "alimtalk") : null;
}
export interface DelayNode extends NodeIdentity {
  type: "delay";
  duration_seconds: number;
}
export interface BranchNode extends NodeIdentity {
  type: "branch";
  condition: SegmentDSL;
}
export interface EventWaitNode extends NodeIdentity {
  type: "event_wait";
  event_name: string;
  timeout_seconds: number;
}
export interface ABVariant { id: string; label: string; weight: number }
export interface ABSplitNode extends NodeIdentity {
  type: "ab_split";
  variants: ABVariant[];
}
export interface JourneyEdge {
  id: string;
  source: string;
  source_port: string;
  target: string | null;
}
export interface ExitRule { conversion_event?: string }
export interface JourneySettings {
  category: MessageCategory;
  reentry: "never" | "always" | { after_days: number };
}
export interface ValidationIssue {
  level: "error" | "warning";
  message: string;
  node_index?: number;
  node_id?: string;
  edge_id?: string;
  field?: string;
}
export type PublishedABNodes = Record<string, { variants: ABVariant[] }>;

export function outputPorts(node: JourneyNode): Array<{ id: string; label: string }> {
  switch (node.type) {
    case "branch": return [{ id: "true", label: "충족" }, { id: "false", label: "미충족" }];
    case "event_wait": return [{ id: "matched", label: "이벤트 발생" }, { id: "timeout", label: "시간 초과" }];
    case "ab_split": return (node.variants ?? []).map(v => ({ id: v.id, label: `${v.label} · ${v.weight}%` }));
    default: return [{ id: "next", label: "다음" }];
  }
}

/** Presentation adapter only. Never flattens or repairs a persisted v2 graph. */
export function toGraphDefinition(def: JourneyDefinition): JourneyGraphDefinition {
  if (def.schema_version === 2) return structuredClone(def) as JourneyGraphDefinition;
  if (def.schema_version !== undefined && def.schema_version !== 1) {
    throw new Error("지원하지 않는 저니 정의 버전입니다");
  }
  const copy = structuredClone(def);
  const nodes = copy.nodes.map((node, i) => ({ ...node, id: node.id ?? `legacy-${i}` }));
  return {
    ...copy, schema_version: 2, nodes, start_node_id: nodes[0]?.id ?? null,
    edges: nodes.map((node, i) => ({
      id: `legacy-edge-${i}`, source: node.id, source_port: "next", target: nodes[i + 1]?.id ?? null,
    })),
  };
}

export { validateJourney, validateBranchCondition } from "./validation";
export { collectPublishedABNodes, validatePublishedABNodes } from "./published";
export function hasErrors(issues: ValidationIssue[]): boolean {
  return issues.some(issue => issue.level === "error");
}
export function campaignToJourney(input: {
  segment_id: string; push: MessageNode["push"]; category: MessageCategory;
}): JourneyDefinition {
  return {
    entry: { type: "blast", segment_id: input.segment_id },
    nodes: [{ type: "message", push: input.push }], exit: {},
    settings: { category: input.category, reentry: "never" },
  };
}
