import type { SegmentDSL } from "@onda/segment-dsl";
import { EMAIL_PROVIDERS, messageChannel, outputPorts, type JourneyDefinition, type JourneyNode, type ValidationIssue } from "./index";

const ATTRIBUTE_OPS = new Set([
  "eq", "neq", "gt", "gte", "lt", "lte", "in", "exists", "not_exists", "contains",
  "before", "after", "in_last_days", "not_in_last_days",
]);
// Go's time.Duration stores nanoseconds in a signed int64.
const MAX_DURATION_SECONDS = 9_223_372_036;
const validDuration = (n: number) => Number.isSafeInteger(n) && n > 0 && n <= MAX_DURATION_SECONDS;
const nonempty = (s: unknown): s is string => typeof s === "string" && s.trim().length > 0;
const utf8 = new TextEncoder();
const scalar = (value: unknown) => typeof value === "string" || typeof value === "boolean" || (typeof value === "number" && Number.isFinite(value));
function validDate(value: unknown): boolean {
  if (typeof value !== "string") return false;
  const match = /^(\d{4}-\d{2}-\d{2})(?:T([01]\d|2[0-3]):([0-5]\d):([0-5]\d)(?:\.\d{1,9})?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d))?$/.exec(value);
  if (!match || !Number.isFinite(Date.parse(value))) return false;
  return new Date(`${match[1]}T00:00:00Z`).toISOString().slice(0, 10) === match[1];
}

export function validateBranchCondition(dsl: SegmentDSL): string[] {
  const errors: string[] = [];
  if (!dsl || dsl.version !== 1 || !["AND", "OR"].includes(dsl.operator) || !Array.isArray(dsl.groups) || !dsl.groups.length) {
    return ["조건 그룹을 하나 이상 설정하세요"];
  }
  if (dsl.groups.length > 20) errors.push("조건 그룹은 최대 20개입니다");
  if (Object.keys(dsl).some(key => !["version", "operator", "groups"].includes(key))) errors.push("지원하지 않는 조건 정의 필드입니다");
  for (const group of dsl.groups) {
    if (!group || !["AND", "OR"].includes(group.operator) || !Array.isArray(group.conditions) || !group.conditions.length) {
      errors.push("각 그룹에 조건을 하나 이상 설정하세요"); continue;
    }
    if (group.conditions.length > 50) errors.push("그룹당 조건은 최대 50개입니다");
    if (Object.keys(group).some(key => !["operator", "conditions"].includes(key))) errors.push("지원하지 않는 조건 그룹 필드입니다");
    for (const condition of group.conditions) {
      if (condition?.type === "event") {
        if (Object.keys(condition).some(key => !["type", "event", "op", "window_days"].includes(key))) errors.push("이벤트 속성·횟수 필터는 아직 지원하지 않습니다");
        if (!nonempty(condition.event)) errors.push("조건 이벤트 이름이 필요합니다");
        if (!["performed", "not_performed"].includes(condition.op)) errors.push("행동 조건은 수행·미수행만 지원합니다");
        const days = condition.window_days ?? 30;
        if (!Number.isInteger(days) || days < 1 || days > 180) errors.push("행동 조회 기간은 1~180일이어야 합니다");
      } else if (condition?.type === "attribute") {
        if (Object.keys(condition).some(key => !["type", "key", "op", "value"].includes(key))) errors.push("지원하지 않는 속성 조건 필드입니다");
        if (!nonempty(condition.key)) errors.push("고객 속성을 선택하세요");
        if (!ATTRIBUTE_OPS.has(condition.op)) errors.push("지원하지 않는 속성 연산자입니다");
        if (!["exists", "not_exists"].includes(condition.op) && condition.value === undefined) errors.push("속성 비교값이 필요합니다");
        if (["eq", "neq"].includes(condition.op) && !scalar(condition.value)) errors.push("비교값은 문자열·숫자·참/거짓이어야 합니다");
        if (["gt", "gte", "lt", "lte"].includes(condition.op) && !(typeof condition.value === "number" && Number.isFinite(condition.value))) errors.push("크기 비교값은 숫자여야 합니다");
        if (condition.op === "contains" && typeof condition.value !== "string") errors.push("배열에 포함될 문자열을 입력하세요");
        if (condition.op === "in" && (!Array.isArray(condition.value) || !condition.value.length || !condition.value.every(scalar))) errors.push("포함 목록에는 문자열·숫자·참/거짓 값을 하나 이상 입력하세요");
        if (["before", "after"].includes(condition.op) && !validDate(condition.value)) errors.push("날짜는 YYYY-MM-DD 또는 시간대가 포함된 ISO 날짜·시간으로 입력하세요");
        if (["in_last_days", "not_in_last_days"].includes(condition.op) &&
            !(typeof condition.value === "number" && Number.isInteger(condition.value) && condition.value > 0 && condition.value <= 106_751)) {
          errors.push("속성 조회 기간은 1~106751일 사이의 정수여야 합니다");
        }
      } else errors.push("분기는 고객 속성과 이벤트 수행·미수행 조건만 지원합니다");
    }
  }
  return [...new Set(errors)];
}

export function validateJourney(def: JourneyDefinition): ValidationIssue[] {
  const issues: ValidationIssue[] = [];
  const issue = (message: string, details: Partial<ValidationIssue> = {}) => issues.push({ level: "error", message, ...details });
  if (def.schema_version !== undefined && def.schema_version !== 1 && def.schema_version !== 2) issue("지원하지 않는 저니 정의 버전입니다", { field: "schema_version" });
  const graph = def.schema_version === 2;
  if (!def.entry?.type) issue("진입(entry) 규칙이 설정되지 않았습니다");
  else if (def.entry.type === "blast" && !def.entry.segment_id) issue("일괄 진입은 대상 세그먼트가 필요합니다");
  else if (def.entry.type === "trigger" && !def.entry.trigger_event?.trim()) issue("트리거 진입은 이벤트가 필요합니다");
  else if (!["blast", "trigger"].includes(def.entry.type)) issue("지원하지 않는 진입 규칙입니다");
  if (!def.nodes?.length) issue("노드가 하나 이상 필요합니다");
  if ((def.nodes?.length ?? 0) > 65_535) issue("저니 단계 수가 지원 범위를 초과했습니다");
  let hasMessage = false;
  for (const [index, node] of (def.nodes ?? []).entries()) {
    const details = { node_index: index, node_id: node.id };
    const fail = (message: string, field?: string) => issue(message, { ...details, field });
    if (!graph && !["message", "delay"].includes(node.type)) fail("분기·이벤트 대기는 v2 그래프에서만 지원합니다");
    switch (node.type) {
      case "message": {
        hasMessage = true;
        // 채널: push 또는 email 중 하나. 각 채널의 필수 내용이 비면 오류.
        // 채널은 정확히 하나. 둘 이상이면 어느 쪽으로 나갈지 모호해진다.
        switch (messageChannel(node)) {
          case "email":
            if (!node.email!.subject?.trim() || !node.email!.html?.trim()) fail("빈 이메일 노드입니다", "email");
            if (node.email!.provider !== undefined && !(EMAIL_PROVIDERS as readonly string[]).includes(node.email!.provider)) {
              fail("지원하지 않는 이메일 발송기입니다", "email");
            }
            break;
          case "alimtalk":
            if (!node.alimtalk!.sender_id?.trim()) fail("발신프로필을 선택하세요", "alimtalk");
            if (!node.alimtalk!.template_code?.trim()) fail("알림톡 템플릿을 선택하세요", "alimtalk");
            if (node.alimtalk!.fallback && !node.alimtalk!.fallback.text?.trim()) {
              fail("대체발송 문구를 입력하세요", "alimtalk");
            }
            break;
          case "push":
            if (!node.push!.title?.trim() || !node.push!.body?.trim()) fail("빈 메시지 노드입니다", "push");
            break;
          default:
            fail("메시지 노드는 푸시·이메일·알림톡 중 하나여야 합니다");
        }
        break;
      }
      case "delay":
        if (!validDuration(node.duration_seconds)) fail("대기 시간은 0보다 큰 정수여야 합니다", "duration_seconds");
        break;
      case "branch":
        for (const message of validateBranchCondition(node.condition)) fail(message, "condition");
        break;
      case "event_wait":
        if (!node.event_name?.trim()) fail("기다릴 이벤트 이름이 필요합니다", "event_name");
        if (!validDuration(node.timeout_seconds)) fail("이벤트 제한시간은 0보다 큰 정수여야 합니다", "timeout_seconds");
        if (node.event_name && node.event_name === def.exit?.conversion_event) issue("전환 종료와 대기 이벤트가 같습니다. 전환 종료가 먼저 적용됩니다", { ...details, field: "event_name", level: "warning" });
        break;
      case "ab_split": {
        const variants = node.variants ?? [];
        if (variants.length < 2 || variants.length > 4) fail("A/B 경로는 2~4개여야 합니다", "variants");
        if (variants.some(v => !nonempty(v.id)) || new Set(variants.map(v => v.id)).size !== variants.length) fail("A/B 경로 ID는 비어 있지 않고 고유해야 합니다", "variants");
        if (variants.some(v => !nonempty(v.label))) fail("A/B 경로 이름이 필요합니다", "variants");
        if (variants.some(v => !Number.isInteger(v.weight) || v.weight < 1 || v.weight > 100) || variants.reduce((sum, v) => sum + v.weight, 0) !== 100) fail("A/B 비율은 각 1% 이상인 정수이며 합계가 100%여야 합니다", "variants");
        break;
      }
      default: fail("지원하지 않는 단계 유형입니다");
    }
  }
  if (def.nodes?.length && !hasMessage) issue("메시지 노드가 하나 이상 필요합니다");
  if (graph) validateGraph(def, issues);
  else if (def.nodes?.at(-1)?.type === "delay") issue("마지막 노드가 대기입니다 — 발송 없이 종료됩니다", { level: "warning", node_index: def.nodes.length - 1 });
  if (!["marketing", "transactional"].includes(def.settings?.category)) issue("메시지 카테고리(marketing/transactional)가 필요합니다");
  const reentry = def.settings?.reentry;
  if (reentry !== "never" && reentry !== "always" && !(reentry && typeof reentry === "object" && Number.isInteger(reentry.after_days) && reentry.after_days > 0)) issue("올바른 재진입 규칙이 필요합니다", { field: "settings.reentry" });
  if (graph && reentry && typeof reentry === "object" && reentry.after_days > 106_751) issue("재진입 대기 기간은 최대 106751일입니다", { field: "settings.reentry.after_days" });
  return issues;
}

function validateGraph(def: JourneyDefinition, issues: ValidationIssue[]) {
  const fail = (message: string, details: Partial<ValidationIssue> = {}) => issues.push({ level: "error", message, ...details });
  const nodes = new Map<string, JourneyNode>();
  for (const [index, node] of (def.nodes ?? []).entries()) {
    if (!nonempty(node.id) || utf8.encode(node.id).byteLength > 128) fail("고유한 단계 ID가 필요합니다 (UTF-8 최대 128바이트)", { node_index: index, field: "id" });
    else if (nodes.has(node.id)) fail("중복된 단계 ID입니다", { node_id: node.id, field: "id" });
    else nodes.set(node.id, node);
  }
  if (!def.start_node_id || !nodes.has(def.start_node_id)) fail("시작 단계를 연결하세요", { field: "start_node_id" });
  if (!Array.isArray(def.edges)) { fail("단계 연결 정보가 필요합니다", { field: "edges" }); return; }
  const ids = new Set<string>();
  const outputs = new Set<string>();
  const adjacency = new Map<string, string[]>();
  for (const edge of def.edges) {
    const detail = { edge_id: edge.id, node_id: edge.source };
    if (!nonempty(edge.id) || ids.has(edge.id)) fail("연결 ID가 비어 있거나 중복되었습니다", detail);
    ids.add(edge.id);
    const source = nodes.get(edge.source);
    if (!source) fail("연결의 출발 단계가 없습니다", detail);
    else if (!outputPorts(source).some(port => port.id === edge.source_port)) fail("지원하지 않는 출력 경로입니다", detail);
    if (edge.target !== null && !nodes.has(edge.target)) fail("연결의 도착 단계가 없습니다", detail);
    const key = JSON.stringify([edge.source, edge.source_port]);
    if (outputs.has(key)) fail("출력 경로마다 하나의 연결만 허용합니다", detail);
    outputs.add(key);
    if (edge.target !== null) adjacency.set(edge.source, [...(adjacency.get(edge.source) ?? []), edge.target]);
  }
  for (const [id, node] of nodes) for (const port of outputPorts(node)) {
    if (!outputs.has(JSON.stringify([id, port.id]))) fail(`${port.label} 경로를 다음 단계 또는 종료에 연결하세요`, { node_id: id, field: "edges" });
  }
  // Iterative topological validation avoids stack overflows on untrusted large graphs.
  const indegree = new Map([...nodes.keys()].map(id => [id, 0]));
  for (const targets of adjacency.values()) for (const target of targets) if (indegree.has(target)) indegree.set(target, indegree.get(target)! + 1);
  const queue = [...nodes.keys()].filter(id => indegree.get(id) === 0);
  for (let i = 0; i < queue.length; i++) for (const target of adjacency.get(queue[i]!) ?? []) {
    if (!indegree.has(target)) continue;
    const degree = indegree.get(target)! - 1; indegree.set(target, degree);
    if (degree === 0) queue.push(target);
  }
  if (queue.length !== nodes.size) fail("반복 연결은 지원하지 않습니다. 순환 경로를 제거하세요", { field: "edges" });
  const reached = new Set<string>();
  const pending = def.start_node_id ? [def.start_node_id] : [];
  while (pending.length) {
    const id = pending.pop()!;
    if (reached.has(id)) continue;
    reached.add(id); pending.push(...(adjacency.get(id) ?? []));
  }
  for (const id of nodes.keys()) if (!reached.has(id)) fail("시작 단계에서 도달할 수 없는 단계입니다", { node_id: id });
}
