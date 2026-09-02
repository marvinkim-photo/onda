"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { EMAIL_PROVIDER_LABELS, type CredentialKind, type CredentialSummary } from "@onda/api-client";
import { api } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { EmailProviderCard } from "../email-templates/email-provider-card";

const KIND_LABELS: Record<CredentialKind, string> = {
  push_fcm: "FCM",
  push_apns: "APNs",
  email_smtp: `이메일 · ${EMAIL_PROVIDER_LABELS.email_smtp}`,
  email_nhn: `이메일 · ${EMAIL_PROVIDER_LABELS.email_nhn}`,
  email_resend: `이메일 · ${EMAIL_PROVIDER_LABELS.email_resend}`,
  alimtalk: "카카오 알림톡",
};

/** 위저드 2단계 — 채널 크리덴셜 등록: 푸시(FCM/APNs) + 이메일 발송기(SMTP/SES/Resend/NHN) (PRD-05 3.1). 검증 상태는 5s 폴링. */
export function CredentialsStep({ appId }: { appId: string }) {
  const queryClient = useQueryClient();
  const creds = useQuery({
    queryKey: ["credentials", appId],
    queryFn: () => api.credentials.list(appId),
    refetchInterval: (q) =>
      q.state.data?.credentials.some((c) => c.status === "unverified") ? 5000 : false,
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ["credentials", appId] });

  return (
    <div className="flex flex-col gap-6">
      <div className="grid gap-6 md:grid-cols-2">
        <FcmForm appId={appId} onDone={invalidate} />
        <ApnsForm appId={appId} onDone={invalidate} />
      </div>
      <div className="flex flex-col gap-2">
        <p className="text-sm text-muted-foreground">
          이메일 채널(선택) — SMTP·AWS SES·Resend(SMTP/API)·NHN Cloud 중 하나를 등록하면 저니 이메일 노드를 쓸 수 있습니다.
        </p>
        <EmailProviderCard appId={appId} onSaved={invalidate} />
      </div>
      <div className="flex flex-col gap-2">
        {creds.data?.credentials.map((c) => <CredentialBadge key={c.id} cred={c} />)}
        {creds.data?.credentials.length === 0 && (
          <p className="text-sm text-muted-foreground">아직 등록된 크리덴셜이 없습니다.</p>
        )}
      </div>
    </div>
  );
}

function CredentialBadge({ cred }: { cred: CredentialSummary }) {
  const label = KIND_LABELS[cred.kind] ?? cred.kind;
  const color =
    cred.status === "verified"
      ? "text-primary"
      : cred.status === "error"
        ? "text-destructive"
        : "text-muted-foreground";
  const statusText =
    cred.status === "verified"
      ? "검증 완료 ✓"
      : cred.status === "error"
        ? "검증 실패"
        : "검증 중…";
  return (
    <div className="rounded-md border border-border p-3 text-sm">
      <span className="font-medium">{label}</span> —{" "}
      <span className={color}>{statusText}</span>
      {cred.status === "error" && cred.status_detail && (
        <p className="mt-1 text-xs text-destructive">{cred.status_detail}</p>
      )}
    </div>
  );
}

function FcmForm({ appId, onDone }: { appId: string; onDone: () => void }) {
  const [json, setJson] = useState("");
  const [parseError, setParseError] = useState<string | null>(null);
  const upsert = useMutation({
    mutationFn: (serviceAccount: Record<string, unknown>) =>
      api.credentials.upsert(appId, { kind: "push_fcm", service_account: serviceAccount }),
    onSuccess: onDone,
  });

  return (
    <form
      className="flex flex-col gap-3"
      onSubmit={(e) => {
        e.preventDefault();
        setParseError(null);
        try {
          upsert.mutate(JSON.parse(json) as Record<string, unknown>);
        } catch {
          setParseError("올바른 JSON이 아닙니다");
        }
      }}
    >
      <Label>FCM — 서비스 계정 JSON</Label>
      <textarea
        className="h-32 rounded-md border border-border bg-card p-2 font-mono text-xs"
        placeholder='{"type":"service_account","project_id":"…"}'
        value={json}
        onChange={(e) => setJson(e.target.value)}
      />
      {(parseError || upsert.isError) && (
        <p className="text-xs text-destructive">
          {parseError ?? "등록 실패 — JSON 필드(project_id·private_key·client_email)를 확인하세요"}
        </p>
      )}
      <Button type="submit" variant="outline" disabled={upsert.isPending || !json}>
        FCM 등록
      </Button>
    </form>
  );
}

function ApnsForm({ appId, onDone }: { appId: string; onDone: () => void }) {
  const [form, setForm] = useState({ p8: "", key_id: "", team_id: "", bundle_id: "" });
  const upsert = useMutation({
    mutationFn: () => api.credentials.upsert(appId, { kind: "push_apns", ...form }),
    onSuccess: onDone,
  });
  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm({ ...form, [k]: e.target.value });

  return (
    <form
      className="flex flex-col gap-3"
      onSubmit={(e) => {
        e.preventDefault();
        upsert.mutate();
      }}
    >
      <Label>APNs — p8 키</Label>
      <textarea
        className="h-16 rounded-md border border-border bg-card p-2 font-mono text-xs"
        placeholder="-----BEGIN PRIVATE KEY-----"
        value={form.p8}
        onChange={(e) => setForm({ ...form, p8: e.target.value })}
      />
      <div className="grid grid-cols-3 gap-2">
        <Input placeholder="Key ID" value={form.key_id} onChange={set("key_id")} />
        <Input placeholder="Team ID" value={form.team_id} onChange={set("team_id")} />
        <Input placeholder="Bundle ID" value={form.bundle_id} onChange={set("bundle_id")} />
      </div>
      {upsert.isError && (
        <p className="text-xs text-destructive">등록 실패 — 입력값을 확인하세요</p>
      )}
      <Button
        type="submit"
        variant="outline"
        disabled={upsert.isPending || !form.p8 || !form.key_id || !form.team_id || !form.bundle_id}
      >
        APNs 등록
      </Button>
    </form>
  );
}
