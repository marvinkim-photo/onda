"use client";

import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import {
  Background,
  BackgroundVariant,
  BaseEdge,
  EdgeLabelRenderer,
  Handle,
  MiniMap,
  Panel,
  Position,
  ReactFlow,
  ReactFlowProvider,
  getBezierPath,
  useNodesInitialized,
  useUpdateNodeInternals,
  useReactFlow,
  useStore,
  useViewport,
  type Edge,
  type EdgeProps,
  type EdgeTypes,
  type FitViewOptions,
  type Node as FlowNode,
  type NodeProps,
  type NodeTypes,
  type OnNodesChange,
  type Connection,
} from "@xyflow/react";
import { outputPorts, type JourneyNode } from "@onda/journey-model";
import { formatDuration, NODE_TOOLS } from "./journey-editor-model";
import { connectionIssue, entryEdgeId, type GraphDefinition } from "./journey-graph";
import { journeyStructureKey, layoutJourney, type CardKind } from "./journey-layout";
import { JourneyIcon, type JourneyIconName } from "./journey-ui";
import "./journey-canvas.css";

export interface JourneyCanvasProps {
  definition: GraphDefinition;
  /** "entry", "exit", or `node:${nodeId}`. Flow IDs cannot collide with stored IDs. */
  selectedId: string;
  segmentName?: string;
  editable: boolean;
  supportedTypes?: string[];
  onSelect: (id: string) => void;
  onInsert?: (edgeId: string, type: JourneyNode["type"]) => void;
  onConnect?: (source: string, port: string, target: string | null) => void;
  onConnectionError?: (message: string) => void;
  /** Node report labels are real values supplied by the version-scoped report. */
  nodeMetrics?: Record<string, string>;
  edgeMetrics?: Record<string, string>;
}

type CardData = {
  kind: CardKind;
  label: string;
  title: string;
  detail: string;
  icon: JourneyIconName;
  ariaLabel: string;
  ports: Array<{ id: string; label: string }>;
  editable: boolean;
  metric?: string;
  needsSetup?: boolean;
  onSelect: JourneyCanvasProps["onSelect"];
};
type JourneyGraphNode = FlowNode<CardData, "journeyCard">;
type InsertData = {
  editable: boolean;
  label: string;
  pathLabel?: string;
  metric?: string;
  supportedTypes?: string[];
  onInsert?: JourneyCanvasProps["onInsert"];
};
type JourneyGraphEdge = Edge<InsertData, "journeyInsert">;

const MIN_ZOOM = 0.2;
const MAX_ZOOM = 1.6;
const FIT_OPTIONS: FitViewOptions = {
  padding: { top: "48px", bottom: "60px", left: "24px", right: "24px" },
  minZoom: MIN_ZOOM,
  maxZoom: 1,
};

const JourneyFlowCard = memo(function JourneyFlowCard({ id, data, selected }: NodeProps<JourneyGraphNode>) {
  const updateNodeInternals = useUpdateNodeInternals();
  const portKey = data.ports.map((port) => port.id).join("|");
  useLayoutEffect(() => { updateNodeInternals(id); }, [id, portKey, updateNodeInternals]);
  return (
    <>
      {data.kind !== "entry" && <Handle type="target" position={Position.Top} isConnectable={data.editable} className={data.editable ? "is-editable" : undefined} />}
      <button type="button" className={`j-canvas-card j-canvas-card-${data.kind} nodrag nopan${selected ? " is-selected" : ""}`}
        aria-label={data.ariaLabel} aria-pressed={selected} title={data.title || data.detail} onClick={() => data.onSelect(id)}>
        <span className="j-canvas-card-heading">
          <span className="j-canvas-card-icon"><JourneyIcon name={data.icon} size={18} /></span>
          <span className="j-canvas-card-label">{data.label}</span>
          {data.needsSetup && <span className="j-canvas-setup-dot" title="설정 필요" />}
          {data.kind === "entry" && <span className="j-canvas-start-tag">시작</span>}
        </span>
        {data.kind !== "exit" && <span className={`j-canvas-card-title${data.needsSetup ? " is-placeholder" : ""}`} title={data.title}>{data.title}</span>}
        {(data.kind === "message" || data.kind === "event_wait") && <span className="j-canvas-card-detail" title={data.detail}>{data.detail}</span>}
        {data.metric && <span className="j-canvas-node-metric">{data.metric}</span>}
        {data.ports.length > 1 && <span className="j-canvas-ports" style={{ gridTemplateColumns: `repeat(${data.ports.length}, minmax(0, 1fr))` }}>
          {data.ports.map((port) => <span key={port.id} title={port.label}>{port.label}</span>)}
        </span>}
      </button>
      {data.ports.map((port, index) => <Handle key={port.id} id={port.id} type="source" position={Position.Bottom}
        style={{ left: `${(index + .5) * 100 / data.ports.length}%` }} isConnectable={data.editable && data.kind !== "entry"}
        className={data.editable && data.kind !== "entry" ? "is-editable" : undefined} title={`${port.label} 경로 연결`} />)}
    </>
  );
});

const JourneyInsertEdge = memo(function JourneyInsertEdge({ id, sourceX, sourceY, targetX, targetY, data }: EdgeProps<JourneyGraphEdge>) {
  const [open, setOpen] = useState(false);
  const container = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const menu = useRef<HTMLDivElement>(null);
  const firstOption = useRef<HTMLButtonElement>(null);
  const [placement, setPlacement] = useState({ side: "below", shiftX: 0, width: 210, maxHeight: 260 });
  const [path, middleX, middleY] = getBezierPath({ sourceX, sourceY, targetX, targetY, sourcePosition: Position.Bottom, targetPosition: Position.Top, curvature: .3 });
  // Keep labels near the separated output ports when several paths share a target.
  // For this downward cubic, t=.25 has x=.15625 and y=.296875 of the endpoint delta.
  const labelX = data?.pathLabel && targetY > sourceY ? sourceX + (targetX - sourceX) * .15625 : middleX;
  const labelY = data?.pathLabel && targetY > sourceY ? sourceY + (targetY - sourceY) * .296875 : middleY;
  const showPopover = open && data?.editable;

  useLayoutEffect(() => {
    if (!showPopover) return;
    const canvas = trigger.current?.closest(".j-canvas");
    if (!canvas) return;
    let frame = 0;
    const positionMenu = () => {
      const button = trigger.current;
      const popup = menu.current;
      if (!button || !popup) return;
      const bounds = canvas.getBoundingClientRect();
      const buttonBounds = button.getBoundingClientRect();
      // The label layer scales with zoom; DOM rectangles are in screen pixels.
      const scale = buttonBounds.width / button.offsetWidth || 1;
      const below = bounds.bottom - buttonBounds.bottom - 10;
      const above = buttonBounds.top - bounds.top - 10;
      const menuHeight = (popup.scrollHeight + 2) * scale;
      const side = below < menuHeight && above > below ? "above" : "below";
      const width = Math.min(210, Math.max(120, (bounds.width - 24) / scale));
      const rawLeft = buttonBounds.left + buttonBounds.width / 2 - width * scale / 2;
      const safeLeft = Math.max(bounds.left + 12, Math.min(rawLeft, bounds.right - 12 - width * scale));
      const next = { side, width, shiftX: (safeLeft - rawLeft) / scale, maxHeight: Math.max(80, (side === "above" ? above : below) / scale) };
      setPlacement((current) => current.side === next.side && Math.abs(current.width - next.width) < .5 &&
        Math.abs(current.shiftX - next.shiftX) < .5 && Math.abs(current.maxHeight - next.maxHeight) < .5 ? current : next);
    };
    const schedulePosition = () => {
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(positionMenu);
    };
    positionMenu();
    const observer = new ResizeObserver(schedulePosition);
    observer.observe(canvas);
    if (menu.current) observer.observe(menu.current);
    canvas.addEventListener("wheel", schedulePosition, { capture: true, passive: true });
    window.addEventListener("resize", schedulePosition);
    return () => {
      cancelAnimationFrame(frame);
      observer.disconnect();
      canvas.removeEventListener("wheel", schedulePosition, true);
      window.removeEventListener("resize", schedulePosition);
    };
  }, [showPopover]);

  useEffect(() => {
    if (!showPopover) return;
    firstOption.current?.focus({ preventScroll: true });
    const dismiss = (event: PointerEvent) => {
      if (event.target instanceof Node && !container.current?.contains(event.target)) setOpen(false);
    };
    document.addEventListener("pointerdown", dismiss);
    return () => document.removeEventListener("pointerdown", dismiss);
  }, [showPopover]);

  return (
    <>
      <BaseEdge id={id} path={path} style={{ stroke: "var(--j-canvas-edge, #bcccd2)", strokeWidth: 1.6 }} interactionWidth={0} />
      {data && (
        <EdgeLabelRenderer>
          <div ref={container} className="j-canvas-insert nodrag nopan"
            style={{ transform: `translate(-50%, -50%) translate(${labelX}px, ${labelY}px)`, zIndex: open ? 10 : 1 }}
            onBlur={(event) => {
              if (!event.currentTarget.contains(event.relatedTarget)) setOpen(false);
            }}
            onKeyDown={(event) => {
              if (event.key === "Escape") {
                event.preventDefault();
                setOpen(false);
                trigger.current?.focus({ preventScroll: true });
              }
            }}>
            {data.editable && <button ref={trigger} type="button" className={`j-canvas-add${open ? " is-open" : ""}`}
              aria-label={`${data.label}에 단계 추가`}
              title="이 위치에 단계 추가" aria-expanded={Boolean(showPopover)} aria-haspopup="dialog"
              aria-controls={showPopover ? `${id}-insert` : undefined} onClick={() => setOpen(!open)}>
              <JourneyIcon name="plus" size={16} />
            </button>}
            {(data.pathLabel || data.metric) && <span className="j-canvas-edge-label" title={[data.pathLabel, data.metric].filter(Boolean).join(" · ")}>
              {data.pathLabel && <span>{data.pathLabel}</span>}{data.metric && <strong>{data.metric}</strong>}
            </span>}
            {showPopover && (
              <div ref={menu} id={`${id}-insert`} className={`j-canvas-insert-menu nowheel is-${placement.side}`} role="dialog" aria-label="추가할 단계 선택"
                style={{ width: placement.width, maxHeight: placement.maxHeight, marginLeft: placement.shiftX }}>
                <p>{data.label}에 추가</p>
                {NODE_TOOLS.map((tool, index) => <button key={tool.type} ref={index === 0 ? firstOption : undefined} type="button"
                  disabled={Boolean(data.supportedTypes && !data.supportedTypes.includes(tool.type))}
                  onClick={() => { setOpen(false); data.onInsert?.(id, tool.type); }}>
                  <span className={`j-canvas-insert-${tool.type}`}><JourneyIcon name={tool.icon} size={18} /></span>
                  <span>{tool.label}<small>{tool.description}</small></span>
                </button>)}
              </div>
            )}
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  );
});

const nodeTypes = { journeyCard: JourneyFlowCard } satisfies NodeTypes;
const edgeTypes = { journeyInsert: JourneyInsertEdge } satisfies EdgeTypes;

function miniMapColor(node: JourneyGraphNode): string {
  return { entry: "var(--j-accent, #0f766e)", message: "var(--j-blue, #3978ed)", delay: "var(--j-amber, #c18324)", branch: "var(--j-purple, #8662b3)", event_wait: "var(--j-cyan, #278f9d)", ab_split: "var(--j-rose, #b66085)", exit: "var(--j-muted, #72818b)" }[node.data.kind];
}

function CanvasViewport({ layoutKey, structureKey }: { layoutKey: string; structureKey: string }) {
  const { fitView } = useReactFlow();
  const lastFit = useRef("");
  const initialized = useNodesInitialized();
  const width = useStore((state) => state.width);
  const height = useStore((state) => state.height);
  const appliedLayoutKey = useStore((state) => state.nodes.map((node) => {
    const internal = state.nodeLookup.get(node.id);
    return [node.id, internal?.internals.positionAbsolute.x, internal?.internals.positionAbsolute.y,
      internal?.measured.width, internal?.measured.height].join(":");
  }).join("|"));

  useEffect(() => {
    // Controlled props reach the store in a separate effect. Wait for the new
    // order, positions, and measured sizes before fitting after add/remove/move.
    if (!initialized || appliedLayoutKey !== layoutKey || !width || !height) return;
    // Dismissing save feedback while typing changes the canvas height. It must
    // not reset the user's zoom; only topology or an actual window resize does.
    const fitKey = `${structureKey}:${width}:${window.innerHeight}`;
    if (lastFit.current === fitKey) return;
    const frame = requestAnimationFrame(() => { lastFit.current = fitKey; void fitView(FIT_OPTIONS); });
    return () => cancelAnimationFrame(frame);
  }, [layoutKey, structureKey, appliedLayoutKey, initialized, width, height, fitView]);

  return null;
}

function CanvasControls({ mobile, moveMode, onToggleMove }: { mobile: boolean; moveMode: boolean; onToggleMove: () => void }) {
  const { zoomIn, zoomOut, fitView } = useReactFlow();
  const { zoom } = useViewport();

  return (
    <Panel position="bottom-left" className="j-canvas-controls nodrag nopan" role="toolbar" aria-label="캔버스 보기 조정">
      <button type="button" aria-label="축소" title="축소" disabled={zoom <= MIN_ZOOM + 0.001} onClick={() => { void zoomOut(); }}>
        <JourneyIcon name="minus" size={17} />
      </button>
      <span aria-label={`현재 확대 비율 ${Math.round(zoom * 100)}퍼센트`}>{Math.round(zoom * 100)}%</span>
      <button type="button" aria-label="확대" title="확대" disabled={zoom >= MAX_ZOOM - 0.001} onClick={() => { void zoomIn(); }}>
        <JourneyIcon name="plus" size={17} />
      </button>
      <i aria-hidden="true" />
      <button type="button" aria-label="전체 흐름 맞춤" title="전체 흐름 맞춤" onClick={() => { void fitView(FIT_OPTIONS); }}>
        <JourneyIcon name="fit" size={17} />
      </button>
      {mobile && <><i aria-hidden="true" /><button type="button" className="j-canvas-move-toggle" aria-pressed={moveMode}
        aria-label={moveMode ? "페이지 스크롤로 전환" : "캔버스 이동 모드 켜기"} onClick={onToggleMove}>
        {moveMode ? "이동 모드 켜짐" : "캔버스 이동"}
      </button></>}
    </Panel>
  );
}

function CanvasContent({ definition, selectedId, segmentName, editable, supportedTypes, onSelect, onInsert, onConnect, onConnectionError, nodeMetrics, edgeMetrics }: JourneyCanvasProps) {
  const [mobile, setMobile] = useState(false);
  const [moveMode, setMoveMode] = useState(false);
  const [measurements, setMeasurements] = useState<Record<string, { width: number; height: number }>>({});
  const onNodesChange = useCallback<OnNodesChange<JourneyGraphNode>>((changes) => {
    const dimensions = changes.flatMap((change) => change.type === "dimensions" && change.dimensions
      ? [{ id: change.id, ...change.dimensions }] : []);
    if (!dimensions.length) return;
    // Retain actual measurements in controlled nodes. Without this feedback,
    // React Flow resets measured sizes whenever title/selection props change.
    setMeasurements((current) => {
      let next = current;
      for (const size of dimensions) {
        if (next[size.id]?.width === size.width && next[size.id]?.height === size.height) continue;
        if (next === current) next = { ...current };
        next[size.id] = { width: size.width, height: size.height };
      }
      return next;
    });
  }, []);
  useEffect(() => {
    const media = window.matchMedia("(max-width: 760px), (pointer: coarse)");
    const update = () => { setMobile(media.matches); setMoveMode(false); };
    update();
    media.addEventListener("change", update);
    return () => media.removeEventListener("change", update);
  }, []);
  const structureKey = journeyStructureKey(definition, Boolean(nodeMetrics));
  const layout = useMemo(() => layoutJourney(structureKey), [structureKey]);
  const nodes = useMemo<JourneyGraphNode[]>(() => {
    const result: JourneyGraphNode[] = [];
    const append = (id: string, data: Omit<CardData, "onSelect" | "editable">) => {
      const geometry = layout.get(id);
      if (!geometry) return;
      const { x, y, width, height } = geometry;
      result.push({
        id, type: "journeyCard", position: { x, y }, data: { ...data, onSelect, editable },
        selected: id === selectedId, draggable: false, connectable: editable,
        deletable: false, selectable: false, focusable: false,
        width, height, measured: measurements[id], style: { width, height },
      });
    };
    const triggerEntry = definition.entry.type === "trigger";
    append("entry", {
      kind: "entry", label: triggerEntry ? "이벤트 진입" : "고객 진입",
      title: triggerEntry ? definition.entry.trigger_event || "진입 이벤트 설정" : segmentName || (definition.entry.segment_id ? "선택한 세그먼트" : "대상 세그먼트 선택"),
      detail: "설정한 조건으로 고객이 저니에 진입합니다.", icon: triggerEntry ? "trigger" : "users",
      ariaLabel: "진입 조건 설정", ports: [{ id: "next", label: "시작" }],
      needsSetup: triggerEntry ? !definition.entry.trigger_event : !definition.entry.segment_id,
    });
    definition.nodes.forEach((node) => {
      const tool = NODE_TOOLS.find((item) => item.type === node.type)!;
      let title = tool.label;
      let kindLabel = tool.label;
      let detail = "";
      let needsSetup = false;
      if (node.type === "message") {
        if (node.alimtalk) {
          kindLabel = "알림톡 메시지"; // 채널에 맞춘 종류 라벨 (기본값은 푸시)
          title = node.alimtalk.template_code || "승인 템플릿 선택";
          // 발송기는 노드가 아니라 앱의 채널 배선이 정한다 — 벤더를 바꿔도 저니는 그대로다.
          detail = "알림톡 · 승인 템플릿";
          needsSetup = !node.alimtalk.sender_id?.trim() || !node.alimtalk.template_code?.trim();
        } else if (node.email) {
          kindLabel = "이메일 메시지";
          title = node.email.subject || "새 이메일 메시지";
          detail = "이메일 · " + (node.email.provider || "활성 발송기");
          needsSetup = !node.email.subject.trim() || !node.email.html.trim();
        } else {
          title = node.push?.title || "새 푸시 메시지";
          detail = node.push?.body || "메시지를 선택하고 내용을 작성해 주세요.";
          needsSetup = !node.push?.title.trim() || !node.push?.body.trim();
        }
      } else if (node.type === "delay") {
        title = formatDuration(node.duration_seconds); needsSetup = !(node.duration_seconds > 0);
      } else if (node.type === "branch") {
        const count = node.condition.groups.reduce((sum, group) => sum + group.conditions.length, 0);
        title = `${node.condition.operator === "AND" ? "모든 그룹 충족" : "하나 이상 그룹 충족"} · 조건 ${count}개`;
        needsSetup = !count || node.condition.groups.some((group) => group.conditions.some((item) => item.type === "attribute" ? !item.key.trim() : item.type === "event" ? !item.event.trim() : true));
      } else if (node.type === "event_wait") {
        title = node.event_name || "기다릴 이벤트 설정"; detail = `최대 ${formatDuration(node.timeout_seconds)}`;
        needsSetup = !node.event_name.trim() || !(node.timeout_seconds > 0);
      } else if (node.type === "ab_split") {
        title = `${node.variants.length}개 경로 · 고객별 고정 배정`;
        needsSetup = node.variants.reduce((sum, variant) => sum + variant.weight, 0) !== 100;
      }
      append(`node:${node.id}`, { kind: node.type, label: kindLabel, title, detail, icon: tool.icon,
        ariaLabel: `${tool.label}: ${title}`, ports: outputPorts(node), needsSetup, metric: nodeMetrics?.[node.id] });
    });
    append("exit", { kind: "exit", label: "저니 종료", title: "", detail: "선택한 경로를 완료하면 종료",
      icon: "flag", ariaLabel: "종료 조건 설정", ports: [] });
    return result;
  }, [definition, selectedId, segmentName, editable, onSelect, measurements, layout, nodeMetrics]);
  const layoutKey = nodes.map((node) => [node.id, node.position.x, node.position.y, node.width, node.height].join(":")).join("|");

  const edges = useMemo<JourneyGraphEdge[]>(() => {
    const makeEdge = (id: string, source: string, sourceHandle: string, target: string, label: string, pathLabel?: string): JourneyGraphEdge => ({
      id, source, sourceHandle, target, type: "journeyInsert",
      data: { editable, label, pathLabel, supportedTypes, onInsert, metric: edgeMetrics?.[id] },
      selectable: false, deletable: false, reconnectable: false, focusable: false,
    });
    return [makeEdge(entryEdgeId(definition), "entry", "next", definition.start_node_id ? `node:${definition.start_node_id}` : "exit", "고객 진입 다음"),
      ...definition.edges.map((edge) => {
        const node = definition.nodes.find((item) => item.id === edge.source);
        const ports = node ? outputPorts(node) : [];
        const port = ports.find((item) => item.id === edge.source_port);
        const label = port?.label ?? "다음";
        return makeEdge(edge.id, `node:${edge.source}`, edge.source_port, edge.target ? `node:${edge.target}` : "exit", label,
          ports.length > 1 ? label : undefined);
      })];
  }, [definition, editable, supportedTypes, onInsert, edgeMetrics]);

  const handleConnect = (connection: Connection) => {
    if (!editable || !connection.source.startsWith("node:") || !connection.sourceHandle) return;
    const source = connection.source.slice(5);
    const target = connection.target === "exit" ? null : connection.target.startsWith("node:") ? connection.target.slice(5) : undefined;
    if (target === undefined) return;
    const issue = connectionIssue(definition, source, connection.sourceHandle, target);
    if (issue) onConnectionError?.(issue);
    else onConnect?.(source, connection.sourceHandle, target);
  };

  return (
    <div className={`j-canvas${mobile && !moveMode ? " is-page-scroll" : ""}`} role="region" aria-label="저니 흐름 캔버스">
      <ReactFlow<JourneyGraphNode, JourneyGraphEdge> nodes={nodes} edges={edges} nodeTypes={nodeTypes} edgeTypes={edgeTypes}
        onNodesChange={onNodesChange} onConnect={handleConnect}
        nodesDraggable={false} nodesConnectable={editable} nodesFocusable={false} edgesFocusable={false} edgesReconnectable={false}
        elementsSelectable={false} deleteKeyCode={null} selectionKeyCode={null} minZoom={MIN_ZOOM} maxZoom={MAX_ZOOM}
        panOnDrag={!mobile || moveMode} panOnScroll={!mobile} preventScrolling={!mobile} zoomOnScroll={false}
        zoomOnPinch={!mobile || moveMode} zoomOnDoubleClick={false} colorMode="system"
        fitView fitViewOptions={FIT_OPTIONS} attributionPosition="top-right">
        <Background variant={BackgroundVariant.Dots} color="var(--j-canvas-dots, #d4dfe3)" bgColor="var(--j-canvas, #f5f8f9)" gap={20} size={1} />
        <Panel position="top-left" className="j-canvas-heading">
          <span><span className="j-canvas-heading-dot" />저니 흐름</span>
          <small>{definition.nodes.length}개 단계 · 고객당 하나의 경로</small>
        </Panel>
        <CanvasViewport layoutKey={layoutKey} structureKey={structureKey} />
        <CanvasControls mobile={mobile} moveMode={moveMode} onToggleMove={() => setMoveMode((current) => !current)} />
        <MiniMap<JourneyGraphNode> position="bottom-right" className="j-canvas-minimap" style={{ width: 128, height: 92 }}
          nodeColor={miniMapColor} nodeBorderRadius={5} nodeStrokeColor="var(--j-surface, #fff)" nodeStrokeWidth={2}
          bgColor="var(--j-surface, #fff)" maskColor="var(--j-canvas-mask)" maskStrokeColor="var(--j-canvas-mask-border)" maskStrokeWidth={1}
          pannable zoomable ariaLabel="저니 전체 보기: 드래그로 이동, 스크롤로 확대" />
      </ReactFlow>
    </div>
  );
}

export const JourneyCanvas = memo(function JourneyCanvas(props: JourneyCanvasProps) {
  return <ReactFlowProvider><CanvasContent {...props} /></ReactFlowProvider>;
});
