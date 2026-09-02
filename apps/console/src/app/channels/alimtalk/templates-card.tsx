"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import { useState } from "react";
import type { AlimtalkSender, AlimtalkTemplate } from "@onda/api-client";
import { api } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  AD_TEMPLATE_NOTICE,
  MISSING_IN_VENDOR_NOTICE,
  isAdMessageType,
  isMissingInVendor,
  messageTypeLabel,
  serverMessage,
  templateStateLabel,
} from "./alimtalk-labels";

/** 동기화는 워커가 비동기로 한다 — 202 직후에는 아직 목록이 그대로다. */
const SYNC_REFETCH_DELAY_MS = 4000;

/**
 * 승인 템플릿 캐시. Onda는 템플릿을 편집하지 않는다 —
 * 알림톡 본문은 카카오 심사를 통과한 것과 정확히 일치해야 하므로 벤더에서 읽어 캐시만 한다.
 */
export function TemplatesCard({
  appId,
  senders,
  onSynced,
}: {
  appId: string | undefined;
  senders: AlimtalkSender[];
  onSynced?: () => void;
}) {
  const [senderId, setSenderId] = useState<string>("");
  const [msg, setMsg] = useState<string | null>(null);
  const selected = senderId || senders.find((s) => s.is_default)?.id || senders[0]?.id || "";

  const templates = useQuery({
    queryKey: ["alimtalk-templates", appId, selected],
    queryFn: () => api.alimtalk.templates.list(appId!, selected ? { sender_id: selected } : undefined),
    enabled: !!appId,
  });

  const sync = useMutation({
    mutationFn: () => {
      if (!appId) throw new Error("앱을 찾을 수 없습니다");
      // 보고 있는 발신프로필만 동기화한다 — 미지정이면 서버가 기본 발신프로필을 고른다.
      return api.alimtalk.templates.sync(appId, selected ? { sender_id: selected } : undefined);
    },
    onSuccess: () => {
      setMsg("동기화 요청됨 — 수 초 후 목록을 새로고침하세요.");
      onSynced?.();
      setTimeout(() => void templates.refetch(), SYNC_REFETCH_DELAY_MS);
    },
    // 400은 서버가 무엇이 없는지 한국어로 지목한다(발신프로필·배선·검증된 크리덴셜).
    // 아무것도 못 하는 상태에서 202를 주지 않으려고 일부러 막아둔 것이라 문구를 그대로 보여 준다.
    onError: (e) => setMsg(serverMessage(e, "동기화 요청에 실패했습니다.")),
  });

  const rows = templates.data?.templates ?? [];

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between gap-2 p-4">
        <CardTitle className="text-sm">승인 템플릿</CardTitle>
        <Button
          className="h-7 shrink-0 px-2 text-xs"
          disabled={!appId || sync.isPending}
          onClick={() => sync.mutate()}
        >
          {sync.isPending ? "요청 중…" : "동기화"}
        </Button>
      </CardHeader>
      <CardContent className="flex flex-col gap-3 p-4 pt-0 text-sm">
        {senders.length === 0 ? (
          <p className="text-xs text-muted-foreground">
            발신프로필을 먼저 추가하세요. 템플릿은 발신프로필(카카오 채널)에 속합니다.
          </p>
        ) : (
          <div className="flex flex-col gap-1">
            <label htmlFor="template-sender" className="text-xs">
              발신프로필
            </label>
            <select
              id="template-sender"
              className="h-9 rounded-md border border-border bg-card px-2 text-sm"
              value={selected}
              onChange={(e) => setSenderId(e.target.value)}
            >
              {senders.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.channel_name || s.sender_key}
                  {s.is_default ? " (기본)" : ""}
                </option>
              ))}
            </select>
          </div>
        )}

        {templates.isPending ? (
          <p className="text-xs text-muted-foreground">불러오는 중…</p>
        ) : rows.length === 0 ? (
          <p className="text-xs text-muted-foreground">
            캐시된 템플릿이 없습니다. 벤더에서 승인된 템플릿을 가져오려면 동기화를 실행하세요.
          </p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[560px] text-left text-xs">
              <thead className="text-muted-foreground">
                <tr className="border-b border-border">
                  <th className="p-2 font-medium">코드</th>
                  <th className="p-2 font-medium">이름</th>
                  <th className="p-2 font-medium">유형</th>
                  <th className="p-2 font-medium">승인 상태</th>
                  <th className="p-2 font-medium">치환자</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((t) => (
                  <TemplateRow key={t.id} template={t} />
                ))}
              </tbody>
            </table>
          </div>
        )}

        {rows.some((t) => isAdMessageType(t.message_type)) && (
          <p className="rounded-md border border-border bg-muted/40 p-2 text-xs">{AD_TEMPLATE_NOTICE}</p>
        )}
        {rows.some((t) => isMissingInVendor(t.vendor_status)) && (
          <p className="rounded-md border border-destructive/40 bg-destructive/5 p-2 text-xs">
            {MISSING_IN_VENDOR_NOTICE} 목록에서 지우지 않는 이유는, 미동기화 템플릿이 발송 전 검증을 건너뛰기
            때문입니다 — 지우면 가드가 조용히 꺼집니다. 저니가 이 템플릿을 참조하고 있는지 확인하세요.
          </p>
        )}
        {msg && <p className="text-xs text-muted-foreground">{msg}</p>}
      </CardContent>
    </Card>
  );
}

function TemplateRow({ template }: { template: AlimtalkTemplate }) {
  const ad = isAdMessageType(template.message_type);
  const missing = isMissingInVendor(template.vendor_status);
  return (
    <tr className={`border-b border-border align-top ${ad ? "bg-amber-50/60" : ""}`}>
      <td className="p-2">
        <code>{template.template_code}</code>
      </td>
      <td className="p-2">{template.name || "(이름 없음)"}</td>
      <td className="p-2">
        {messageTypeLabel(template.message_type)}
        {ad && (
          <span className="ml-1 rounded bg-amber-200 px-1 text-[10px] font-medium text-amber-900" title={AD_TEMPLATE_NOTICE}>
            광고성
          </span>
        )}
      </td>
      <td className="p-2">
        <span
          className={
            missing ? "text-destructive" : template.status === "approved" ? "text-primary" : "text-muted-foreground"
          }
          title={missing ? MISSING_IN_VENDOR_NOTICE : undefined}
        >
          {templateStateLabel(template.status, template.vendor_status)}
        </span>
        {template.vendor_status && !missing && (
          <span className="block text-[10px] text-muted-foreground">벤더 {template.vendor_status}</span>
        )}
      </td>
      <td className="p-2">
        {template.variables.length === 0 ? (
          <span className="text-muted-foreground">없음</span>
        ) : (
          template.variables.map((v) => (
            <code key={v} className="mr-1 rounded bg-muted px-1">
              {`#{${v}}`}
            </code>
          ))
        )}
      </td>
    </tr>
  );
}
