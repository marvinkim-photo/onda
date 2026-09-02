"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import type { ChannelConnector, ConnectorCatalogEntry } from "@onda/api-client";
import { api } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { ALIMTALK_CHANNEL } from "./channel";
import { connectorWebhookUrl, reportSummary, serverMessage } from "./alimtalk-labels";
import {
  canSubmit,
  fieldDestination,
  initialValues,
  planConfig,
  planCredential,
  schemaFields,
} from "./connector-schema";
import { SchemaFields } from "./schema-fields";

/** 배선에 저장된 config 중 문자열 값만 폼으로 되돌린다 — 폼 위젯이 다룰 수 있는 형태만 쓴다. */
function savedStringConfig(wired: ChannelConnector | null): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(wired?.config ?? {})) {
    if (typeof value === "string") out[key] = value;
  }
  return out;
}

/**
 * 벤더 설정 입력 — 크리덴셜(비밀)과 채널 배선(config)을 한 번에 저장한다.
 *
 * 폼은 손으로 짜지 않는다: `credentials_schema` · `config_schema`를 그대로 렌더하므로
 * 새 벤더를 매니페스트로 추가해도 콘솔은 그대로다.
 * 부모가 `key={connector.id}`로 렌더해 벤더를 바꾸면 입력값이 초기화된다.
 */
export function ConnectorSetup({
  appId,
  connector,
  wired,
  onSaved,
}: {
  appId: string | undefined;
  connector: ConnectorCatalogEntry;
  wired: ChannelConnector | null;
  onSaved: () => void;
}) {
  const credentialFields = useMemo(() => schemaFields(connector.credentials_schema), [connector]);
  const configFields = useMemo(() => schemaFields(connector.config_schema), [connector]);
  const [credentialValues, setCredentialValues] = useState(() => initialValues(credentialFields));
  const [configValues, setConfigValues] = useState(() => ({
    ...initialValues(configFields),
    // 이미 배선돼 있으면 저장된 비(非)비밀 설정을 그대로 보여 준다(비밀은 절대 돌려받지 않는다).
    ...savedStringConfig(wired),
  }));
  const [msg, setMsg] = useState<string | null>(null);

  const plan = planCredential(credentialFields, credentialValues);

  const creds = useQuery({
    queryKey: ["credentials", appId],
    queryFn: () => api.credentials.list(appId!),
    enabled: !!appId,
    // 검증은 채널 워커가 비동기로 한다 — unverified인 동안만 짧게 되묻는다.
    refetchInterval: (query) =>
      (query.state.data?.credentials ?? []).some((c) => c.kind === "alimtalk" && c.status === "unverified")
        ? 5000
        : false,
  });
  const credential = (creds.data?.credentials ?? []).find((c) => c.kind === "alimtalk") ?? null;

  const save = useMutation({
    mutationFn: async () => {
      if (!appId) throw new Error("앱을 찾을 수 없습니다");
      await api.credentials.upsert(appId, {
        kind: "alimtalk",
        connector_id: connector.id,
        // 벤더가 실제로 읽는 이름은 매니페스트가 정한다 — 슬롯 이름으로만 보내면
        // 이름이 다른 벤더(NHN의 app_key)가 "필드 누락"으로 검증에서 떨어진다.
        extra: plan.extra,
        // 슬롯은 흔한 이름을 위한 호환이라 매핑되는 필드가 없으면 아예 보내지 않는다.
        ...plan.credential,
      });
      await api.alimtalk.connector.put(appId, ALIMTALK_CHANNEL, {
        connector_id: connector.id,
        config: planConfig(configFields, configValues),
        enabled: true,
      });
    },
    onSuccess: () => {
      setMsg("저장했습니다 — 워커가 검증 중입니다(수 초). 아래 상태에서 결과를 확인하세요.");
      void creds.refetch();
      onSaved();
    },
    onError: (e) => setMsg(serverMessage(e, "저장에 실패했습니다")),
  });

  const toggle = useMutation({
    mutationFn: () => {
      if (!appId || !wired) throw new Error("배선이 없습니다");
      return api.alimtalk.connector.put(appId, ALIMTALK_CHANNEL, {
        connector_id: wired.connector_id,
        config: wired.config,
        enabled: !wired.enabled,
      });
    },
    onSuccess: () => {
      setMsg(wired?.enabled ? "발송을 중지했습니다." : "발송을 재개했습니다.");
      onSaved();
    },
    onError: (e) => setMsg(serverMessage(e, "변경에 실패했습니다")),
  });

  const blocked = !canSubmit(plan);

  return (
    <Card>
      <CardHeader className="p-4">
        <CardTitle className="text-sm">설정 입력 · {connector.name}</CardTitle>
      </CardHeader>
      <CardContent className="flex flex-col gap-3 p-4 pt-0 text-sm">
        <VendorSummary connector={connector} />

        <div className="flex flex-col gap-2">
          <p className="text-xs font-medium">벤더 크리덴셜</p>
          {credentialFields.length === 0 ? (
            <p className="text-xs text-destructive">
              이 커넥터의 매니페스트에 크리덴셜 스키마가 없습니다 — 배포의 매니페스트를 확인하세요.
            </p>
          ) : (
            <SchemaFields
              fields={credentialFields}
              values={credentialValues}
              idPrefix={`cred-${connector.id}`}
              disabled={save.isPending}
              hint={fieldDestination}
              onChange={(name, value) => setCredentialValues((v) => ({ ...v, [name]: value }))}
            />
          )}
        </div>

        {configFields.length > 0 && (
          <div className="flex flex-col gap-2">
            <p className="text-xs font-medium">앱 설정 (비밀 아님)</p>
            <SchemaFields
              fields={configFields}
              values={configValues}
              idPrefix={`conf-${connector.id}`}
              disabled={save.isPending}
              onChange={(name, value) => setConfigValues((v) => ({ ...v, [name]: value }))}
            />
          </div>
        )}

        {plan.empty && plan.missingRequired.length === 0 && (
          <p className="text-xs text-destructive">저장할 값이 하나도 없습니다. 위 필드를 채워 주세요.</p>
        )}
        {plan.missingRequired.length > 0 && (
          <p className="text-xs text-muted-foreground">필수 입력: {plan.missingRequired.join(" · ")}</p>
        )}

        <div className="flex flex-wrap gap-2">
          <Button disabled={!appId || save.isPending || blocked} onClick={() => save.mutate()}>
            {save.isPending ? "저장 중…" : wired?.connector_id === connector.id ? "설정 저장" : "이 벤더로 배선"}
          </Button>
          {wired?.connector_id === connector.id && (
            <Button variant="outline" disabled={toggle.isPending} onClick={() => toggle.mutate()}>
              {wired.enabled ? "발송 중지" : "발송 재개"}
            </Button>
          )}
        </div>
        {msg && <p className="text-xs text-muted-foreground">{msg}</p>}

        <VerificationStatus
          status={credential?.status ?? null}
          detail={credential?.status_detail ?? null}
          pending={creds.isPending}
        />

        {connector.callback_path ? (
          <WebhookGuide appId={appId} connector={connector} />
        ) : (
          <p className="rounded-md border border-border bg-muted/40 p-2 text-xs text-muted-foreground">
            이 벤더는 결과를 조회(폴링)로 수집하므로 등록할 웹훅이 없습니다. 워커가 미종결 발송을 주기적으로
            되물어 도달·실패를 채웁니다.
          </p>
        )}
      </CardContent>
    </Card>
  );
}

/** 벤더가 무엇을 보고하는지 — 리포트에서 "미지원"과 "0건"을 가르는 근거를 설정 화면에서 미리 밝힌다. */
function VendorSummary({ connector }: { connector: ConnectorCatalogEntry }) {
  return (
    <div className="rounded-md border border-border bg-muted/40 p-2 text-xs">
      <p>
        <span className="font-medium">{connector.vendor.name || "벤더 미상"}</span> · 버전 {connector.version} ·{" "}
        {connector.runtime === "in_process_go" ? "워커 내장" : "원격 HTTP"}
      </p>
      {connector.description && <p className="mt-1 text-muted-foreground">{connector.description}</p>}
      <p className="mt-1">{reportSummary(connector.reports)}</p>
      {connector.vendor.url && (
        <p className="mt-1">
          <ExternalLink href={connector.vendor.url}>벤더 콘솔 열기</ExternalLink>
          {connector.vendor.support && (
            <>
              {" · "}
              <ExternalLink href={connector.vendor.support}>연동 문서</ExternalLink>
            </>
          )}
        </p>
      )}
    </div>
  );
}

/** 검증은 워커가 비동기로 한다 — 저장 직후 "검증 중"이 정상이다. */
function VerificationStatus({
  status,
  detail,
  pending,
}: {
  status: "unverified" | "verified" | "error" | null;
  detail: string | null;
  pending: boolean;
}) {
  if (pending) return <p className="text-xs text-muted-foreground">검증 상태 확인 중…</p>;
  if (!status) return <p className="text-xs text-muted-foreground">등록된 알림톡 크리덴셜이 없습니다.</p>;
  return (
    <p className="text-xs">
      검증 상태:{" "}
      <span
        className={
          status === "verified" ? "text-primary" : status === "error" ? "text-destructive" : "text-muted-foreground"
        }
      >
        {status === "verified" ? "검증 완료" : status === "error" ? "검증 실패" : "검증 중 (5초마다 확인)"}
      </span>
      {status === "error" && detail && <span className="text-destructive"> — {detail}</span>}
    </p>
  );
}

/** 콜백형 벤더에만 보인다. 폴링형에는 등록할 URL이 없어 빈 상자를 띄우지 않는다. */
function WebhookGuide({ appId, connector }: { appId: string | undefined; connector: ConnectorCatalogEntry }) {
  const [copied, setCopied] = useState(false);
  if (!appId) return null;
  const url = connectorWebhookUrl(
    process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080",
    appId,
    connector.id,
  );
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(url);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      setCopied(false);
    }
  };
  return (
    <div className="rounded-md border border-border bg-muted/40 p-2 text-xs">
      <p className="font-medium">벤더 콘솔에 등록할 웹훅 URL</p>
      <p className="mt-1 text-muted-foreground">
        이 주소를 벤더의 결과 수신(콜백) 설정에 등록해야 도달·실패가 집계됩니다.
      </p>
      <div className="mt-1 flex items-center gap-1">
        <code className="flex-1 truncate rounded bg-card px-1 py-0.5" title={url}>
          {url}
        </code>
        <Button type="button" variant="outline" className="h-6 px-2 text-xs" onClick={copy}>
          {copied ? "복사됨" : "복사"}
        </Button>
      </div>
    </div>
  );
}

/** 외부 콘솔로 나가는 링크 — 항상 새 탭, 아이콘으로 이탈을 알린다. */
export function ExternalLink({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      className="inline-flex items-center gap-1 whitespace-nowrap text-xs text-primary underline-offset-2 hover:underline"
    >
      {children}
      <svg
        aria-hidden="true"
        viewBox="0 0 24 24"
        className="h-3 w-3 shrink-0"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
      >
        <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" />
        <path d="M15 3h6v6" />
        <path d="M10 14 21 3" />
      </svg>
      <span className="sr-only">(새 탭에서 열림)</span>
    </a>
  );
}
