/**
 * Onda API 클라이언트.
 * TODO(S3): openapi.yaml 코드젠 산출물로 대체한다 (ADR-5). 그 전까지 스펙과
 * 이 파일의 드리프트는 리뷰로 관리하며, 콘솔은 반드시 이 패키지만 사용한다
 * (CLAUDE.md 규칙 4 — 수기 fetch 금지의 단일 예외 지점).
 */

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    public readonly body: unknown,
  ) {
    super(`API ${status}`);
    this.name = "ApiError";
  }
}

export interface MeResponse {
  member_id: string;
  tenant_id: string;
  email: string;
  name: string;
  role: "owner" | "admin" | "editor" | "viewer";
  permissions?: string[];
}

/**
 * 로그인 결과 — 2FA 활성 계정은 totp_required, 조직 2FA 강제인데 미등록이면 enrollment_required.
 */
export type LoginResult =
  | { ok: true }
  | { totp_required: true }
  | { enrollment_required: true };

export interface TotpEnrollResponse {
  secret: string;
  otpauth_uri: string;
}

export interface TotpVerifyResponse {
  backup_codes: string[];
}

export interface AuditEntry {
  id: string;
  actor_member_id: string | null;
  actor_email: string | null;
  action: string;
  target_type: string | null;
  target_id: string | null;
  detail: Record<string, unknown>;
  ip: string | null;
  created_at: string;
}

export type MemberRole = "owner" | "admin" | "editor" | "viewer";

export interface Member {
  id: string;
  email: string;
  name: string;
  role: MemberRole;
  status: string;
  totp_enabled: boolean;
  created_at: string;
}

export interface SignupResponse {
  tenant_id: string;
  app_id: string;
  sdk_key: string;
  server_key: string;
}

export class OndaClient {
  constructor(private readonly baseUrl: string) {}

  private async request<T>(
    method: string,
    path: string,
    body?: unknown,
  ): Promise<T> {
    const res = await fetch(`${this.baseUrl}${path}`, {
      method,
      credentials: "include", // httpOnly 세션 쿠키 (ADR-8)
      headers: body ? { "content-type": "application/json" } : undefined,
      body: body ? JSON.stringify(body) : undefined,
    });
    const text = await res.text();
    const json: unknown = text ? JSON.parse(text) : null;
    if (!res.ok) throw new ApiError(res.status, json);
    return json as T;
  }

  readonly auth = {
    signup: (input: {
      email: string;
      password: string;
      name: string;
      tenant_name: string;
    }) => this.request<SignupResponse>("POST", "/v1/auth/signup", input),
    login: (input: { email: string; password: string; totp?: string }) =>
      this.request<LoginResult>("POST", "/v1/auth/login", input),
    logout: () => this.request<{ ok: true }>("POST", "/v1/auth/logout"),
    me: () => this.request<MeResponse>("GET", "/v1/auth/me"),

    // TOTP 2FA (PRD-06 2.1)
    totpStatus: () =>
      this.request<{ enabled: boolean }>("GET", "/v1/auth/totp/status"),
    totpEnroll: () =>
      this.request<TotpEnrollResponse>("POST", "/v1/auth/totp/enroll"),
    totpEnrollVerify: (code: string) =>
      this.request<TotpVerifyResponse>("POST", "/v1/auth/totp/enroll/verify", { code }),
    totpDisable: (code: string) =>
      this.request<{ ok: true }>("POST", "/v1/auth/totp/disable", { code }),
  };

  readonly members = {
    /** 팀 멤버 목록 (team:read) */
    list: () => this.request<{ members: Member[] }>("GET", `/v1/members`),
    /** 멤버 생성 — self-host 초기 비밀번호 지정 (team:write) */
    create: (input: { email: string; name: string; role: MemberRole; password: string }) =>
      this.request<Member>("POST", `/v1/members`, input),
    /** 역할 변경 — 마지막 Owner 강등 금지·세션 폐기 (team:write) */
    changeRole: (memberId: string, role: MemberRole) =>
      this.request<{ ok: true; revoked: number }>(
        "PATCH",
        `/v1/members/${memberId}/role`,
        { role },
      ),
    /** 멤버 제거 — soft delete·세션 폐기 (team:write) */
    remove: (memberId: string) =>
      this.request<{ ok: true; revoked: number }>("DELETE", `/v1/members/${memberId}`),
    /** 관리자 2FA 리셋 (Owner/Admin) */
    resetTotp: (memberId: string) =>
      this.request<{ ok: true }>("POST", `/v1/members/${memberId}/totp/reset`),
  };

  readonly audit = {
    /** 감사 로그 조회 (Admin/Owner) — DEV-sub-07 T-9 */
    list: (limit?: number) =>
      this.request<{ entries: AuditEntry[] }>(
        "GET",
        `/v1/audit${limit ? `?limit=${limit}` : ""}`,
      ),
  };

  readonly tenant = {
    /** 조직 설정·상태 조회 (보안·삭제 유예) */
    get: () =>
      this.request<{
        name: string | null;
        require_2fa: boolean;
        delete_requested_at: string | null;
        purge_after: string | null;
      }>("GET", "/v1/tenant"),
    /** 조직 전체 2FA 강제 on/off (Admin/Owner) — DEV-sub-07 T-5 */
    setRequire2fa: (require2fa: boolean) =>
      this.request<{ ok: true; require_2fa: boolean }>("PUT", "/v1/tenant/security", {
        require_2fa: require2fa,
      }),
    /** 테넌트 삭제 요청 — 7일 유예 후 파기 (Owner) — DEV-sub-07 T-10 */
    requestDeletion: () =>
      this.request<{ ok: true; purge_after: string | null }>("DELETE", "/v1/tenant"),
    /** 삭제 취소 — 유예 내 복구 (Owner) */
    restoreDeletion: () =>
      this.request<{ ok: true }>("POST", "/v1/tenant/restore"),
  };

  readonly apps = {
    list: () => this.request<{ apps: AppSummary[] }>("GET", "/v1/apps"),
    keys: (appId: string) =>
      this.request<{ keys: ApiKeySummary[] }>("GET", `/v1/apps/${appId}/keys`),
    rotateSdkKey: (appId: string, keyId: string) =>
      this.request<{ sdk_key: string; grace_days: number }>(
        "POST",
        `/v1/apps/${appId}/keys/${keyId}/rotate`,
      ),
    createServerKey: (appId: string) =>
      this.request<{ id: string; server_key: string }>("POST", `/v1/apps/${appId}/keys`),
    revokeKey: (appId: string, keyId: string) =>
      this.request<{ ok: true }>("DELETE", `/v1/apps/${appId}/keys/${keyId}`),
    ingestStatus: (appId: string) =>
      this.request<IngestStatus>("GET", `/v1/apps/${appId}/ingest-status`),
    testPush: (appId: string, input: { external_id: string; title: string; body: string }) =>
      this.request<{ queued: number; test_run_id: string }>(
        "POST",
        `/v1/apps/${appId}/test-push`,
        input,
      ),
  };

  readonly segments = {
    list: (appId: string) =>
      this.request<{ segments: SegmentSummary[] }>("GET", `/v1/apps/${appId}/segments`),
    get: (appId: string, id: string) =>
      this.request<SegmentDetail>("GET", `/v1/apps/${appId}/segments/${id}`),
    create: (appId: string, input: { name: string; definition: unknown }) =>
      this.request<{ id: string }>("POST", `/v1/apps/${appId}/segments`, input),
    update: (appId: string, id: string, input: { name: string; definition: unknown }) =>
      this.request<{ ok: true }>("PATCH", `/v1/apps/${appId}/segments/${id}`, input),
    remove: (appId: string, id: string) =>
      this.request<{ ok: true }>("DELETE", `/v1/apps/${appId}/segments/${id}`),
    preview: (appId: string, input: { definition: unknown; category?: string }) =>
      this.request<SegmentPreview>("POST", `/v1/apps/${appId}/segments/preview`, input),
  };

  readonly journeys = {
    list: (appId: string) =>
      this.request<{ journeys: JourneySummary[]; capabilities: JourneyCapabilities }>("GET", `/v1/apps/${appId}/journeys`),
    get: (appId: string, id: string) =>
      this.request<JourneyDetail>("GET", `/v1/apps/${appId}/journeys/${id}`),
    create: (appId: string, input: { name: string; definition: unknown }) =>
      this.request<{ id: string; revision: string }>("POST", `/v1/apps/${appId}/journeys`, input),
    update: (appId: string, id: string, input: { name: string; definition: unknown }) =>
      this.request<{ ok: true; revision: string }>("PATCH", `/v1/apps/${appId}/journeys/${id}`, input),
    validate: (appId: string, id: string) =>
      this.request<JourneyValidation>("POST", `/v1/apps/${appId}/journeys/${id}/validate`),
    activate: (appId: string, id: string, input?: { revision: string }) =>
      this.request<{ version: number; entry: string; audience_ref?: string }>(
        "POST",
        `/v1/apps/${appId}/journeys/${id}/activate`,
        input,
      ),
    pause: (appId: string, id: string) =>
      this.request<{ ok: true }>("POST", `/v1/apps/${appId}/journeys/${id}/pause`),
    archive: (appId: string, id: string) =>
      this.request<{ ok: true }>("DELETE", `/v1/apps/${appId}/journeys/${id}`),
  };

  readonly data = {
    ingestionErrors: (appId: string) =>
      this.request<{ errors: IngestionErrorEntry[] }>(
        "GET",
        `/v1/apps/${appId}/data/ingestion-errors`,
      ),
    attributes: (appId: string) =>
      this.request<{ attributes: AttributeEntry[] }>("GET", `/v1/apps/${appId}/data/attributes`),
    deleteAttribute: (appId: string, key: string, force?: boolean) =>
      this.request<{ deleted: boolean; referencing_segments?: Array<{ id: string; name: string }> }>(
        "DELETE",
        `/v1/apps/${appId}/data/attributes/${encodeURIComponent(key)}${force ? "?force=true" : ""}`,
      ),
  };

  readonly analytics = {
    dashboard: (appId: string) =>
      this.request<DashboardData>("GET", `/v1/apps/${appId}/dashboard`),
    journeyReport: (appId: string, id: string, params?: { version?: number }) =>
      this.request<JourneyReport>("GET", `/v1/apps/${appId}/journeys/${id}/report${params?.version ? `?version=${params.version}` : ""}`),
    usage: (appId: string) => this.request<UsageData>("GET", `/v1/apps/${appId}/usage`),
    uninstalls: (appId: string, days?: number) =>
      this.request<{ days: number; uninstalls: number; active_devices: number; uninstall_rate: number }>(
        "GET",
        `/v1/apps/${appId}/uninstalls${days ? `?days=${days}` : ""}`,
      ),
    /** 앱 삭제 감지 스윕 — 활성 토큰에 무음 푸시 발행(죽은 토큰=삭제 신호) */
    uninstallSweep: (appId: string) =>
      this.request<{ queued: number; run_id: string }>("POST", `/v1/apps/${appId}/uninstall-sweep`),
    deliveryReport: (appId: string, id: string) =>
      this.request<DeliveryReport>("GET", `/v1/apps/${appId}/journeys/${id}/delivery`),
  };

  readonly appSettings = {
    get: (appId: string) => this.request<AppSettings>("GET", `/v1/apps/${appId}/settings`),
    update: (appId: string, input: AppSettings) =>
      this.request<{ ok: true }>("PUT", `/v1/apps/${appId}/settings`, input),
  };

  readonly messageLog = {
    list: (appId: string, params?: { status?: string; journey_id?: string; limit?: number }) => {
      const q = new URLSearchParams();
      if (params?.status) q.set("status", params.status);
      if (params?.journey_id) q.set("journey_id", params.journey_id);
      if (params?.limit) q.set("limit", String(params.limit));
      const qs = q.toString();
      return this.request<MessageLogResponse>(
        "GET",
        `/v1/apps/${appId}/message-log${qs ? `?${qs}` : ""}`,
      );
    },
  };

  readonly users = {
    search: (appId: string, q: string) =>
      this.request<{ users: UserSearchResult[] }>(
        "GET",
        `/v1/apps/${appId}/users?q=${encodeURIComponent(q)}`,
      ),
    detail: (appId: string, id: string) =>
      this.request<UserDetail>("GET", `/v1/apps/${appId}/users/${id}`),
  };

  readonly credentials = {
    list: (appId: string) =>
      this.request<{ credentials: CredentialSummary[] }>(
        "GET",
        `/v1/apps/${appId}/credentials`,
      ),
    upsert: (appId: string, input: CredentialInput) =>
      this.request<{ id: string; kind: string; status: string }>(
        "PUT",
        `/v1/apps/${appId}/credentials`,
        input,
      ),
    remove: (appId: string, kind: CredentialKind) =>
      this.request<{ ok: true }>("DELETE", `/v1/apps/${appId}/credentials/${kind}`),
  };

  readonly emailTemplates = {
    /** 템플릿 목록 (journeys:read) */
    list: (appId: string) =>
      this.request<{ templates: EmailTemplateSummary[] }>("GET", `/v1/apps/${appId}/email-templates`),
    get: (appId: string, id: string) =>
      this.request<EmailTemplate>("GET", `/v1/apps/${appId}/email-templates/${id}`),
    create: (appId: string, input: { name: string; subject: string; html: string }) =>
      this.request<{ id: string }>("POST", `/v1/apps/${appId}/email-templates`, input),
    update: (appId: string, id: string, input: { name: string; subject: string; html: string }) =>
      this.request<{ ok: true }>("PATCH", `/v1/apps/${appId}/email-templates/${id}`, input),
    remove: (appId: string, id: string) =>
      this.request<{ ok: true }>("DELETE", `/v1/apps/${appId}/email-templates/${id}`),
    /** 서버측 {{ }} 치환 미리보기 — 발송과 동일 결과 */
    preview: (appId: string, input: { subject?: string; html: string; variables?: Record<string, unknown> }) =>
      this.request<{ subject?: string; html: string }>("POST", `/v1/apps/${appId}/email-templates/preview`, input),
  };

  /**
   * 발송기 카탈로그 — 이 배포에서 고를 수 있는 커넥터.
   * 콘솔의 "벤더 선택 → 설정 입력"이 이 응답만으로 그려진다(폼은 credentials_schema로 렌더).
   */
  readonly connectors = {
    catalog: (channel?: string) =>
      this.request<{ connectors: ConnectorCatalogEntry[] }>(
        "GET",
        `/v1/connectors${channel ? `?channel=${encodeURIComponent(channel)}` : ""}`,
      ),
  };

  readonly alimtalk = {
    /** 채널 → 커넥터 배선 조회 (Owner/Admin) — 미배선이면 404 */
    connector: {
      get: (appId: string, channel: string) =>
        this.request<ChannelConnector>("GET", `/v1/apps/${appId}/channels/${channel}/connector`),
      put: (
        appId: string,
        channel: string,
        input: { connector_id: string; config?: Record<string, unknown>; enabled?: boolean },
      ) =>
        this.request<ChannelConnector>(
          "PUT",
          `/v1/apps/${appId}/channels/${channel}/connector`,
          input,
        ),
    },
    /** 발신프로필(카카오 채널) — 앱당 여러 개 (journeys:read / journeys:write) */
    senders: {
      list: (appId: string) =>
        this.request<{ senders: AlimtalkSender[] }>("GET", `/v1/apps/${appId}/alimtalk/senders`),
      create: (
        appId: string,
        input: {
          sender_key: string;
          channel_name?: string;
          status?: AlimtalkSenderStatus;
          is_default?: boolean;
        },
      ) => this.request<AlimtalkSender>("POST", `/v1/apps/${appId}/alimtalk/senders`, input),
      update: (
        appId: string,
        id: string,
        input: { channel_name?: string; status?: AlimtalkSenderStatus; is_default?: boolean },
      ) => this.request<AlimtalkSender>("PATCH", `/v1/apps/${appId}/alimtalk/senders/${id}`, input),
      remove: (appId: string, id: string) =>
        this.request<{ ok: true }>("DELETE", `/v1/apps/${appId}/alimtalk/senders/${id}`),
    },
    /** 승인 템플릿 캐시 — Onda는 편집하지 않고 벤더에서 읽어 캐시만 한다 */
    templates: {
      list: (appId: string, params?: { sender_id?: string }) =>
        this.request<{ templates: AlimtalkTemplate[] }>(
          "GET",
          `/v1/apps/${appId}/alimtalk/templates${params?.sender_id ? `?sender_id=${params.sender_id}` : ""}`,
        ),
      /**
       * 벤더 승인 템플릿 동기화 요청 (journeys:write). 202 + 워커가 비동기 수행.
       *
       * 발신프로필·커넥터 배선·검증된 크리덴셜이 모두 있어야 202가 나온다. 하나라도 없으면
       * 400과 함께 무엇이 없는지 한국어로 지목한다 — 워커가 아무것도 못 하는 상태에서
       * 202를 주면 "요청됐다"는 거짓 신호가 되기 때문이다.
       *
       * sender_id 생략 시: 기본 발신프로필, 없고 하나뿐이면 그 하나. 여러 개인데 기본이
       * 없으면 400으로 되묻는다.
       */
      sync: (appId: string, body?: { sender_id?: string }) =>
        this.request<{ accepted: true; sender_id: string }>(
          "POST",
          `/v1/apps/${appId}/alimtalk/templates/sync`,
          body,
        ),
    },
  };

  readonly email = {
    /** 테스트 이메일 발송 — 템플릿/인라인을 {{ }} 치환 후 실전송 (journeys:activate) */
    test: (
      appId: string,
      input: {
        to_email: string;
        template_id?: string;
        subject?: string;
        html?: string;
        provider?: EmailProvider;
        variables?: Record<string, unknown>;
      },
    ) => this.request<{ queued: number; test_run_id: string }>("POST", `/v1/apps/${appId}/test-email`, input),
  };
}

export interface AppSummary {
  id: string;
  name: string;
  timezone: string;
  created_at: string;
}

export interface ApiKeySummary {
  id: string;
  kind: "sdk" | "server";
  scope: "full" | "ingest_only";
  prefix: string;
  status: "active" | "rotating" | "revoked";
  grace_expires_at: string | null;
  last_used_at: string | null;
  created_at: string;
}

export interface IngestStatus {
  events_total: number;
  last_event_at: string | null;
}

/** 크리덴셜 종류 (credentials.kind = PG channel_kind enum) */
export type CredentialKind = "push_fcm" | "push_apns" | EmailProvider | "alimtalk";
/** 이메일 발송기(provider) — 저니 이메일 노드·테스트 발송의 provider 값 */
export type EmailProvider = "email_smtp" | "email_nhn" | "email_resend";
export const EMAIL_PROVIDERS: readonly EmailProvider[] = ["email_smtp", "email_nhn", "email_resend"];
/** 콘솔 표시용 발송기 라벨 */
export const EMAIL_PROVIDER_LABELS: Record<EmailProvider, string> = {
  email_smtp: "SMTP",
  email_nhn: "NHN Cloud",
  email_resend: "Resend",
};
export type CredentialInput =
  | FcmCredentialInput
  | ApnsCredentialInput
  | EmailSmtpCredentialInput
  | EmailNhnCredentialInput
  | EmailResendCredentialInput
  | AlimtalkCredentialInput;

export interface CredentialSummary {
  id: string;
  kind: CredentialKind;
  status: "unverified" | "verified" | "error";
  status_detail: string | null;
  last_verified_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface SegmentSummary {
  id: string;
  name: string;
  status: "active" | "broken";
  status_detail: string | null;
  last_count: number | null;
  last_evaluated_at: string | null;
  updated_at: string;
}

export interface SegmentDetail {
  id: string;
  name: string;
  definition: unknown;
  status: "active" | "broken";
  status_detail: string | null;
  last_count: number | null;
  updated_at: string;
}

export interface SegmentPreview {
  approx_count: number;
  sample: Array<{ user_id: string; external_id: string | null; platforms: string[] }>;
}

export interface JourneySummary {
  id: string;
  name: string;
  status: "draft" | "active" | "paused" | "archived";
  category: "marketing" | "transactional";
  active_version: number | null;
  updated_at: string;
}

export interface JourneyDetail extends JourneySummary {
  draft_definition: unknown;
  revision: string;
  published_ab_nodes: Record<string, { variants: Array<{ id: string; label: string; weight: number }> }>;
  capabilities: JourneyCapabilities;
}

export interface JourneyCapabilities {
  graph_v2: boolean;
  supported_node_types: string[];
}

export interface JourneyValidation {
  issues: Array<{ level: "error" | "warning"; message: string; node_index?: number; node_id?: string; edge_id?: string; field?: string }>;
  estimated_count: number | null;
  revision: string;
}

export interface IngestionErrorEntry {
  endpoint: string;
  reason: string;
  detail: string;
  payload: string;
  request_id: string;
  received_at: string;
}

export interface AttributeEntry {
  key: string;
  type: string;
  first_seen_at: string;
  last_seen_at: string;
  seg_ref_count: number;
}

export interface DashboardData {
  today: { sent: number; failed: number; skipped: number; by_status: Record<string, number> };
  active_journeys: number;
}

export interface JourneyReport {
  name: string;
  status: string;
  state_distribution: Record<string, number>;
  sends: Array<{ status: string; node_index: number; count: number }>;
  version: number | null;
  versions: Array<{ version: number; created_at: string }>;
  definition: unknown | null;
  instrumentation: "available" | "unsupported" | "unpublished";
  nodes: JourneyNodeReport[];
}

export interface JourneyNodeReport {
  node_id: string;
  node_index: number;
  type: string;
  arrived: number;
  waiting: number;
  completed: number;
  failed: number;
  paths: Array<{ output_port: string; executions: number; unique_users: number }>;
}

export interface UsageData {
  mau_30d: number;
  dau_today: number;
  sends_30d: Array<{ channel: string; sent: number }>;
}

/**
 * 도달·오픈 리포트 (R-15): sent=공급자 접수(실도달 아님).
 * delivered/opened = SDK 이벤트($push_delivered/$push_opened) ∪ message_lifecycle(공급자 콜백 — 예: Resend 웹훅)을
 * sent된 message_id로 조인·중복 제거. clicked/bounced는 message_lifecycle(이메일 콜백)에서만 집계.
 */
export interface DeliveryReport {
  sent: number;
  delivered: number;
  opened: number;
  clicked: number;
  bounced: number;
  delivery_rate: number;
  open_rate: number;
}

export interface AppSettings {
  timezone: string;
  quiet_hours: {
    enabled: boolean;
    start: string;
    end: string;
    policy: "delay_until_open" | "skip";
  };
  frequency_cap: { enabled: boolean; max_per_24h: number };
}

export interface MessageLogEntry {
  message_id: string;
  idempotency_key: string;
  journey_id: string;
  journey_version: number;
  node_index: number;
  campaign_ref: string;
  user_id: string;
  device_id: string;
  channel: string;
  status: string;
  failure_class: string;
  failure_detail: string;
  sent_at: string;
}

export interface MessageLogResponse {
  messages: MessageLogEntry[];
  recent_hour: { total: number; failed: number; failure_rate: number };
}

export interface UserSearchResult {
  id: string;
  external_id: string | null;
  email: string | null;
  status: string;
  last_seen_at: string | null;
}

export interface UserDetail {
  user: {
    id: string;
    external_id: string | null;
    std_attrs: Record<string, unknown>;
    custom_attrs: Record<string, unknown>;
    subscriptions: Record<string, unknown>;
    status: string;
    last_seen_at: string | null;
    created_at: string;
  };
  devices: Array<{
    id: string;
    platform: string;
    token_status: string;
    os_permission: string;
    has_token: boolean;
    device_meta: Record<string, unknown>;
    last_active_at: string | null;
    updated_at: string;
  }>;
  journeys: Array<{
    journey_id: string;
    name: string;
    journey_version: number;
    current_node: number;
    status: string;
    next_wake_at: string | null;
    entered_at: string;
  }>;
  events: Array<{ event_name: string; ts: string }>;
  messages: Array<{
    channel: string;
    status: string;
    failure_class: string;
    failure_detail: string;
    journey_id: string;
    sent_at: string;
  }>;
}

/** SMTP 이메일 크리덴셜 (AWS SES SMTP·범용 SMTP·MailHog). security: starttls(587)|tls(465)|none. */
export interface EmailSmtpCredentialInput {
  kind: "email_smtp";
  host: string;
  port: number;
  username?: string;
  password?: string;
  from_email: string;
  from_name?: string;
  security?: "starttls" | "tls" | "none";
}

/** NHN Cloud(TOAST) Email API 크리덴셜. */
export interface EmailNhnCredentialInput {
  kind: "email_nhn";
  app_key: string;
  secret_key: string;
  from_email: string;
  from_name?: string;
}

/**
 * Resend Email API 크리덴셜. webhook_secret = Resend 웹훅(Svix) 서명 비밀(whsec_…) —
 * `POST {API_URL}/v1/webhooks/resend/{appId}` 서명 검증에 사용. base_url은 테스트용.
 */
export interface EmailResendCredentialInput {
  kind: "email_resend";
  api_key: string;
  from_email: string;
  from_name?: string;
  webhook_secret?: string;
  base_url?: string;
}

export interface EmailTemplateSummary {
  id: string;
  name: string;
  subject: string;
  updated_at: string;
}

export interface EmailTemplate {
  id: string;
  name: string;
  subject: string;
  html: string;
  created_at: string;
  updated_at: string;
}

/**
 * 알림톡 벤더(딜러사) 크리덴셜. connector_id가 어느 벤더인지 결정한다 —
 * channel_kind enum에는 'alimtalk' 하나뿐이므로 제3자 벤더도 enum 변경 없이 들어온다.
 * 필수 필드가 벤더마다 다르므로 느슨하다: 엄격한 검증은 벤더 manifest로 워커가 한다.
 */
export interface AlimtalkCredentialInput {
  kind: "alimtalk";
  connector_id: string;
  /**
   * 흔한 이름일 뿐 모든 벤더가 쓰는 이름이 아니다. 이 슬롯에 대응하는 필드가 없는
   * 벤더도 있으므로 선택이며, 서버는 "슬롯이든 extra든 비밀이 하나라도 있으면 된다"까지만 강제한다.
   */
  api_key?: string;
  secret_key?: string;
  sender_key?: string;
  base_url?: string;
  /**
   * 매니페스트가 선언한 필드를 **이름 그대로** 담는다. 벤더가 실제로 읽는 값이다
   * (NHN은 app_key, 다른 딜러사는 또 다르다). 저장 시 펼쳐지며 이름 있는 슬롯이 이긴다.
   *
   * 슬롯 넷만으로는 제3자 벤더가 다섯 번째 비밀을 선언하는 순간 저장할 방법이 없어져
   * "매니페스트만 있으면 벤더가 들어온다"는 계약이 닫히지 않는다.
   */
  extra?: Record<string, string>;
}

/** 앱별 채널 → 커넥터 배선. config는 비밀이 아닌 설정만(비밀은 credentials에). */
/**
 * 커넥터 매니페스트의 콘솔용 투영. 단일 출처는 배포의 매니페스트 디렉터리이고,
 * API와 워커가 같은 디렉터리를 읽는다.
 *
 * 여기 있다고 발송이 되는 것은 아니다 — in_process_go 커넥터는 워커 바이너리에 구현이
 * 포함돼 있어야 하며, 없으면 워커가 기동에서 실패한다.
 */
export interface ConnectorCatalogEntry {
  id: string;
  name: string;
  description?: string;
  version: string;
  channel: string;
  vendor: { name: string; url?: string; support?: string };
  tier?: string;
  runtime: "in_process_go" | "remote_http";
  /** 크리덴셜 입력 폼을 그리는 JSON Schema. 벤더마다 필드가 달라 폼을 손으로 짤 수 없다. */
  credentials_schema: unknown;
  /** 비밀이 아닌 앱 단위 설정 폼 (발신번호·기본 발신프로필 등) */
  config_schema?: unknown;
  capabilities: Record<string, unknown>;
  /** 이 커넥터가 보고할 수 있는 상태. 리포트가 "미지원"과 "0"을 구분하는 근거. */
  reports: string[];
  /** 웹훅형이면 등록할 경로 조각. 없으면 폴링형이라 등록할 것이 없다. */
  callback_path?: string;
  compliance?: Record<string, unknown>;
  cost?: Record<string, unknown>;
}

export interface ChannelConnector {
  id: string;
  channel: string;
  connector_id: string;
  config: Record<string, unknown>;
  enabled: boolean;
  created_at?: string;
  updated_at?: string;
}

export type AlimtalkSenderStatus = "active" | "disabled";

export interface AlimtalkSender {
  id: string;
  sender_key: string;
  channel_name: string;
  status: AlimtalkSenderStatus;
  is_default: boolean;
  created_at?: string;
  updated_at?: string;
}

/** 카카오 승인 템플릿 캐시. content는 완성 텍스트를 요구하는 벤더를 위한 렌더 원본. */
export interface AlimtalkTemplate {
  id: string;
  sender_id: string;
  template_code: string;
  name: string;
  content: string;
  message_type: string;
  emphasize_type: string;
  variables: string[];
  buttons: unknown[];
  quick_replies: unknown[];
  status: string;
  vendor_status: string;
  synced_at: string | null;
  updated_at: string;
}

export interface FcmCredentialInput {
  kind: "push_fcm";
  service_account: Record<string, unknown>;
}

export interface ApnsCredentialInput {
  kind: "push_apns";
  p8: string;
  key_id: string;
  team_id: string;
  bundle_id: string;
  environment?: "production" | "sandbox";
}
