import { readFileSync } from "node:fs";
import { join } from "node:path";

/**
 * 큐 토픽·메시지 스키마의 단일 출처 (DEV-MAIN §5, ADR-6).
 * 스트림 키·컨슈머 그룹 이름과 JSON Schema를 TS 측에 노출한다.
 * Go 측(libqueue-go)은 동일한 schemas/ 디렉터리를 임베드한다.
 */

/** Redis Streams 키. 접근은 반드시 libqueue 경유 (CLAUDE.md 규칙 2). */
export const STREAMS = {
  ingest: "stream:ingest",
  /** 정규화 이벤트 (user_id 해석 후) — 트리거 매처가 구독 */
  events: "stream:events",
  journeyEntry: "stream:journey.entry",
  journeyWake: "stream:journey.wake",
  dispatch: "stream:dispatch",
  sendPush: "stream:send.push",
  sendEmail: "stream:send.email",
  /** 채널 중립 발송 (send.message.v1) — 신규 커넥터는 이 스트림만 구독 */
  sendMessage: "stream:send.message",
  /** 발송 수명주기 (message.lifecycle.v1) — 커넥터·콜백·SDK가 수렴 */
  messageLifecycle: "stream:message.lifecycle",
  feedback: "stream:feedback",
} as const;

export type StreamKey = (typeof STREAMS)[keyof typeof STREAMS];

/** Consumer group 이름 (DEV-sub-01: cg:ingest 등) */
export const CONSUMER_GROUPS = {
  ingest: "cg:ingest",
  triggerMatcher: "cg:trigger-matcher",
  scheduler: "cg:scheduler",
  fanout: "cg:fanout",
  channel: "cg:channel",
  channelEmail: "cg:channel.email",
  channelMessage: "cg:channel.message",
  lifecycle: "cg:lifecycle",
  feedback: "cg:feedback",
} as const;

/** stream:events payload — 정규화된 단일 이벤트 (트리거 진입·이탈 매칭용) */
export interface NormalizedEventPayload {
  user_id: string;
  event_name: string;
  occurred_at: string;
  /** Durable receipt identity. Optional only for pre-upgrade stream entries. */
  insert_id?: string;
  /** Decimal bigint string; never a lossy JS number. */
  receipt_seq?: string;
  received_at?: string;
  client_ts?: string;
  properties?: Record<string, unknown>;
}

/** 메시지 type — 파괴적 변경은 신규 type으로 (schema_ver 규칙) */
export type MessageType =
  | "ingest.batch"
  | "event.normalized"
  | "journey.enter"
  | "journey.wake"
  | "dispatch.fanout"
  | "send.push"
  | "send.email"
  | "send.message"
  | "message.lifecycle"
  | "feedback.token";

/** 모든 큐 메시지의 공통 envelope (DEV-MAIN §5) */
export interface Envelope<P = Record<string, unknown>> {
  id: string;
  type: MessageType;
  schema_ver: number;
  tenant_id: string;
  app_id: string;
  occurred_at: string;
  trace_id: string;
  payload: P;
}

export type IngestEndpoint =
  | "track"
  | "identify"
  | "attributes"
  | "devices_token"
  | "user_delete"
  | "subscriptions"
  | "devices_logout";

export interface IngestDeviceInfo {
  device_id: string;
  platform: "ios" | "android";
  app_version?: string;
  os_version?: string;
  model?: string;
  locale?: string;
}

export interface IngestTrackEvent {
  insert_id: string;
  /** PG canonical customer / immutable receipt, filled by the track API. */
  user_id?: string;
  receipt_seq?: string;
  received_at?: string;
  anon_id?: string | null;
  external_id?: string | null;
  event: string;
  properties?: Record<string, unknown>;
  client_ts: string;
  server_ts?: string;
}

/** ingest 스트림 payload (검증·정규화 후) */
export interface IngestBatchPayload {
  endpoint: IngestEndpoint;
  request_id: string;
  api_key_id?: string;
  device?: IngestDeviceInfo;
  events?: IngestTrackEvent[];
  identify?: {
    external_id: string;
    anon_id?: string | null;
    attributes?: Record<string, unknown>;
  };
  attributes?: Array<{
    external_id: string;
    attributes: Record<string, unknown>;
  }>;
  token?: {
    push_token: string;
    os_permission?: "granted" | "denied" | "undetermined";
    anon_id?: string | null;
    external_id?: string | null;
  };
  user_delete?: { external_id: string };
  // R-03: 수신 동의 변경·로그아웃(토큰 소유권 해제) 서버 동기화
  subscription?: {
    anon_id?: string | null;
    external_id?: string | null;
    channel: "push";
    state: "opted_in" | "unsubscribed";
  };
  logout?: { device_id: string };
}

/** journey.entry 스트림 payload */
export interface JourneyEntryPayload {
  journey_id: string;
  version: number;
  source: "blast" | "trigger";
  audience_ref?: string | null;
  user_id?: string | null;
  /** v2 admission identity; UUID for trigger, blast:<audience_ref> for blast. */
  entry_id?: string;
  receipt_seq?: string;
}

/** send.push 스트림 payload — 멱등 키에 device_id 포함 (PRD-03 4.3 v0.2) */
export interface SendPushPayload {
  idempotency_key: string;
  /** 발송 시점 생성 안정 ID — message_log·푸시 data(onda.message_id)·SDK 도달/오픈 연결 (재검증 F) */
  message_id: string;
  user_id: string;
  device_id: string;
  push_token: string;
  platform: "ios" | "android";
  content: {
    push: {
      title: string;
      body: string;
      image_url?: string;
      deep_link?: string;
      data?: Record<string, string>;
      silent?: boolean;
    };
  };
  category: "marketing" | "transactional";
  journey_id?: string | null;
  journey_version?: number | null;
  node_index?: number | null;
  campaign_ref?: string | null;
}

/** 이메일 발송기(provider) 종류 — credentials.kind와 동일 값 (email_resend = Resend API) */
export type EmailProvider = "email_smtp" | "email_nhn" | "email_resend";

/** send.email 스트림 payload — content.email은 {{ }} 치환 완료 본문, email=수신 주소 */
export interface SendEmailPayload {
  idempotency_key: string;
  message_id?: string;
  user_id?: string | null;
  email: string;
  provider?: EmailProvider;
  content: {
    email: {
      subject: string;
      html: string;
    };
  };
  category?: "marketing" | "transactional";
  journey_id?: string | null;
  journey_version?: number | null;
  node_index?: number | null;
  campaign_ref?: string | null;
}

/** message.lifecycle 스트림 payload v1 — 채널·공급자 중립 발송 수명주기 (커넥터·공급자 콜백·SDK 수렴) */
export type LifecycleStatus =
  | "accepted"
  | "sent"
  | "delivered"
  | "opened"
  | "clicked"
  | "failed"
  | "unsubscribed"
  | "bounced";
export type LifecycleFailureClass =
  | "retryable"
  | "rate_limited"
  | "permanent_content"
  | "invalid_target"
  | "credential_auth"
  | "unsupported"
  | "retry_exhausted";
export interface MessageLifecyclePayload {
  message_id: string;
  idempotency_key?: string;
  status: LifecycleStatus;
  occurred_at: string;
  source: "engine" | "connector" | "provider_callback" | "sdk";
  channel: string;
  connector_id: string;
  provider_message_id?: string | null;
  user_id?: string | null;
  endpoint_id?: string | null;
  failure_class?: LifecycleFailureClass | null;
  failure_detail?: string | null;
  fallback_index?: number | null;
  attempt?: number | null;
  cost?: { currency: string; amount: number } | null;
  click_ref?: string | null;
}

function loadSchema(name: string): Record<string, unknown> {
  // dist/index.js 기준 ../schemas — package files에 schemas/ 포함
  const path = join(__dirname, "..", "schemas", name);
  return JSON.parse(readFileSync(path, "utf8")) as Record<string, unknown>;
}

export const envelopeSchema = loadSchema("envelope.schema.json");

/** type별 payload 스키마. 새 메시지 type 추가 시 여기와 schemas/에 함께 등록한다. */
export const payloadSchemas: Partial<Record<MessageType, Record<string, unknown>>> = {
  "ingest.batch": loadSchema("ingest.batch.schema.json"),
  "event.normalized": loadSchema("event.normalized.schema.json"),
  "send.push": loadSchema("send.push.schema.json"),
  "send.email": loadSchema("send.email.schema.json"),
  "journey.enter": loadSchema("journey.entry.schema.json"),
  "send.message": loadSchema("send.message.v1.schema.json"),
  "message.lifecycle": loadSchema("message.lifecycle.v1.schema.json"),
};
