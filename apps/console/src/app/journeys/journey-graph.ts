import { outputPorts, type JourneyGraphDefinition, type JourneyNode } from "@onda/journey-model";
import { newJourneyId, NODE_TOOLS } from "./journey-editor-model";

export type GraphDefinition = JourneyGraphDefinition;
export type GraphNode = GraphDefinition["nodes"][number];
export type GraphEdge = GraphDefinition["edges"][number];
export const ENTRY_EDGE_ID = "__entry_edge__";
export function entryEdgeId(definition: GraphDefinition): string {
  const used = new Set(definition.edges.map((edge) => edge.id));
  let id = ENTRY_EDGE_ID;
  while (used.has(id)) id += "_";
  return id;
}
export type PublishedABNodes = Record<string, { variants: Extract<JourneyNode, { type: "ab_split" }>["variants"] }>;
export type JourneyCapabilities = { graph_v2: boolean; supported_node_types: string[] };

/** Refuse malformed topology without flattening or silently repairing stored drafts. */
export function graphReadIssue(definition: GraphDefinition): string | null {
  const fail = "저장된 단계 연결을 읽을 수 없습니다. 원본을 보존했으며 관리자 확인이 필요합니다.";
  if (!Array.isArray(definition.nodes) || !definition.nodes.length || !Array.isArray(definition.edges)) return fail;
  const ids = definition.nodes.map((node) => node.id);
  if (ids.some((id) => typeof id !== "string" || !id) || new Set(ids).size !== ids.length ||
    !definition.start_node_id || !ids.includes(definition.start_node_id)) return fail;
  try {
    const nodes = new Map(definition.nodes.map((node) => [node.id, node]));
    const adjacency = new Map<string, string[]>();
    const edgeIds = new Set<string>();
    const outputs = new Set<string>();
    const indegree = new Map(ids.map((id) => [id, 0]));
    for (const node of definition.nodes) {
      if (!NODE_TOOLS.some((tool) => tool.type === node.type)) return "현재 콘솔에서 지원하지 않는 단계가 포함되어 있습니다. 원본은 변경하지 않았습니다.";
      if (node.type === "message") {
        // 채널은 푸시·이메일·알림톡 셋 중 하나. 알림톡을 빼면 알림톡 저니가 통째로
        // "연결을 읽을 수 없음"으로 잠긴다 — 워커 쪽에도 같은 누락이 있었다.
        const okPush = typeof node.push?.title === "string" && typeof node.push?.body === "string";
        const okEmail = typeof node.email?.subject === "string" && typeof node.email?.html === "string";
        const okAlimtalk = typeof node.alimtalk?.sender_id === "string"
          && typeof node.alimtalk?.template_code === "string";
        if (!okPush && !okEmail && !okAlimtalk) return fail;
      }
      if (node.type === "branch" && (!Array.isArray(node.condition?.groups) || node.condition.groups.some((group) => !Array.isArray(group.conditions) ||
        group.conditions.some((condition) => !condition || (condition.type === "attribute" && typeof condition.key !== "string") ||
          (condition.type === "event" && typeof condition.event !== "string"))))) return fail;
    }
    for (const edge of definition.edges) {
      const node = nodes.get(edge.source);
      const key = JSON.stringify([edge.source, edge.source_port]);
      if (!edge.id || edgeIds.has(edge.id) || !node || outputs.has(key) || !outputPorts(node).some((port) => port.id === edge.source_port) ||
        (edge.target !== null && !indegree.has(edge.target))) return fail;
      edgeIds.add(edge.id); outputs.add(key);
      if (edge.target) {
        indegree.set(edge.target, indegree.get(edge.target)! + 1);
        adjacency.set(edge.source, [...(adjacency.get(edge.source) ?? []), edge.target]);
      }
    }
    for (const node of definition.nodes) for (const port of outputPorts(node)) {
      if (!outputs.has(JSON.stringify([node.id, port.id]))) return fail;
    }
    const queue = ids.filter((id) => indegree.get(id) === 0);
    for (let i = 0; i < queue.length; i++) for (const target of adjacency.get(queue[i]!) ?? []) {
      indegree.set(target, indegree.get(target)! - 1);
      if (indegree.get(target) === 0) queue.push(target);
    }
    if (queue.length !== ids.length || reachableNodes(definition).size !== ids.length) return fail;
  } catch { return fail; }
  return null;
}

export function nodeTitle(node: JourneyNode): string {
  if (node.type === "message") {
    if (node.push?.title?.trim()) return node.push.title;
    if (node.email?.subject?.trim()) return node.email.subject;
  }
  if (node.type === "event_wait" && node.event_name.trim()) return node.event_name;
  return NODE_TOOLS.find((tool) => tool.type === node.type)?.label ?? node.type;
}

export function outgoingEdges(definition: GraphDefinition, id: string): GraphEdge[] {
  const node = definition.nodes.find((item) => item.id === id);
  return node ? outputPorts(node).flatMap((port) => definition.edges.filter((edge) => edge.source === id && edge.source_port === port.id)) : [];
}

export function reachableNodes(definition: GraphDefinition, start = definition.start_node_id): Set<string> {
  const found = new Set<string>();
  const next = start ? [start] : [];
  const adjacency = new Map<string, string[]>();
  for (const edge of definition.edges) {
    if (edge.target) adjacency.set(edge.source, [...(adjacency.get(edge.source) ?? []), edge.target]);
  }
  while (next.length) {
    const id = next.pop()!;
    if (found.has(id)) continue;
    found.add(id);
    next.push(...(adjacency.get(id) ?? []));
  }
  return found;
}

/** Split exactly one path; inserting a decision initially preserves the old continuation on every output. */
export function insertOnEdge(definition: GraphDefinition, edgeId: string, node: JourneyNode): GraphDefinition {
  if (!node.id || definition.nodes.some((existing) => existing.id === node.id)) throw new Error("새 단계의 ID가 올바르지 않습니다.");
  const next = structuredClone(definition);
  const edge = next.edges.find((item) => item.id === edgeId);
  const atEntry = edgeId === entryEdgeId(definition);
  if (!atEntry && !edge) throw new Error("추가할 경로를 다시 선택해 주세요.");
  const target = atEntry ? next.start_node_id : edge!.target;
  if (edge) edge.target = node.id;
  else next.start_node_id = node.id;
  next.nodes.push(structuredClone(node) as GraphNode);
  next.edges.push(...outputPorts(node).map((port) => ({
    id: newJourneyId("edge"), source: node.id!, source_port: port.id, target,
  })));
  return next;
}

export function connectionIssue(definition: GraphDefinition, source: string, port: string, target: string | null): string | null {
  const node = definition.nodes.find((item) => item.id === source);
  if (!node || !outputPorts(node).some((item) => item.id === port)) return "연결할 경로를 찾을 수 없습니다.";
  if (target && !definition.nodes.some((item) => item.id === target)) return "연결할 단계를 찾을 수 없습니다.";
  if (target === source || (target && reachableNodes(definition, target).has(source))) return "이전 단계로 되돌아가는 연결은 만들 수 없습니다.";
  const next = structuredClone(definition);
  next.edges = next.edges.filter((edge) => edge.source !== source || edge.source_port !== port);
  next.edges.push({ id: "preview", source, source_port: port, target });
  const before = reachableNodes(definition);
  const after = reachableNodes(next);
  if ([...before].some((id) => !after.has(id))) return "이 연결은 기존 단계를 흐름에서 분리합니다. 단계 삭제에서 영향을 먼저 확인해 주세요.";
  return null;
}

export function connectRoute(definition: GraphDefinition, source: string, port: string, target: string | null): GraphDefinition {
  const issue = connectionIssue(definition, source, port, target);
  if (issue) throw new Error(issue);
  const next = structuredClone(definition);
  const edge = next.edges.find((item) => item.source === source && item.source_port === port);
  if (edge) edge.target = target;
  else next.edges.push({ id: newJourneyId("edge"), source, source_port: port, target });
  return next;
}

const linear = (node?: JourneyNode) => node?.type === "message" || node?.type === "delay";

function movablePair(definition: GraphDefinition, id: string, offset: -1 | 1): [GraphNode, GraphNode] | null {
  const selected = definition.nodes.find((node) => node.id === id);
  if (!linear(selected)) return null;
  const incoming = (nodeId: string) => definition.edges.filter((edge) => edge.target === nodeId);
  const previous = offset === -1 ? incoming(id) : outgoingEdges(definition, id);
  if (previous.length !== 1) return null;
  const neighborId = offset === -1 ? previous[0]!.source : previous[0]!.target;
  const neighbor = definition.nodes.find((node) => node.id === neighborId);
  if (!linear(neighbor) || !selected || !neighbor) return null;
  const first = offset === -1 ? neighbor : selected;
  const second = offset === -1 ? selected : neighbor;
  if (incoming(first.id).length > 1 || incoming(second.id).length !== 1 ||
    outgoingEdges(definition, first.id).length !== 1 || outgoingEdges(definition, second.id).length !== 1) return null;
  return [first, second];
}

export function canMoveNode(definition: GraphDefinition, id: string, offset: -1 | 1): boolean {
  return movablePair(definition, id, offset) !== null;
}

/** Rewire adjacent linear nodes without changing their IDs, configuration or neighboring branches. */
export function moveLinearNode(definition: GraphDefinition, id: string, offset: -1 | 1): GraphDefinition {
  const pair = movablePair(definition, id, offset);
  if (!pair) throw new Error("같은 경로의 메시지·시간 대기 사이에서만 순서를 바꿀 수 있습니다.");
  const [first, second] = pair;
  const next = structuredClone(definition);
  const firstOut = next.edges.find((edge) => edge.source === first.id)!;
  const secondOut = next.edges.find((edge) => edge.source === second.id)!;
  const tail = secondOut.target;
  for (const edge of next.edges) if (edge.target === first.id) edge.target = second.id;
  if (next.start_node_id === first.id) next.start_node_id = second.id;
  firstOut.target = tail;
  secondOut.target = first.id;
  return next;
}

export interface RemovalPreview {
  definition: GraphDefinition;
  removed: GraphNode[];
  sharedKept: GraphNode[];
}

export function previewRemoval(definition: GraphDefinition, id: string, keepPort?: string): RemovalPreview {
  const node = definition.nodes.find((item) => item.id === id);
  if (!node) throw new Error("삭제할 단계를 찾을 수 없습니다.");
  const outputs = outgoingEdges(definition, id);
  if (outputs.length > 1 && !keepPort) throw new Error("삭제 후 보존할 경로를 선택해 주세요.");
  const kept = outputs.find((edge) => !keepPort || edge.source_port === keepPort);
  if (!kept) throw new Error("보존할 다음 경로를 찾을 수 없습니다.");
  const next = structuredClone(definition);
  const beforeReachable = reachableNodes(definition);
  const descendants = reachableNodes(definition, id);
  next.nodes = next.nodes.filter((item) => item.id !== id);
  next.edges = next.edges.filter((edge) => edge.source !== id).map((edge) => edge.target === id ? { ...edge, target: kept.target } : edge);
  if (next.start_node_id === id) next.start_node_id = kept.target;
  const afterReachable = reachableNodes(next);
  const removedIds = new Set([id, ...[...beforeReachable].filter((item) => !afterReachable.has(item))]);
  next.nodes = next.nodes.filter((item) => !removedIds.has(item.id));
  next.edges = next.edges.filter((edge) => !removedIds.has(edge.source) && (!edge.target || !removedIds.has(edge.target)));
  if (!next.nodes.length || !next.start_node_id) throw new Error("저니에는 최소 하나의 단계가 필요합니다.");
  return {
    definition: next,
    removed: definition.nodes.filter((item) => removedIds.has(item.id)),
    sharedKept: next.nodes.filter((item) => descendants.has(item.id) &&
      definition.edges.filter((edge) => edge.target === item.id).length > 1),
  };
}

export function renewExperiment(definition: GraphDefinition, id: string, replacementId = newJourneyId()): GraphDefinition {
  const node = definition.nodes.find((item) => item.id === id);
  if (node?.type !== "ab_split") throw new Error("A/B 분기를 선택해 주세요.");
  if (definition.nodes.some((item) => item.id === replacementId)) throw new Error("실험 ID가 중복됩니다.");
  const next = structuredClone(definition);
  next.nodes = next.nodes.map((item) => item.id === id ? { ...item, id: replacementId } : item);
  next.edges = next.edges.map((edge) => ({ ...edge,
    source: edge.source === id ? replacementId : edge.source,
    target: edge.target === id ? replacementId : edge.target,
  }));
  if (next.start_node_id === id) next.start_node_id = replacementId;
  return next;
}

/** Range of configured waits along one possible route; excludes quiet hours and execution latency. */
export function pathDurationRange(definition: GraphDefinition): { min: number; max: number } | null {
  if (!definition.start_node_id) return null;
  const nodes = new Map(definition.nodes.map((node) => [node.id, node]));
  const edges = new Map<string, GraphEdge[]>();
  const indegree = new Map(definition.nodes.map((node) => [node.id, 0]));
  for (const edge of definition.edges) {
    if (!nodes.has(edge.source) || (edge.target && !nodes.has(edge.target))) return null;
    edges.set(edge.source, [...(edges.get(edge.source) ?? []), edge]);
    if (edge.target) indegree.set(edge.target, indegree.get(edge.target)! + 1);
  }
  const order = [...nodes.keys()].filter((id) => indegree.get(id) === 0);
  for (let i = 0; i < order.length; i++) for (const edge of edges.get(order[i]!) ?? []) {
    if (!edge.target) continue;
    indegree.set(edge.target, indegree.get(edge.target)! - 1);
    if (indegree.get(edge.target) === 0) order.push(edge.target);
  }
  if (order.length !== nodes.size) return null;
  const ranges = new Map<string, { min: number; max: number }>();
  for (let i = order.length - 1; i >= 0; i--) {
    const node = nodes.get(order[i]!)!;
    const outputs = edges.get(node.id) ?? [];
    const ports = outputPorts(node);
    if (outputs.length !== ports.length || ports.some((port) => outputs.filter((edge) => edge.source_port === port.id).length !== 1)) return null;
    const cost = node.type === "delay" ? node.duration_seconds : node.type === "event_wait" ? node.timeout_seconds : 0;
    if (!Number.isSafeInteger(cost) || cost < 0 || (cost === 0 && (node.type === "delay" || node.type === "event_wait"))) return null;
    const paths = outputs.map((edge) => {
      const tail = edge.target ? ranges.get(edge.target) : { min: 0, max: 0 };
      if (!tail) return null;
      return {
        min: tail.min + (node.type === "delay" || (node.type === "event_wait" && edge.source_port === "timeout") ? cost : 0),
        max: tail.max + cost,
      };
    });
    if (!paths.length || paths.some((path) => !path)) return null;
    ranges.set(node.id, { min: Math.min(...paths.map((path) => path!.min)), max: Math.max(...paths.map((path) => path!.max)) });
  }
  return ranges.get(definition.start_node_id) ?? null;
}
