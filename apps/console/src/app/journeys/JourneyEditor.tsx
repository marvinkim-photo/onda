"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, type JourneyValidation } from "@onda/api-client";
import { toGraphDefinition, type JourneyDefinition, type JourneyNode } from "@onda/journey-model";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useEffect, useRef, useState } from "react";
import { api } from "@/lib/api";
import { JourneyCanvas } from "./JourneyCanvas";
import { JourneyInspector } from "./JourneyInspector";
import { ChoosePathDialog, RemoveNodeDialog, RenewExperimentDialog } from "./JourneyGraphDialogs";
import { checkDraft, createJourneyNode, emptyJourney, formatDuration, NODE_TOOLS } from "./journey-editor-model";
import { connectRoute, entryEdgeId, graphReadIssue, insertOnEdge, moveLinearNode,
  outgoingEdges, pathDurationRange, renewExperiment, type GraphDefinition, type JourneyCapabilities, type PublishedABNodes } from "./journey-graph";
import { createJourneyDraftSession, type JourneyDraftInput } from "./journey-persistence";
import { JourneyIcon, JourneyState, JourneyStatus, JourneyTopbar } from "./journey-ui";

interface Props {
  appId: string;
  journeyId?: string;
  initialName?: string;
  initialDef?: JourneyDefinition;
  status?: string;
  capabilities?: JourneyCapabilities;
  publishedABNodes?: PublishedABNodes;
}

type Confirmation = { id: string } & JourneyValidation;
type Snapshot = { definition: GraphDefinition; selectedId: string; description: string };

function readableError(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 401) return "로그인이 필요합니다. 다시 로그인한 뒤 시도해 주세요.";
    if (error.status === 403) return "이 저니를 편집할 권한이 없습니다.";
    if (error.status === 409) return "저장된 내용이나 저니 상태가 변경되었거나 이름이 중복됩니다. 내용을 확인하고 다시 검증해 주세요.";
    if (error.status === 404) return "저니를 수정할 수 없습니다. 현재 상태를 다시 확인해 주세요.";
    if (error.status === 400) return "입력 내용을 확인해 주세요. 연결 정보와 필수 항목이 올바르지 않을 수 있습니다.";
    return "요청을 완료하지 못했습니다. 연결 상태를 확인한 뒤 다시 시도해 주세요.";
  }
  return error instanceof Error ? error.message : "요청을 완료하지 못했습니다. 다시 시도해 주세요.";
}

export function JourneyEditor(props: Props) {
  const [loaded] = useState(() => {
    try {
      const graph = props.initialDef ? toGraphDefinition(props.initialDef) : emptyJourney();
      return { graph, error: graphReadIssue(graph) };
    } catch (error) {
      return { graph: null, error: readableError(error) };
    }
  });
  if (!loaded.graph || loaded.error) return <JourneyState error title="저니 연결을 확인해 주세요"
    description={loaded.error ?? "저장된 흐름을 읽지 못했습니다. 원본은 변경하지 않았습니다."}
    action={<Link href="/journeys" className="j-button">목록으로</Link>} />;
  return <JourneyEditorContent {...props} initialGraph={loaded.graph} />;
}

function JourneyEditorContent({ appId, journeyId, initialName, initialGraph, status = "draft", capabilities, publishedABNodes = {} }: Props & { initialGraph: GraphDefinition }) {
  const router = useRouter();
  const queryClient = useQueryClient();
  const [name, setName] = useState(initialName ?? "");
  const [definition, setDefinition] = useState(initialGraph);
  const [selectedId, setSelectedId] = useState(() => journeyId
    ? `node:${initialGraph.nodes.find((node) => node.type === "message")?.id ?? initialGraph.start_node_id}` : "entry");
  const [session] = useState(() => createJourneyDraftSession(api.journeys, appId, journeyId));
  const [confirmation, setConfirmation] = useState<Confirmation | null>(null);
  const [savedFingerprint, setSavedFingerprint] = useState(() => journeyId ? JSON.stringify({ name: initialName ?? "", definition: initialGraph }) : "");
  const [history, setHistory] = useState<Snapshot[]>([]);
  const lastEdit = useRef({ selection: "", time: 0 });
  const [graphFeedback, setGraphFeedback] = useState<{ message: string; error: boolean } | null>(null);
  const [removeId, setRemoveId] = useState<string | null>(null);
  const [renewId, setRenewId] = useState<string | null>(null);
  const [pathChoice, setPathChoice] = useState<{ nodeId: string | null; type: JourneyNode["type"] } | null>(null);
  const dirty = JSON.stringify({ name, definition }) !== savedFingerprint;
  const statusEditable = status === "draft" || status === "paused";
  const graphSupported = capabilities?.graph_v2 === true && definition.nodes.every((node) => capabilities.supported_node_types.includes(node.type));
  const canEdit = statusEditable && graphSupported;

  const segments = useQuery({ queryKey: ["segments", appId], queryFn: () => api.segments.list(appId) });
  const recordSaved = (id: string, input: JourneyDraftInput) => {
    setName(input.name);
    setSavedFingerprint(JSON.stringify(input));
    void queryClient.invalidateQueries({ queryKey: ["journeys", appId] });
    void queryClient.invalidateQueries({ queryKey: ["journey", appId, id] });
  };
  const save = useMutation({
    mutationFn: (input: JourneyDraftInput) => { checkDraft(input.name, input.definition); return session.save(input); },
    onSuccess: (id, input) => { recordSaved(id, input); if (!journeyId) router.replace(`/journeys/${id}`); },
  });
  const validate = useMutation({
    mutationFn: (input: JourneyDraftInput) => { checkDraft(input.name, input.definition); return session.validate(input); },
    onSuccess: (result, input) => {
      recordSaved(result.id, input);
      if (!result.issues.some((issue) => issue.level === "error")) setConfirmation(result);
    },
  });
  const activate = useMutation({
    // The server checks that this exact saved revision is the one we validated.
    mutationFn: (result: Confirmation) => api.journeys.activate(appId, result.id, { revision: result.revision }),
    onSuccess: () => { void queryClient.invalidateQueries({ queryKey: ["journeys", appId] }); router.push("/journeys"); },
    onError: (error) => { if (error instanceof ApiError && error.status === 409) setConfirmation(null); },
  });
  const pause = useMutation({
    mutationFn: () => api.journeys.pause(appId, journeyId!),
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["journeys", appId] }),
        queryClient.invalidateQueries({ queryKey: ["journey", appId, journeyId] }),
      ]);
    },
  });
  const busy = save.isPending || validate.isPending || activate.isPending || pause.isPending;
  const editable = canEdit && !busy && !confirmation && !removeId && !renewId && !pathChoice;
  const validationIssues = validate.data?.issues ?? [];
  const requestError = save.error ?? validate.error ?? activate.error ?? pause.error;
  const messageCount = definition.nodes.filter((node) => node.type === "message").length;
  const duration = pathDurationRange(definition);
  const durationText = !duration ? "설정 확인" : duration.max === 0 ? "없음"
    : duration.min === duration.max ? formatDuration(duration.max)
      : `${duration.min === 0 ? "0초" : formatDuration(duration.min)} ~ ${formatDuration(duration.max)}`;

  const clearFeedback = () => { save.reset(); validate.reset(); activate.reset(); pause.reset(); setGraphFeedback(null); };
  const commitDefinition = (next: GraphDefinition, description: string, nextSelection = selectedId, fieldEdit = false) => {
    if (!canEdit || busy || confirmation) return;
    clearFeedback();
    const now = Date.now();
    if (!fieldEdit || lastEdit.current.selection !== selectedId || now - lastEdit.current.time > 800) {
      setHistory((items) => [...items.slice(-29), { definition, selectedId, description }]);
    }
    lastEdit.current = { selection: fieldEdit ? selectedId : "", time: now };
    setDefinition(next);
    setSelectedId(nextSelection);
    if (!fieldEdit) setGraphFeedback({ message: description, error: false });
  };
  const updateDefinition = (mutator: (draft: GraphDefinition) => void) => {
    if (!editable) return;
    const next = structuredClone(definition);
    mutator(next);
    commitDefinition(next, "단계 설정 변경", selectedId, true);
  };
  const applyOperation = (operation: () => GraphDefinition, description: string, nextSelection = selectedId) => {
    try { commitDefinition(operation(), description, nextSelection); }
    catch (error) { setGraphFeedback({ message: readableError(error), error: true }); }
  };
  const insertNode = (edgeId: string, type: JourneyNode["type"]) => {
    if (!canEdit || busy || confirmation || !capabilities?.supported_node_types.includes(type)) return;
    const node = createJourneyNode(type);
    setPathChoice(null);
    applyOperation(() => insertOnEdge(definition, edgeId, node), `${NODE_TOOLS.find((tool) => tool.type === type)!.label} 단계를 추가했습니다.`, `node:${node.id}`);
  };
  const paletteInsert = (type: JourneyNode["type"]) => {
    if (!editable) return;
    if (selectedId === "entry") { insertNode(entryEdgeId(definition), type); return; }
    const nodeId = selectedId === "exit" ? null : selectedId.slice(5);
    const edges = nodeId === null ? definition.edges.filter((edge) => edge.target === null) : outgoingEdges(definition, nodeId);
    if (edges.length === 1) insertNode(edges[0]!.id, type);
    else setPathChoice({ nodeId, type });
  };
  const undo = () => {
    if (!editable) return;
    const previous = history.at(-1);
    if (!previous) return;
    clearFeedback(); setDefinition(previous.definition); setSelectedId(previous.selectedId);
    setHistory((items) => items.slice(0, -1)); lastEdit.current = { selection: "", time: 0 };
    setGraphFeedback({ message: `${previous.description} 이전으로 되돌렸습니다.`, error: false });
  };
  const currentInput = (): JourneyDraftInput => ({ name: name.trim(), definition });

  return <main className="j-editor">
    <h1 className="sr-only">{journeyId ? "저니 편집" : "새 저니 만들기"}</h1>
    <JourneyTopbar current={<span className="j-editor-current">
      <label htmlFor="journey-name" className="sr-only">저니 이름</label>
      <input id="journey-name" className="j-topbar-name" value={name} maxLength={200} placeholder="새 저니 이름"
        disabled={!editable} onChange={(event) => { clearFeedback(); setName(event.target.value); }} />
      <JourneyStatus status={status} />
    </span>} actions={statusEditable ? <>
      <span className="j-save-state" role="status">{busy ? "처리 중…" : dirty ? "저장 전" : <><JourneyIcon name="check" size={14} />저장됨</>}</span>
      <button type="button" className="j-button j-undo-button" disabled={!editable || !history.length} onClick={undo}
        title="이전 편집으로 되돌리기"><JourneyIcon name="undo" size={16} /><span>되돌리기</span></button>
      <button type="button" className="j-button" disabled={!editable || !name.trim()} onClick={() => { clearFeedback(); save.mutate(currentInput()); }}>
        {save.isPending ? "저장 중…" : "임시 저장"}</button>
      <button type="button" className="j-button j-button-primary" disabled={!editable || !name.trim()} onClick={() => { clearFeedback(); validate.mutate(currentInput()); }}>
        {validate.isPending ? "검증 중…" : "검증 후 활성화"}<JourneyIcon name="arrow-right" size={16} /></button>
    </> : <><span className="j-readonly-label">읽기 전용</span>
      {journeyId && status === "active" && <button type="button" className="j-button" disabled={busy}
        onClick={() => { clearFeedback(); pause.mutate(); }}>{pause.isPending ? "일시정지 중…" : "일시정지하고 편집"}</button>}
      {journeyId && <Link href={`/journeys/${journeyId}/report`} className="j-button"><JourneyIcon name="chart" size={16} />리포트 보기</Link>}
    </>} />

    {status === "paused" && <div className="j-feedback" role="status"><JourneyIcon name="info" size={18} /><span>
      일시정지 중에도 대기 제한시간은 흐릅니다. 기존 고객은 재개 후 진입 당시의 버전으로 계속 진행합니다.
    </span></div>}

    {!graphSupported && <div className="j-feedback" role="status"><JourneyIcon name="info" size={18} /><span>
      이 서버는 그래프 저니 편집을 지원하지 않거나 지원 상태를 확인할 수 없습니다. 저장된 흐름은 읽기 전용으로 표시합니다.
    </span></div>}
    {requestError && <div className="j-feedback j-feedback-error" role="alert"><JourneyIcon name="info" size={18} /><span>{readableError(requestError)}</span>
      {requestError instanceof ApiError && requestError.status === 401 && <Link href="/login">로그인</Link>}</div>}
    {graphFeedback && <div className={`j-feedback${graphFeedback.error ? " j-feedback-error" : " j-feedback-success"}`} role={graphFeedback.error ? "alert" : "status"}>
      <JourneyIcon name={graphFeedback.error ? "info" : "check"} size={17} /><span>{graphFeedback.message}</span>
      {!graphFeedback.error && history.length > 0 && <button type="button" disabled={!editable} onClick={undo}>되돌리기</button>}
      <button type="button" className="j-feedback-dismiss" aria-label="안내 닫기" onClick={() => setGraphFeedback(null)}><JourneyIcon name="close" size={15} /></button>
    </div>}
    {validationIssues.length > 0 && <section className="j-validation" aria-label="검증 결과" role="alert"><strong>활성화 전 확인해 주세요</strong>
      <div>{validationIssues.map((issue, index) => <button type="button" key={`${issue.node_id ?? issue.node_index ?? "journey"}-${index}`}
        className={`j-validation-issue ${issue.level}`} onClick={() => {
          const node = issue.node_id ?? (issue.node_index != null ? definition.nodes[issue.node_index]?.id : undefined);
          setSelectedId(node ? `node:${node}` : "entry");
        }}><JourneyIcon name="info" size={15} />{issue.node_id && `${issue.node_id.slice(-6)} · `}{issue.message}</button>)}</div>
    </section>}

    <div className="j-editor-workspace"><aside className="j-palette" aria-label="저니 단계 도구"><div className="j-palette-main">
      <h2>단계 추가</h2><div className="j-palette-buttons">{NODE_TOOLS.map((tool) => <button key={tool.type} type="button"
        className="j-palette-add" disabled={!editable || !capabilities?.supported_node_types.includes(tool.type)} onClick={() => paletteInsert(tool.type)}>
        <span className={`j-icon-tile j-icon-${tool.type}`}><JourneyIcon name={tool.icon} /></span>
        <span>{tool.label}<small>{tool.description}</small></span><JourneyIcon name="plus" size={16} />
      </button>)}</div>
      <p className="j-palette-hint">선택한 단계 다음에 추가합니다.<br />분기에서는 경로를 선택하세요.</p>
      <button type="button" className="j-palette-setting" onClick={() => setSelectedId("entry")}><JourneyIcon name="users" size={17} />진입 조건<JourneyIcon name="arrow-right" size={14} /></button>
      <button type="button" className="j-palette-setting" onClick={() => setSelectedId("exit")}><JourneyIcon name="flag" size={17} />종료 · 재진입<JourneyIcon name="arrow-right" size={14} /></button>
    </div><div className="j-flow-summary" aria-label="저니 구성 요약">
      <div><span>메시지</span><strong>{messageCount}<small>개</small></strong></div>
      <div className="j-duration-range"><span title="고객이 한 경로를 지날 때의 설정 대기 범위입니다. 야간 제한·처리 지연은 제외합니다.">경로별 대기 ⓘ</span><strong className="j-duration-total">{durationText}</strong></div>
      <p><span />고객당 하나의 경로</p>
    </div></aside>

    <JourneyCanvas definition={definition} selectedId={selectedId} supportedTypes={capabilities?.supported_node_types}
      segmentName={segments.data?.segments.find((segment) => segment.id === definition.entry.segment_id)?.name}
      editable={editable} onSelect={setSelectedId} onInsert={insertNode}
      onConnect={(source, port, target) => applyOperation(() => connectRoute(definition, source, port, target), "다음 단계 연결을 변경했습니다.")}
      onConnectionError={(message) => setGraphFeedback({ message, error: true })} />
    <JourneyInspector key={selectedId} definition={definition} selectedId={selectedId} publishedABNodes={publishedABNodes}
      segments={segments.data?.segments ?? []} segmentsPending={segments.isPending} segmentsError={segments.isError}
      onRetrySegments={() => { void segments.refetch(); }} editable={editable} onUpdate={updateDefinition}
      onMove={(id, offset) => applyOperation(() => moveLinearNode(definition, id, offset), "단계 순서를 변경했습니다.")}
      onRemove={setRemoveId} onRenewExperiment={setRenewId}
      onConnect={(source, port, target) => applyOperation(() => connectRoute(definition, source, port, target), "다음 단계 연결을 변경했습니다.")} />
    </div>

    {pathChoice && <ChoosePathDialog definition={definition} nodeId={pathChoice.nodeId} type={pathChoice.type}
      onCancel={() => setPathChoice(null)} onChoose={(edgeId) => insertNode(edgeId, pathChoice.type)} />}
    {removeId && <RemoveNodeDialog definition={definition} nodeId={removeId} onCancel={() => setRemoveId(null)} onConfirm={(preview) => {
      setRemoveId(null); commitDefinition(preview.definition, `${preview.removed.length}개 단계를 삭제했습니다.`, "entry");
    }} />}
    {renewId && <RenewExperimentDialog onCancel={() => setRenewId(null)} onConfirm={() => {
      const next = renewExperiment(definition, renewId);
      const replacement = next.nodes.find((node, index) => node.id !== definition.nodes[index]?.id)!;
      setRenewId(null); commitDefinition(next, "새 A/B 실험을 만들었습니다. 비율을 수정한 뒤 저장해 주세요.", `node:${replacement.id}`);
    }} />}
    {confirmation && <ActivationDialog confirmation={confirmation} name={name} entryType={definition.entry.type}
      pending={activate.isPending} error={activate.error ? readableError(activate.error) : undefined}
      onCancel={() => { if (!activate.isPending) { setConfirmation(null); activate.reset(); } }}
      onConfirm={() => activate.mutate(confirmation)} />}
  </main>;
}

function ActivationDialog({ confirmation, name, entryType, pending, error, onCancel, onConfirm }: {
  confirmation: Confirmation;
  name: string;
  entryType: JourneyDefinition["entry"]["type"];
  pending: boolean;
  error?: string;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => { dialog.current?.showModal(); }, []);
  return (
    <dialog ref={dialog} className="j-activation-dialog" aria-labelledby="activation-title" aria-describedby="activation-description"
      onCancel={(event) => { event.preventDefault(); onCancel(); }}>
      <span className="j-dialog-icon"><JourneyIcon name="check" size={26} /></span>
      <h2 id="activation-title">저니를 활성화할까요?</h2>
      <p className="j-dialog-name">{name}</p>
      <div className="j-activation-audience">
        <span>{entryType === "trigger" ? "진입 방식" : "예상 대상"}</span>
        <strong>{entryType === "trigger" ? "이벤트 발생 시 시작" : confirmation.estimated_count != null
          ? `약 ${confirmation.estimated_count.toLocaleString()}명` : "대상 수를 확인할 수 없음"}</strong>
      </div>
      <p id="activation-description">활성화하면 설정한 진입 조건과 선택된 경로에 따라 실제 푸시 발송이 시작됩니다.</p>
      {confirmation.issues.filter((issue) => issue.level === "warning").map((issue, index) => (
        <p key={index} className="j-dialog-warning">{issue.message}</p>
      ))}
      {error && <p className="j-dialog-error" role="alert">{error}</p>}
      <div className="j-dialog-actions">
        <button type="button" className="j-button" autoFocus disabled={pending} onClick={onCancel}>돌아가서 확인</button>
        <button type="button" className="j-button j-button-primary" disabled={pending} onClick={onConfirm}>
          {pending ? "활성화 중…" : "저니 활성화"}
        </button>
      </div>
    </dialog>
  );
}
