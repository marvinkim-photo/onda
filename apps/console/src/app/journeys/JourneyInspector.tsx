"use client";

import { useId, useRef, useState, type ChangeEvent, type DragEvent, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { EMAIL_PROVIDER_LABELS, type EmailProvider, type SegmentSummary } from "@onda/api-client";
import type { MessageNode, DelayNode, JourneyNode } from "@onda/journey-model";
import { api } from "@/lib/api";
import { useAppId } from "../use-app-id";
import { isEmailProvider } from "../email-templates/email-provider-card";
import { ABSplitSettings, EventWaitSettings, JourneyConditionEditor, RouteSettings } from "./JourneyDecisionSettings";
import { canMoveNode, outgoingEdges, type GraphDefinition, type PublishedABNodes } from "./journey-graph";
import { DURATION_UNITS, durationUnit, formatDuration } from "./journey-editor-model";
import { JourneyIcon, type JourneyIconName } from "./journey-ui";
import { JourneyEmailTemplateSheet } from "./JourneyEmailTemplateSheet";
import { AlimtalkMessageFields } from "./JourneyAlimtalkFields";
import { withMessageChannel, type MessageChannel } from "./alimtalk-variables";
import { importEmailTemplateZip, type ImportedEmailTemplate } from "./email-template-zip";
import "./journey-inspector.css";

export interface JourneyInspectorProps {
  definition: GraphDefinition;
  selectedId: string;
  segments: SegmentSummary[];
  segmentsPending: boolean;
  segmentsError: boolean;
  onRetrySegments: () => void;
  editable: boolean;
  publishedABNodes: PublishedABNodes;
  onUpdate: (mutator: (definition: GraphDefinition) => void) => void;
  onMove: (id: string, offset: -1 | 1) => void;
  onRemove: (id: string) => void;
  onConnect: (source: string, port: string, target: string | null) => void;
  onRenewExperiment: (id: string) => void;
}

type UpdateDefinition = JourneyInspectorProps["onUpdate"];
type InspectorKind = "entry" | "exit" | JourneyNode["type"];

const inspectorContent: Record<InspectorKind, {
  title: string; icon: JourneyIconName; description: string; helper: string;
}> = {
  entry: {
    title: "진입 조건", icon: "users", description: "누가, 언제 이 저니를 시작할까요?",
    helper: "진입한 고객은 캔버스의 순서에 따라 각 단계를 거칩니다.",
  },
  exit: {
    title: "종료와 재진입", icon: "flag", description: "목표를 달성한 다음의 흐름을 정하세요.",
    helper: "종료 이벤트가 없으면 마지막 단계까지 진행한 뒤 저니가 완료됩니다.",
  },
  message: {
    title: "메시지", icon: "message", description: "고객에게 전할 이야기를 작성하세요.",
    helper: "캔버스에서 다른 단계를 선택하면 해당 설정을 확인할 수 있습니다.",
  },
  delay: {
    title: "시간 대기", icon: "clock", description: "다음 단계까지 여유를 두세요.",
    helper: "설정한 시간이 지나면 다음 단계로 이어집니다.",
  },
  branch: {
    title: "조건 분기", icon: "branch", description: "고객 조건에 따라 다음 경로를 나누세요.",
    helper: "조건을 충족하는 고객과 충족하지 않는 고객은 각각 하나의 경로로 진행합니다.",
  },
  event_wait: {
    title: "이벤트 대기", icon: "event-wait", description: "고객의 다음 행동을 기다리세요.",
    helper: "이벤트가 접수되면 발생 경로로, 제한 시간이 지나면 시간 초과 경로로 이동합니다.",
  },
  ab_split: {
    title: "A/B 분기", icon: "split", description: "고객을 비율에 따라 나누어 비교하세요.",
    helper: "한 고객은 하나의 경로로만 진행합니다. 진행 중인 버전의 배정은 유지됩니다.",
  },
};

export function JourneyInspector({
  definition, selectedId, segments, segmentsPending, segmentsError, publishedABNodes,
  onRetrySegments, editable, onUpdate, onMove, onRemove, onConnect, onRenewExperiment,
}: JourneyInspectorProps) {
  const fieldId = useId();
  const index = definition.nodes.findIndex((node) => `node:${node.id}` === selectedId);
  const node = definition.nodes[index];
  const kind: InspectorKind | undefined = selectedId === "entry" ? "entry"
    : selectedId === "exit" ? "exit" : node?.type;

  if (!kind) {
    return (
      <aside className="j-inspector j-inspector-empty" aria-label="단계 설정">
        <span className="j-inspector-icon"><JourneyIcon name="info" size={22} /></span>
        <h2>단계를 선택하세요</h2>
        <p>캔버스에서 진입 조건이나 단계를 선택하면 설정이 여기에 표시됩니다.</p>
      </aside>
    );
  }

  const content = inspectorContent[kind];
  const icon = kind === "entry" && definition.entry.type === "trigger" ? "trigger" : content.icon;
  const update: UpdateDefinition = (mutator) => { if (editable) onUpdate(mutator); };

  return (
    <aside className={`j-inspector j-inspector-${kind}`} aria-labelledby={`${fieldId}-heading`}>
      <header className="j-inspector-header">
        <div className="j-inspector-heading">
          <span className="j-inspector-icon"><JourneyIcon name={icon} size={23} /></span>
          <div>
            <p className="j-inspector-eyebrow">
              {index >= 0 ? `${index + 1}번째 단계` : kind === "entry" ? "저니의 시작" : "저니의 마무리"}
            </p>
            <h2 id={`${fieldId}-heading`}>{content.title}</h2>
          </div>
          {!editable && <span className="j-inspector-readonly">읽기 전용</span>}
        </div>
        <p className="j-inspector-description">{content.description}</p>
        {index >= 0 && (
          <div className="j-inspector-actions">
            <span>단계 관리</span>
            <div className="j-inspector-action-group" role="group" aria-label="선택한 단계 관리">
              <button type="button" aria-label="단계를 위로 이동" title="위로 이동"
                disabled={!editable || !node || !canMoveNode(definition, node.id, -1)} onClick={() => node && onMove(node.id, -1)}>
                <JourneyIcon name="up" size={16} />
              </button>
              <button type="button" aria-label="단계를 아래로 이동" title="아래로 이동"
                disabled={!editable || !node || !canMoveNode(definition, node.id, 1)} onClick={() => node && onMove(node.id, 1)}>
                <JourneyIcon name="down" size={16} />
              </button>
              <span className="j-inspector-action-divider" />
              <button type="button" className="j-inspector-remove" aria-label="단계 삭제"
                title={definition.nodes.length <= 1 ? "최소 1개의 단계가 필요합니다" : "단계 삭제"}
                disabled={!editable || definition.nodes.length <= 1} onClick={() => node && onRemove(node.id)}>
                <JourneyIcon name="trash" size={16} />
              </button>
            </div>
          </div>
        )}
      </header>

      <div className="j-inspector-content">
        {kind === "entry" && (
          <EntrySettings definition={definition} segments={segments} pending={segmentsPending}
            error={segmentsError} onRetry={onRetrySegments} editable={editable} onUpdate={update} id={fieldId} />
        )}
        {kind === "exit" && (
          <ExitSettings definition={definition} editable={editable} onUpdate={update} id={fieldId} />
        )}
        {node?.type === "message" && (
          <MessageSettings node={node} index={index} editable={editable} onUpdate={update} id={fieldId} />
        )}
        {node?.type === "delay" && (
          <DelaySettings node={node} index={index} last={outgoingEdges(definition, node.id).every((edge) => edge.target === null)}
            editable={editable} onUpdate={update} id={fieldId} />
        )}
        {node?.type === "branch" && <JourneyConditionEditor value={node.condition} editable={editable}
          onChange={(condition) => update((draft) => {
            const current = draft.nodes.find((item) => `node:${item.id}` === selectedId);
            if (current?.type === "branch") current.condition = condition;
          })} />}
        {node?.type === "event_wait" && <EventWaitSettings node={node} editable={editable} onUpdate={update} id={fieldId} />}
        {node?.type === "ab_split" && <ABSplitSettings node={node} definition={definition} editable={editable}
          locked={Object.hasOwn(publishedABNodes, node.id)} onUpdate={update} id={fieldId} onRenew={() => onRenewExperiment(node.id)} />}
        {node && <RouteSettings node={node} definition={definition} editable={editable} onConnect={onConnect} />}
      </div>

      <footer className="j-inspector-footer">
        <JourneyIcon name="info" size={16} /><p>{content.helper}</p>
      </footer>
    </aside>
  );
}

function Field({ id, label, detail, children }: {
  id: string; label: string; detail?: ReactNode; children: ReactNode;
}) {
  return (
    <div className="j-inspector-field">
      <div className="j-inspector-label-row"><label htmlFor={id}>{label}</label>{detail}</div>
      {children}
    </div>
  );
}

function Note({ children, warning = false }: { children: ReactNode; warning?: boolean }) {
  return (
    <div className={`j-inspector-note${warning ? " j-inspector-note-warning" : ""}`}>
      <JourneyIcon name="info" size={16} /><p>{children}</p>
    </div>
  );
}

function EntrySettings({ definition, segments, pending, error, onRetry, editable, onUpdate, id }: {
  definition: GraphDefinition; segments: SegmentSummary[]; pending: boolean; error: boolean;
  onRetry: () => void; editable: boolean; onUpdate: UpdateDefinition; id: string;
}) {
  const entry = definition.entry;
  const activeSegments = segments.filter((segment) => segment.status === "active");
  const selectedSegment = segments.find((segment) => segment.id === entry.segment_id);
  const unavailable = Boolean(entry.segment_id && !activeSegments.some((segment) => segment.id === entry.segment_id));

  return (
    <>
      <Field id={`${id}-entry-type`} label="시작 방식">
        <select id={`${id}-entry-type`} value={entry.type} disabled={!editable}
          onChange={(event) => {
            const type = event.currentTarget.value as "blast" | "trigger";
            onUpdate((draft) => { draft.entry = { ...draft.entry, type }; });
          }}>
          <option value="blast">세그먼트 일괄 진입</option>
          <option value="trigger">이벤트 발생 시 진입</option>
        </select>
      </Field>

      {entry.type === "blast" ? (
        <Field id={`${id}-segment`} label="대상 세그먼트">
          <select id={`${id}-segment`} value={entry.segment_id ?? ""}
            disabled={!editable || pending || error} aria-describedby={`${id}-segment-help`}
            onChange={(event) => {
              const segmentId = event.currentTarget.value;
              onUpdate((draft) => { draft.entry = { ...draft.entry, segment_id: segmentId || undefined }; });
            }}>
            <option value="">{pending ? "세그먼트 불러오는 중…" : "세그먼트를 선택하세요"}</option>
            {unavailable && <option value={entry.segment_id} disabled>
              {selectedSegment?.name ?? "저장된 세그먼트"}{!pending && !error ? " · 사용 불가" : ""}
            </option>}
            {activeSegments.map((segment) => <option key={segment.id} value={segment.id}>{segment.name}</option>)}
          </select>
          <div id={`${id}-segment-help`} className="j-inspector-help">
            {error ? (
              <p className="j-inspector-error">세그먼트를 불러오지 못했습니다. <button type="button"
                className="j-inspector-retry" onClick={onRetry}>다시 불러오기</button></p>
            ) : pending ? <p>저장된 선택은 그대로 유지됩니다.</p>
              : unavailable ? <p className="j-inspector-error">현재 사용할 수 없는 세그먼트입니다. 다른 대상을 선택해 주세요.</p>
                : activeSegments.length === 0 ? <p>사용 가능한 세그먼트가 없습니다. 먼저 대상 세그먼트를 만들어 주세요.</p>
                  : <p>선택한 세그먼트의 고객이 함께 저니를 시작합니다.</p>}
          </div>
          {selectedSegment?.status === "active" && selectedSegment.last_count != null && (
            <div className="j-inspector-audience">
              <JourneyIcon name="users" size={17} /><span>최근 집계</span>
              <strong>{selectedSegment.last_count.toLocaleString()}<small>명</small></strong>
            </div>
          )}
        </Field>
      ) : (
        <>
          <Field id={`${id}-trigger-event`} label="진입 이벤트">
            <input id={`${id}-trigger-event`} value={entry.trigger_event ?? ""} disabled={!editable}
              autoComplete="off" spellCheck={false} placeholder="예: product_viewed"
              aria-describedby={`${id}-trigger-help`} onChange={(event) => {
                const triggerEvent = event.currentTarget.value;
                onUpdate((draft) => { draft.entry = { ...draft.entry, trigger_event: triggerEvent || undefined }; });
              }} />
            <p id={`${id}-trigger-help`} className="j-inspector-help">앱에서 수집하는 이벤트 이름을 정확히 입력하세요.</p>
          </Field>
          {entry.segment_id && (
            <Note warning>기존 세그먼트 필터는 보존됩니다. 트리거 진입의 세그먼트 필터는 현재 지원되지 않습니다.</Note>
          )}
        </>
      )}

      <div className="j-inspector-divider" />
      <Field id={`${id}-category`} label="메시지 카테고리">
        <select id={`${id}-category`} value={definition.settings.category} disabled={!editable}
          aria-describedby={`${id}-category-help`} onChange={(event) => {
            const category = event.currentTarget.value as "marketing" | "transactional";
            onUpdate((draft) => { draft.settings = { ...draft.settings, category }; });
          }}>
          <option value="marketing">마케팅</option>
          <option value="transactional">거래성</option>
        </select>
        <p id={`${id}-category-help`} className="j-inspector-help">
          {definition.settings.category === "marketing"
            ? "수신 동의와 야간·빈도 제한을 적용합니다."
            : "거래성은 수신 동의·야간·빈도 제한을 적용하지 않습니다. 메시지 목적에 맞게 선택하세요."}
        </p>
      </Field>
    </>
  );
}

function reentryMode(value: unknown): "never" | "always" | "after_days" | "existing" {
  if (value === "never" || value === "always") return value;
  return value !== null && typeof value === "object" && "after_days" in value ? "after_days" : "existing";
}

function ExitSettings({ definition, editable, onUpdate, id }: {
  definition: GraphDefinition; editable: boolean; onUpdate: UpdateDefinition; id: string;
}) {
  const reentry = definition.settings.reentry;
  const mode = reentryMode(reentry);
  const days = typeof reentry === "object" && reentry !== null ? reentry.after_days : Number.NaN;
  const validDays = Number.isSafeInteger(days) && days > 0 && days <= 106_751;
  const supportsReentry = definition.entry.type === "trigger";
  const reentryEditable = editable && supportsReentry;

  return (
    <>
      <Field id={`${id}-conversion-event`} label="목표 달성 이벤트"
        detail={<span className="j-inspector-optional">선택</span>}>
        <input id={`${id}-conversion-event`} value={definition.exit?.conversion_event ?? ""}
          disabled={!editable} autoComplete="off" spellCheck={false} placeholder="예: purchase_completed"
          aria-describedby={`${id}-conversion-help`} onChange={(event) => {
            const conversionEvent = event.currentTarget.value;
            onUpdate((draft) => { draft.exit = { ...draft.exit, conversion_event: conversionEvent || undefined }; });
          }} />
        <p id={`${id}-conversion-help`} className="j-inspector-help">이 이벤트가 발생하면 진행 중인 저니에서 이탈합니다. 비워두면 마지막 단계까지 진행합니다.</p>
      </Field>
      <div className="j-inspector-divider" />
      <Field id={`${id}-reentry`} label="재진입 허용">
        <select id={`${id}-reentry`} value={mode} disabled={!reentryEditable}
          aria-describedby={`${id}-reentry-help`} onChange={(event) => {
            if (!reentryEditable) return;
            const value = event.currentTarget.value;
            if (value !== "never" && value !== "always" && value !== "after_days") return;
            onUpdate((draft) => {
              const current = draft.settings.reentry;
              draft.settings = { ...draft.settings, reentry: value === "after_days"
                ? { ...(typeof current === "object" && current !== null ? current : {}),
                  after_days: typeof current === "object" && current !== null && "after_days" in current
                    ? current.after_days : 1 }
                : value };
            });
          }}>
          {mode === "existing" && <option value="existing" disabled>기존 설정 유지</option>}
          <option value="never">다시 진입하지 않음</option>
          <option value="always">항상 허용</option>
          <option value="after_days">일정 기간 후 허용</option>
        </select>
        <p id={`${id}-reentry-help`} className="j-inspector-help">
          {supportsReentry ? "같은 고객이 이 저니에 다시 들어올 수 있는 조건입니다."
            : "재진입 정책은 이벤트 발생으로 시작하는 저니에만 적용됩니다."}
        </p>
      </Field>
      {mode === "after_days" && (
        <Field id={`${id}-reentry-days`} label="재진입 대기 기간">
          <div className="j-inspector-input-suffix">
            <input id={`${id}-reentry-days`} type="number" min={1} max={106_751} step={1}
              value={Number.isFinite(days) ? days : ""} disabled={!reentryEditable}
              aria-invalid={(supportsReentry && !validDays) || undefined} aria-describedby={`${id}-reentry-days-help`}
              onChange={(event) => {
                if (!reentryEditable) return;
                const afterDays = event.currentTarget.valueAsNumber;
                onUpdate((draft) => {
                  const current = draft.settings.reentry;
                  draft.settings = { ...draft.settings, reentry: {
                    ...(typeof current === "object" && current !== null ? current : {}), after_days: afterDays,
                  } };
                });
              }} />
            <span>일 후</span>
          </div>
          <p id={`${id}-reentry-days-help`} className={`j-inspector-help${supportsReentry && !validDays ? " j-inspector-error" : ""}`}>
            {!supportsReentry ? "저장된 값은 유지되며, 이벤트 발생 방식으로 바꾸면 수정할 수 있습니다."
              : validDays ? "지정한 기간이 지난 뒤 재진입할 수 있습니다." : "1~106751일 사이의 정수로 입력해 주세요."}
          </p>
        </Field>
      )}
      <div className="j-inspector-completion"><span><JourneyIcon name="check" size={20} /></span>
        <div><strong>자연스럽게 마무리</strong><p>마지막 단계를 마치면 저니가 완료됩니다.</p></div>
      </div>
    </>
  );
}

const CHANNEL_LABELS: Record<MessageChannel, string> = { push: "푸시", email: "이메일", alimtalk: "알림톡" };

function MessageSettings({ node, index, editable, onUpdate, id }: {
  node: MessageNode; index: number; editable: boolean; onUpdate: UpdateDefinition; id: string;
}) {
  // 채널은 정확히 하나. 셋 다 비어 있는 새 노드는 푸시로 읽는다.
  const channel: MessageChannel = node.alimtalk ? "alimtalk" : node.email ? "email" : "push";

  function setChannel(next: MessageChannel) {
    if (next === channel) return;
    onUpdate((draft) => {
      const current = draft.nodes[index];
      if (current?.type !== "message") return;
      // 다른 채널의 키를 남기면 messageChannel이 null이 되고 발행 검증이 막힌다.
      draft.nodes[index] = { ...withMessageChannel({ id: current.id, type: "message" }, next), id: current.id };
    });
  }

  return (
    <>
      <Field id={`${id}-channel`} label="채널">
        <div className="j-inspector-segmented j-inspector-segmented-3" role="group" aria-label="발송 채널">
          {(["push", "email", "alimtalk"] as const).map((option) => (
            <button key={option} type="button" disabled={!editable} aria-pressed={channel === option}
              className={channel === option ? "is-active" : undefined} onClick={() => setChannel(option)}>
              {CHANNEL_LABELS[option]}
            </button>
          ))}
        </div>
      </Field>
      {channel === "alimtalk"
        ? <AlimtalkMessageFields node={node} index={index} editable={editable} onUpdate={onUpdate} id={id} />
        : channel === "email"
          ? <EmailMessageFields node={node} index={index} editable={editable} onUpdate={onUpdate} id={id} />
          : <PushMessageFields node={node} index={index} editable={editable} onUpdate={onUpdate} id={id} />}
    </>
  );
}

function PushMessageFields({ node, index, editable, onUpdate, id }: {
  node: MessageNode; index: number; editable: boolean; onUpdate: UpdateDefinition; id: string;
}) {
  const title = node.push?.title ?? "";
  const body = node.push?.body ?? "";
  function change(field: "title" | "body", value: string) {
    onUpdate((draft) => {
      const current = draft.nodes[index];
      if (current?.type === "message") {
        // 다른 채널의 키를 남기지 않는다 — 메시지 노드는 정확히 하나의 채널만 채워야 한다.
        draft.nodes[index] = {
          ...current, push: { ...(current.push ?? { title: "", body: "" }), [field]: value },
          email: undefined, alimtalk: undefined,
        };
      }
    });
  }

  return (
    <>
      <Field id={`${id}-push-title`} label="메시지 제목"
        detail={<span id={`${id}-title-count`} className="j-inspector-counter">{title.length}/256</span>}>
        <input id={`${id}-push-title`} value={title} maxLength={256} disabled={!editable}
          placeholder="반가운 첫인사를 전해 보세요" aria-describedby={`${id}-title-count`}
          aria-invalid={title.length > 256 || undefined} onChange={(event) => change("title", event.currentTarget.value)} />
      </Field>
      <Field id={`${id}-push-body`} label="메시지 내용"
        detail={<span id={`${id}-body-count`} className="j-inspector-counter">{body.length}/2,048</span>}>
        <textarea id={`${id}-push-body`} value={body} maxLength={2048} rows={4} disabled={!editable}
          placeholder="고객에게 전하고 싶은 내용을 입력하세요." aria-describedby={`${id}-body-count ${id}-variable-help`}
          aria-invalid={body.length > 2048 || undefined} onChange={(event) => change("body", event.currentTarget.value)} />
        <p id={`${id}-variable-help`} className="j-inspector-help j-inspector-variable-help">
          <code>{"{{first_name}}"}</code><span>제목·본문에 이름 변수 사용</span>
        </p>
      </Field>
      <section className="j-inspector-preview" aria-label="푸시 알림 표시 예시">
        <div className="j-inspector-preview-label"><h3>알림 미리보기</h3><span>PUSH</span></div>
        <div className="j-inspector-phone">
          <div className="j-inspector-phone-status" aria-hidden="true"><span>9:41</span><span className="j-inspector-phone-signal"><i /><i /><i /><b /></span></div>
          <div className="j-inspector-phone-clock" aria-hidden="true">9:41</div>
          <div className="j-inspector-notification">
            <div className="j-inspector-notification-heading">
              <span className="j-inspector-notification-app"><JourneyIcon name="wave" size={15} /></span>
              <span>Onda</span><small>지금</small>
            </div>
            <strong className={!title ? "j-inspector-preview-placeholder" : undefined}>{title || "메시지 제목"}</strong>
            <p className={!body ? "j-inspector-preview-placeholder" : undefined}>{body || "작성한 메시지가 여기에 표시됩니다."}</p>
          </div>
          <span className="j-inspector-phone-home" aria-hidden="true" />
        </div>
        <p className="j-inspector-preview-caption">표시 예시 · 실제 기기에 따라 다를 수 있습니다.</p>
      </section>
      {(node.push?.image_url || node.push?.deep_link) && (
        <Note>기존 이미지 URL·딥링크는 보존됩니다. 현재 저니 발송에는 제목과 본문만 전달됩니다.</Note>
      )}
    </>
  );
}

/** 이메일 노드 편집 — 제목·HTML {{ }} 개인화 + 발송기(provider) 선택(검증된 것만). */
function EmailMessageFields({ node, index, editable, onUpdate, id }: {
  node: MessageNode; index: number; editable: boolean; onUpdate: UpdateDefinition; id: string;
}) {
  const appId = useAppId();
  const subject = node.email?.subject ?? "";
  const html = node.email?.html ?? "";
  const provider = node.email?.provider ?? "";
  const fileInput = useRef<HTMLInputElement>(null);
  const previewButton = useRef<HTMLButtonElement>(null);
  const importGeneration = useRef(0);
  const [sourceMode, setSourceMode] = useState<"html" | "zip">("html");
  const [importedTemplate, setImportedTemplate] = useState<ImportedEmailTemplate | null>(null);
  const [sheetOpen, setSheetOpen] = useState(false);
  const [importing, setImporting] = useState(false);
  const [importError, setImportError] = useState<string | null>(null);
  const creds = useQuery({
    queryKey: ["credentials", appId],
    queryFn: () => api.credentials.list(appId!),
    enabled: !!appId,
  });
  const verified = (creds.data?.credentials ?? []).flatMap((c) =>
    isEmailProvider(c.kind) && c.status === "verified" ? [c.kind] : [],
  );

  function change(patch: Partial<{ subject: string; html: string; provider: EmailProvider | undefined }>) {
    onUpdate((draft) => {
      const current = draft.nodes[index];
      if (current?.type === "message") {
        const base = current.email ?? { subject: "", html: "" };
        draft.nodes[index] = { ...current, email: { ...base, ...patch }, push: undefined, alimtalk: undefined };
      }
    });
  }

  async function readZip(file: File | undefined) {
    if (!file || !editable) return;
    const generation = ++importGeneration.current;
    setSourceMode("zip");
    setSheetOpen(false);
    setImporting(true);
    setImportError(null);
    try {
      const imported = await importEmailTemplateZip(file);
      if (generation !== importGeneration.current) return;
      setImportedTemplate(imported);
      setSheetOpen(true);
    } catch (error) {
      if (generation !== importGeneration.current) return;
      setImportedTemplate(null);
      setSheetOpen(false);
      setImportError(error instanceof Error ? error.message : "ZIP 템플릿을 불러오지 못했습니다.");
    } finally {
      if (generation === importGeneration.current) {
        setImporting(false);
        if (fileInput.current) fileInput.current.value = "";
      }
    }
  }

  function onFileChange(event: ChangeEvent<HTMLInputElement>) {
    void readZip(event.currentTarget.files?.[0]);
  }

  function onDrop(event: DragEvent<HTMLDivElement>) {
    event.preventDefault();
    void readZip(event.dataTransfer.files?.[0]);
  }

  function chooseZip() {
    if (editable && !importing) fileInput.current?.click();
  }

  function closeSheet() {
    setSheetOpen(false);
    window.requestAnimationFrame(() => previewButton.current?.focus());
  }

  function chooseAnotherZip() {
    setSheetOpen(false);
    window.requestAnimationFrame(() => fileInput.current?.click());
  }

  const templateApplied = Boolean(importedTemplate && importedTemplate.html === html);

  return (
    <>
      <Field id={`${id}-email-subject`} label="이메일 제목 ({{변수}} 가능)">
        <input id={`${id}-email-subject`} value={subject} maxLength={998} disabled={!editable}
          placeholder="{{first_name}}님, 안녕하세요" onChange={(e) => change({ subject: e.currentTarget.value })} />
      </Field>
      <div className="j-inspector-field">
        <div className="j-inspector-label-row">
          <span className="j-inspector-group-label" id={`${id}-email-source-label`}>작성 방식</span>
        </div>
        <div className="j-inspector-segmented j-email-source-switch" role="group" aria-labelledby={`${id}-email-source-label`}>
          <button type="button" disabled={!editable} aria-pressed={sourceMode === "html"}
            className={sourceMode === "html" ? "is-active" : undefined} onClick={() => setSourceMode("html")}>HTML 직접 작성</button>
          <button type="button" disabled={!editable} aria-pressed={sourceMode === "zip"}
            className={sourceMode === "zip" ? "is-active" : undefined} onClick={() => {
              setSourceMode("zip");
              if (importedTemplate) setSheetOpen(true);
            }}>ZIP 템플릿</button>
        </div>
      </div>
      {sourceMode === "html" ? (
        <Field id={`${id}-email-html`} label="HTML 본문 ({{변수}} 가능)">
          <textarea id={`${id}-email-html`} value={html} rows={8} disabled={!editable}
            className="j-inspector-code" placeholder="<h1>안녕하세요 {{first_name}}님</h1>"
            onChange={(e) => change({ html: e.currentTarget.value })} />
          {importedTemplate && <p className="j-inspector-help">가져온 템플릿도 여기에서 계속 수정할 수 있습니다.</p>}
        </Field>
      ) : (
        <div className="j-template-import-field">
          <input ref={fileInput} id={`${id}-email-zip`} className="j-template-file-input" type="file" tabIndex={-1} hidden
            accept=".zip,application/zip" disabled={!editable || importing} onChange={onFileChange} />
          {importedTemplate ? (
            <div className="j-template-imported-summary">
              <span className="j-template-imported-icon"><JourneyIcon name="check" size={17} /></span>
              <div><strong>{importedTemplate.archiveName}</strong><p>
                {templateApplied ? "현재 HTML에 적용됨" : "적용 전"} · 파일 {importedTemplate.fileCount}개
              </p></div>
              <button ref={previewButton} type="button" disabled={!editable} onClick={() => setSheetOpen(true)}>미리보기</button>
            </div>
          ) : (
            <div className={`j-template-dropzone${importError ? " has-error" : ""}`} onDragOver={(event) => event.preventDefault()} onDrop={onDrop}>
              <span><JourneyIcon name="message" size={20} /></span>
              <strong>{importing ? "ZIP 압축을 확인하는 중…" : "ZIP 템플릿 가져오기"}</strong>
              <p>{importing ? "파일 수와 크기, HTML 안전성을 검사합니다." : "index.html과 이미지·CSS를 하나의 ZIP으로 올려 주세요."}</p>
              <button type="button" className="j-button" disabled={!editable || importing} onClick={chooseZip}>
                {importing ? "불러오는 중…" : "ZIP 파일 선택"}
              </button>
            </div>
          )}
          {importError && <p className="j-template-import-error" role="alert">{importError}</p>}
          {importedTemplate && <button type="button" className="j-template-change-file" disabled={!editable || importing} onClick={chooseZip}>
            다른 ZIP 파일 선택
          </button>}
          <p className="j-inspector-help">적용 전 별도 화면에서 데스크톱·모바일 미리보기를 확인합니다.</p>
        </div>
      )}
      <Field id={`${id}-email-provider`} label="발송기">
        <select id={`${id}-email-provider`} value={provider} disabled={!editable}
          onChange={(e) => change({ provider: (e.currentTarget.value || undefined) as EmailProvider | undefined })}>
          <option value="">자동(활성 발송기)</option>
          {verified.map((kind) => <option key={kind} value={kind}>{EMAIL_PROVIDER_LABELS[kind]}</option>)}
        </select>
      </Field>
      {verified.length === 0 && (
        <Note>검증된 이메일 발송기가 없습니다 — &lsquo;이메일 템플릿 &gt; 이메일 발송기&rsquo;에서 먼저 등록·검증하세요.</Note>
      )}
      {sheetOpen && importedTemplate && (
        <JourneyEmailTemplateSheet template={importedTemplate} onCancel={closeSheet}
          onChooseAnother={chooseAnotherZip} onApply={() => {
            change({ html: importedTemplate.html });
            closeSheet();
          }} />
      )}
    </>
  );
}

function DelaySettings({ node, index, last, editable, onUpdate, id }: {
  node: DelayNode; index: number; last: boolean; editable: boolean; onUpdate: UpdateDefinition; id: string;
}) {
  // The parent keys the inspector by selectedId, so each selected delay chooses an exact unit once.
  const [unit, setUnit] = useState(() => durationUnit(node.duration_seconds));
  const amount = node.duration_seconds / unit;
  const valid = Number.isSafeInteger(node.duration_seconds) && node.duration_seconds > 0;
  function setSeconds(seconds: number) {
    onUpdate((draft) => {
      const current = draft.nodes[index];
      if (current?.type === "delay") draft.nodes[index] = { ...current, duration_seconds: seconds };
    });
  }

  return (
    <>
      <div className="j-inspector-duration-fields">
        <Field id={`${id}-duration`} label="대기 시간">
          <input id={`${id}-duration`} type="number" min={0} step="any" disabled={!editable}
            value={Number.isFinite(amount) ? amount : ""} placeholder="숫자 입력"
            aria-invalid={!valid || undefined} aria-describedby={`${id}-duration-help`}
            onChange={(event) => setSeconds(event.currentTarget.valueAsNumber * unit)} />
        </Field>
        <Field id={`${id}-duration-unit`} label="단위">
          <select id={`${id}-duration-unit`} value={unit} disabled={!editable}
            onChange={(event) => {
              const nextUnit = Number(event.currentTarget.value);
              setUnit(nextUnit);
              setSeconds(amount * nextUnit);
            }}>
            {DURATION_UNITS.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
          </select>
        </Field>
      </div>
      <p id={`${id}-duration-help`} className={`j-inspector-help${!valid ? " j-inspector-error" : ""}`}>
        {valid ? "고정한 시간만큼 기다린 뒤 다음 단계로 이동합니다." : "대기 시간은 1초 이상의 정수가 되도록 입력해 주세요."}
      </p>
      <div className={`j-inspector-duration-summary${!valid ? " j-inspector-duration-invalid" : ""}`}>
        <span className="j-inspector-duration-summary-icon"><JourneyIcon name="clock" size={26} /></span>
        <span>이 단계에서 기다리는 시간</span>
        <strong>{valid ? formatDuration(node.duration_seconds) : "시간을 확인해 주세요"}</strong>
        {valid && <small>총 {node.duration_seconds.toLocaleString()}초</small>}
      </div>
      {last && <Note warning>마지막 단계가 대기입니다. 대기 시간이 지나면 추가 발송 없이 저니가 종료됩니다.</Note>}
    </>
  );
}
