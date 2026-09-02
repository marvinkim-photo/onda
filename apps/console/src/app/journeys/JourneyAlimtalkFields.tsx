"use client";

import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import type { AlimtalkContent, MessageNode } from "@onda/journey-model";
import { api } from "@/lib/api";
import { useAppId } from "../use-app-id";
import {
  AD_TEMPLATE_NOTICE,
  MISSING_IN_VENDOR_NOTICE,
  isAdMessageType,
  isMissingInVendor,
  messageTypeLabel,
  templateBlockReason,
} from "../channels/alimtalk/alimtalk-labels";
import type { GraphDefinition } from "./journey-graph";
import {
  isProfileReference,
  renderTemplatePreview,
  staleVariables,
  templateVariables,
  unmappedVariables,
} from "./alimtalk-variables";
import { JourneyIcon } from "./journey-ui";

const EMPTY_ALIMTALK: AlimtalkContent = { sender_id: "", template_code: "" };

/**
 * 저니 알림톡 노드 편집.
 *
 * 본문은 편집하지 않는다 — 알림톡은 카카오 심사를 통과한 템플릿과 정확히 일치해야 하므로,
 * 승인 템플릿을 고르고 치환자만 매핑한다.
 * 발송 벤더는 노드가 아니라 앱의 채널 배선이 정한다: 벤더를 바꿔도 저니를 다시 쓰지 않아도 된다.
 */
export function AlimtalkMessageFields({
  node,
  index,
  editable,
  onUpdate,
  id,
}: {
  node: MessageNode;
  index: number;
  editable: boolean;
  onUpdate: (mutator: (definition: GraphDefinition) => void) => void;
  id: string;
}) {
  const appId = useAppId();
  const content = node.alimtalk ?? EMPTY_ALIMTALK;

  const senders = useQuery({
    queryKey: ["alimtalk-senders", appId],
    queryFn: () => api.alimtalk.senders.list(appId!),
    enabled: !!appId,
  });
  const templates = useQuery({
    queryKey: ["alimtalk-templates", appId, content.sender_id],
    queryFn: () => api.alimtalk.templates.list(appId!, { sender_id: content.sender_id }),
    enabled: !!appId && !!content.sender_id,
  });

  const senderList = senders.data?.senders ?? [];
  const templateList = templates.data?.templates ?? [];
  const template = templateList.find((t) => t.template_code === content.template_code) ?? null;
  // 저장된 템플릿 코드가 캐시에 없다 — 벤더에서 지워졌거나 발신프로필이 바뀐 저니다.
  const orphaned = !!content.template_code && !template && !templates.isPending && templateList.length > 0;
  const blockReason = template ? templateBlockReason(template.status, template.vendor_status) : null;
  const variables = templateVariables(template);
  const missing = unmappedVariables(variables, content.variables);
  const stale = staleVariables(variables, content.variables);

  function change(patch: Partial<AlimtalkContent>) {
    onUpdate((draft) => {
      const current = draft.nodes[index];
      if (current?.type !== "message") return;
      // 채널은 정확히 하나 — 알림톡을 쓰면 푸시·이메일 키를 남기지 않는다.
      draft.nodes[index] = {
        id: current.id,
        type: "message",
        alimtalk: { ...(current.alimtalk ?? EMPTY_ALIMTALK), ...patch },
      };
    });
  }

  function setVariable(name: string, value: string) {
    const next = { ...(content.variables ?? {}) };
    if (value) next[name] = value;
    else delete next[name];
    change({ variables: next });
  }

  function setFallback(patch: Partial<NonNullable<AlimtalkContent["fallback"]>> | null) {
    if (patch === null) {
      change({ fallback: undefined });
      return;
    }
    change({ fallback: { type: "SMS", text: "", ...(content.fallback ?? {}), ...patch } });
  }

  return (
    <>
      <div className="j-inspector-field">
        <div className="j-inspector-label-row">
          <label htmlFor={`${id}-alimtalk-sender`}>발신프로필</label>
        </div>
        <select
          id={`${id}-alimtalk-sender`}
          value={content.sender_id}
          disabled={!editable}
          onChange={(e) => change({ sender_id: e.currentTarget.value, template_code: "", variables: {} })}
        >
          <option value="">발신프로필 선택</option>
          {senderList.map((s) => (
            <option key={s.id} value={s.id}>
              {s.channel_name || s.sender_key}
              {s.is_default ? " (기본)" : ""}
            </option>
          ))}
        </select>
      </div>
      {!senders.isPending && senderList.length === 0 && (
        <InspectorNote>
          등록된 발신프로필이 없습니다 — &lsquo;알림톡 설정&rsquo;에서 발신프로필을 먼저 추가하세요.
        </InspectorNote>
      )}

      <div className="j-inspector-field">
        <div className="j-inspector-label-row">
          <label htmlFor={`${id}-alimtalk-template`}>승인 템플릿</label>
        </div>
        <select
          id={`${id}-alimtalk-template`}
          value={content.template_code}
          disabled={!editable || !content.sender_id}
          onChange={(e) => change({ template_code: e.currentTarget.value, variables: {} })}
        >
          <option value="">템플릿 선택</option>
          {templateList.map((t) => {
            const blocked = templateBlockReason(t.status, t.vendor_status);
            return (
              <option key={t.id} value={t.template_code} disabled={blocked !== null}>
                {t.template_code} · {t.name || "(이름 없음)"}
                {blocked ? ` — ${blocked}` : ""}
              </option>
            );
          })}
        </select>
        {template && (
          <p className="j-inspector-help">
            유형 {messageTypeLabel(template.message_type)} · 치환자 {variables.length}개
          </p>
        )}
      </div>
      {content.sender_id && !templates.isPending && templateList.length === 0 && (
        <InspectorNote>
          이 발신프로필에 캐시된 템플릿이 없습니다 — &lsquo;알림톡 설정 &gt; 승인 템플릿&rsquo;에서 동기화하세요.
        </InspectorNote>
      )}
      {orphaned && (
        <InspectorNote warning>
          이 저니가 참조하는 템플릿 <code>{content.template_code}</code>이(가) 이 발신프로필의 캐시에 없습니다.
          벤더에서 사라졌거나 다른 발신프로필의 템플릿입니다 — 이대로면 발송되지 않습니다.
        </InspectorNote>
      )}
      {template && isMissingInVendor(template.vendor_status) ? (
        <InspectorNote warning>{MISSING_IN_VENDOR_NOTICE}</InspectorNote>
      ) : (
        blockReason && (
          <InspectorNote warning>
            고른 템플릿이 승인 상태가 아닙니다 ({blockReason}) — 승인된 템플릿으로 바꾸세요.
          </InspectorNote>
        )
      )}
      {template && isAdMessageType(template.message_type) && (
        <InspectorNote warning>{AD_TEMPLATE_NOTICE}</InspectorNote>
      )}

      {template && variables.length > 0 && (
        <div className="j-inspector-field">
          <div className="j-inspector-label-row">
            <span className="j-inspector-group-label" id={`${id}-alimtalk-vars-label`}>
              변수 매핑
            </span>
          </div>
          <ul className="j-alimtalk-vars" aria-labelledby={`${id}-alimtalk-vars-label`}>
            {variables.map((name) => {
              const value = content.variables?.[name] ?? "";
              return (
                <li key={name} className={`j-alimtalk-var${value.trim() ? "" : " is-unmapped"}`}>
                  <label htmlFor={`${id}-var-${name}`}>
                    <code>{`#{${name}}`}</code>
                  </label>
                  <input
                    id={`${id}-var-${name}`}
                    value={value}
                    disabled={!editable}
                    placeholder="{{프로필속성}} 또는 고정 문구"
                    aria-invalid={!value.trim() || undefined}
                    onChange={(e) => setVariable(name, e.currentTarget.value)}
                  />
                  <span className="j-alimtalk-var-kind">
                    {!value.trim() ? "미매핑" : isProfileReference(value) ? "프로필 속성" : "고정 문구"}
                  </span>
                </li>
              );
            })}
          </ul>
          <p className="j-inspector-help">
            <code>{"{{first_name}}"}</code>처럼 쓰면 발송 시점의 프로필 값이 들어갑니다.
          </p>
        </div>
      )}

      {missing.length > 0 && (
        <InspectorNote warning>
          값이 없는 치환자: {missing.map((v) => `#{${v}}`).join(" · ")} — 알림톡은 빈 값을 허용하지 않아 발송 시
          전부 실패합니다.
        </InspectorNote>
      )}
      {stale.length > 0 && (
        <InspectorNote>
          이 템플릿이 쓰지 않는 매핑이 남아 있습니다: {stale.join(" · ")} — 발송에는 쓰이지 않습니다.
        </InspectorNote>
      )}

      {template && (
        <section className="j-inspector-preview" aria-label="알림톡 미리보기">
          <div className="j-inspector-preview-label">
            <h3>알림톡 미리보기</h3>
            <span>ALIMTALK</span>
          </div>
          <pre className="j-alimtalk-preview">{renderTemplatePreview(template.content, content.variables)}</pre>
          <p className="j-inspector-preview-caption">
            승인 본문에 매핑 값을 끼운 결과입니다. 값이 없는 치환자는 <code>{"#{이름}"}</code> 그대로 남습니다.
          </p>
        </section>
      )}

      <div className="j-inspector-field">
        <div className="j-inspector-label-row">
          <span className="j-inspector-group-label">대체발송 (선택)</span>
        </div>
        <label className="j-alimtalk-fallback-toggle">
          <input
            type="checkbox"
            checked={!!content.fallback}
            disabled={!editable}
            onChange={(e) => setFallback(e.currentTarget.checked ? {} : null)}
          />
          <span>알림톡이 실패하면 문자로 대체발송</span>
        </label>
        {content.fallback && (
          <>
            <select
              aria-label="대체발송 종류"
              value={content.fallback.type}
              disabled={!editable}
              onChange={(e) => setFallback({ type: e.currentTarget.value as "SMS" | "LMS" })}
            >
              <option value="SMS">SMS (단문)</option>
              <option value="LMS">LMS (장문)</option>
            </select>
            {content.fallback.type === "LMS" && (
              <input
                aria-label="대체발송 제목"
                value={content.fallback.title ?? ""}
                disabled={!editable}
                placeholder="LMS 제목"
                onChange={(e) => setFallback({ title: e.currentTarget.value })}
              />
            )}
            <textarea
              aria-label="대체발송 문구"
              value={content.fallback.text}
              rows={3}
              disabled={!editable}
              placeholder="알림톡을 받지 못한 고객에게 보낼 문구"
              onChange={(e) => setFallback({ text: e.currentTarget.value })}
            />
            <p className="j-inspector-help">
              벤더가 대체발송을 지원하면 벤더가 직접 처리합니다. 발신번호는 알림톡 설정의 앱 설정에서 지정합니다.
            </p>
          </>
        )}
      </div>

      <InspectorNote>
        발송 벤더는 앱의 알림톡 채널 배선이 정합니다 — 저니에서 고르지 않으므로, 벤더를 바꿔도 이 저니는 그대로
        동작합니다.
      </InspectorNote>
    </>
  );
}

/** JourneyInspector의 Note와 같은 모양 — 모듈 밖으로 내보내지 않은 컴포넌트라 여기서 다시 쓴다. */
function InspectorNote({ children, warning = false }: { children: ReactNode; warning?: boolean }) {
  return (
    <div className={`j-inspector-note${warning ? " j-inspector-note-warning" : ""}`}>
      <JourneyIcon name="info" size={16} />
      <p>{children}</p>
    </div>
  );
}
