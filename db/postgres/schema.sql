-- Onda — PostgreSQL 선언적 스키마 (Atlas 단일 출처)
-- 원칙: PG = 현재 상태(current state)만. append-only 수집 기록은 ClickHouse (PRD-01 5.2).
-- 격리: 모든 테넌트 데이터 테이블은 tenant_id 컬럼 + 애플리케이션 레벨 강제 (PRD-06 4장).

-- ---------------------------------------------------------------------------
-- ENUM 타입
-- ---------------------------------------------------------------------------
CREATE TYPE member_role AS ENUM ('owner', 'admin', 'editor', 'viewer');
CREATE TYPE member_status AS ENUM ('active', 'invited', 'disabled');
CREATE TYPE api_key_kind AS ENUM ('sdk', 'server');
CREATE TYPE api_key_scope AS ENUM ('full', 'ingest_only');
CREATE TYPE api_key_status AS ENUM ('active', 'rotating', 'revoked');
CREATE TYPE user_status AS ENUM ('active', 'merged', 'deleted');
CREATE TYPE device_platform AS ENUM ('ios', 'android');
CREATE TYPE token_status AS ENUM ('active', 'invalid', 'expired');
CREATE TYPE os_permission AS ENUM ('granted', 'denied', 'undetermined');
CREATE TYPE attr_type AS ENUM ('string', 'number', 'boolean', 'datetime', 'string_array');
-- alimtalk은 값 하나뿐이다 — 벤더 식별은 channel_connectors.connector_id가 한다 (제3자 벤더 = enum 무변경).
CREATE TYPE channel_kind AS ENUM ('push_fcm', 'push_apns', 'email_smtp', 'email_nhn', 'email_resend', 'alimtalk');  -- v1.5: sms
CREATE TYPE credential_status AS ENUM ('unverified', 'verified', 'error');

-- ---------------------------------------------------------------------------
-- 테넌트 · 계정 (DEV-sub-07)
-- ---------------------------------------------------------------------------
CREATE TABLE tenants (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name         text NOT NULL,
  -- 조직 전체 2FA 강제 (PRD-06 2.1, T-5). 켜면 미등록 멤버는 로그인 후 등록 화면으로 강제.
  require_2fa  boolean NOT NULL DEFAULT false,
  -- 삭제 플로우: 요청 → 7일 유예(복구 가능) → 파기 (PRD-06 6장)
  delete_requested_at timestamptz,
  purge_after  timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
-- 기존 설치용(멱등): CREATE TABLE는 기존 tenants에서 스킵되므로 신규 컬럼은 ALTER로 추가.
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS require_2fa boolean NOT NULL DEFAULT false;

CREATE TABLE members (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     uuid NOT NULL REFERENCES tenants(id),
  email         text NOT NULL,
  password_hash text,                        -- Argon2id. OAuth 전용 계정은 NULL
  name          text NOT NULL DEFAULT '',
  role          member_role NOT NULL DEFAULT 'viewer',
  status        member_status NOT NULL DEFAULT 'invited',
  -- TOTP 2FA (PRD-06 2.1) — S4에서 활성화, 스키마는 선행 확정
  totp_secret_enc     bytea,                 -- KMS 암호화
  totp_enabled_at     timestamptz,
  totp_last_counter   bigint,                -- 코드 재사용 방지
  totp_failed_count   int NOT NULL DEFAULT 0,
  totp_locked_until   timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, email)
);
CREATE UNIQUE INDEX members_email_login_idx ON members (lower(email));

-- 백업 코드: 활성화 시 10개 발급, 해시 저장, 1회 사용 (PRD-06 2.1)
CREATE TABLE member_backup_codes (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  uuid NOT NULL REFERENCES tenants(id),
  member_id  uuid NOT NULL REFERENCES members(id) ON DELETE CASCADE,
  code_hash  text NOT NULL,
  used_at    timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX member_backup_codes_member_idx ON member_backup_codes (member_id);

-- DB 세션 + Redis 캐시 (ADR-8). 토큰 원문은 저장하지 않고 해시만.
CREATE TABLE sessions (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  member_id    uuid NOT NULL REFERENCES members(id) ON DELETE CASCADE,
  token_hash   text NOT NULL UNIQUE,         -- SHA-256(세션 토큰)
  ip           inet,
  user_agent   text,
  created_at   timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL,
  revoked_at   timestamptz
);
CREATE INDEX sessions_member_idx ON sessions (member_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

-- ---------------------------------------------------------------------------
-- 앱 · API 키 (PRD-06 3장)
-- ---------------------------------------------------------------------------
CREATE TABLE apps (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  uuid NOT NULL REFERENCES tenants(id),
  name       text NOT NULL,
  timezone   text NOT NULL DEFAULT 'Asia/Seoul',   -- quiet hours 기준 시간대 (PRD-03 6.1)
  -- 발송 정책 (PRD-03 6장). quiet_hours: {enabled, start "HH:MM", end "HH:MM", policy "delay_until_open"|"skip"}
  quiet_hours    jsonb NOT NULL DEFAULT '{"enabled": false, "start": "21:00", "end": "08:00", "policy": "delay_until_open"}',
  -- frequency_cap: {enabled, max_per_24h}
  frequency_cap  jsonb NOT NULL DEFAULT '{"enabled": false, "max_per_24h": 3}',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, name)
);

CREATE TABLE api_keys (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     uuid NOT NULL REFERENCES tenants(id),
  app_id        uuid NOT NULL REFERENCES apps(id),
  kind          api_key_kind NOT NULL,
  scope         api_key_scope NOT NULL DEFAULT 'full',  -- server 키만 의미 있음. sdk는 항상 쓰기 전용
  prefix        text NOT NULL,               -- 'pk_' | 'sk_' + 앞 8자 (콘솔 표시용)
  key_hash      text NOT NULL UNIQUE,        -- SHA-256(키 원문)
  status        api_key_status NOT NULL DEFAULT 'active',
  -- SDK 키 회전: 구키는 status=rotating + grace_expires_at까지 병행 유효 (30일, PRD-06 3장)
  grace_expires_at timestamptz,
  last_used_at  timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  revoked_at    timestamptz
);
CREATE INDEX api_keys_app_idx ON api_keys (app_id, kind, status);

-- ---------------------------------------------------------------------------
-- 발송 크리덴셜 (PRD-04 3장, DEV-sub-04)
-- 봉투 암호화: ciphertext = AES-256-GCM(DEK, payload JSON),
--              dek_wrapped = AES-256-GCM(마스터키, DEK). 마스터키는 KMS/로컬 파일.
-- 복호화는 발송 워커 런타임에서만. 콘솔·API 응답은 항상 마스킹.
-- ---------------------------------------------------------------------------
CREATE TABLE credentials (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES tenants(id),
  app_id           uuid NOT NULL REFERENCES apps(id),
  kind             channel_kind NOT NULL,
  ciphertext       bytea NOT NULL,
  dek_wrapped      bytea NOT NULL,
  status           credential_status NOT NULL DEFAULT 'unverified',
  status_detail    text,                     -- 검증 실패 사유 (콘솔 구체 표시 — U-2)
  last_verified_at timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (app_id, kind)                      -- 앱당 채널 크리덴셜 1개 (교체 = upsert)
);

-- ---------------------------------------------------------------------------
-- 커넥터 배선 · 알림톡 (PRD-04, DEV-sub-04) — upgrades/0004_alimtalk.sql과 동일 정의.
-- ---------------------------------------------------------------------------
-- 앱별 채널 → 커넥터 배선 + 비밀이 아닌 설정(발신번호·기본 발신프로필 등).
-- 비밀은 credentials(봉투 암호화)에만 두고 여기에는 절대 넣지 않는다.
CREATE TABLE channel_connectors (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  app_id       uuid NOT NULL REFERENCES apps(id),
  channel      text NOT NULL,              -- send.message.v1의 channel 값 (예: kakao_alimtalk)
  connector_id text NOT NULL,              -- manifest.id — ^[a-z][a-z0-9_]{1,63}$
  config       jsonb NOT NULL DEFAULT '{}',
  enabled      boolean NOT NULL DEFAULT true,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (app_id, channel)                 -- 채널당 활성 커넥터 1개 (교체 = upsert)
);

-- 알림톡 발신프로필(카카오 채널). 앱당 여러 개 — credentials의 kind당 1개 제약을 여기서 우회한다.
CREATE TABLE alimtalk_senders (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  app_id       uuid NOT NULL REFERENCES apps(id),
  sender_key   text NOT NULL,              -- NHN senderKey / Solapi pfId / 알리고 senderkey
  channel_name text NOT NULL DEFAULT '',   -- 카카오 채널 검색용 ID (@브랜드) — 콘솔 표시용
  status       text NOT NULL DEFAULT 'active',
  is_default   boolean NOT NULL DEFAULT false,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (app_id, sender_key)
);
CREATE INDEX alimtalk_senders_app_idx ON alimtalk_senders (app_id, status);

-- 승인 템플릿 캐시. 카카오 승인 본문이 단일 출처이므로 벤더에서 동기화만 하고 편집하지 않는다.
-- content는 완성 텍스트를 요구하는 벤더(알리고)를 위한 렌더 원본이다.
CREATE TABLE alimtalk_templates (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL REFERENCES tenants(id),
  app_id         uuid NOT NULL REFERENCES apps(id),
  sender_id      uuid NOT NULL REFERENCES alimtalk_senders(id) ON DELETE CASCADE,
  template_code  text NOT NULL,
  name           text NOT NULL DEFAULT '',
  content        text NOT NULL DEFAULT '',
  message_type   text NOT NULL DEFAULT '',  -- BA 기본형 · EX 부가정보형 · AD 광고추가형 · MI 복합형
  emphasize_type text NOT NULL DEFAULT '',  -- NONE · TEXT · IMAGE · ITEM_LIST
  variables      text[] NOT NULL DEFAULT '{}',  -- 치환자 이름만 (#{} 표기 제외)
  buttons        jsonb NOT NULL DEFAULT '[]',
  quick_replies  jsonb NOT NULL DEFAULT '[]',
  status         text NOT NULL DEFAULT 'unknown',      -- Onda 정규화 상태 (approved 등)
  vendor_status  text NOT NULL DEFAULT '',             -- 벤더 원문 상태 코드 (진단용 보존)
  synced_at      timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (app_id, sender_id, template_code)
);
CREATE INDEX alimtalk_templates_sender_idx ON alimtalk_templates (sender_id, status);

-- 폴링형 벤더(NHN·알리고)의 미종결 접수. 콜백형 벤더는 쓰지 않는다
-- (manifest.capabilities.lifecycle_mode = polling|both일 때만 적재).
-- PK가 (app_id, message_id)라 같은 발송의 재적재가 자연히 멱등이다.
-- PK가 (tenant_id, message_id)인 이유: 워커가 ON CONFLICT (tenant_id, message_id)로 재적재를
-- 멱등화하고 (tenant_id, message_id)로 백오프·삭제한다(internal/message/worker.go·poller.go).
-- tenants/apps FK는 일부러 두지 않는다 — 폴러가 소유하는 임시 행이고, 테넌트 파기(journey/purge.go)의
-- 삭제 목록에 없어 FK가 있으면 파기를 막는다. 미종결 접수는 폴러가 확정 후 스스로 지운다.
CREATE TABLE pending_receipts (
  tenant_id           uuid NOT NULL,
  app_id              uuid NOT NULL,
  connector_id        text NOT NULL,       -- 채널 컬럼은 없다 — connector_id로 manifest.channel이 나온다
  message_id          uuid NOT NULL,
  provider_message_id text NOT NULL,       -- 복합키(NHN requestId+recipientSeq)는 벤더가 한 문자열로 인코딩
  attempts            int NOT NULL DEFAULT 0,
  next_poll_at        timestamptz NOT NULL,
  created_at          timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, message_id)
);
-- 폴러의 유일한 조회 경로: WHERE next_poll_at <= now() ORDER BY next_poll_at LIMIT n.
CREATE INDEX pending_receipts_due_idx ON pending_receipts (next_poll_at);

-- ---------------------------------------------------------------------------
-- 유저 · 디바이스 (PRD-01 5.1)
-- ---------------------------------------------------------------------------
CREATE TABLE users (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     uuid NOT NULL REFERENCES tenants(id),
  app_id        uuid NOT NULL REFERENCES apps(id),
  external_id   text,
  anon_id       uuid,
  status        user_status NOT NULL DEFAULT 'active',
  merged_into   uuid REFERENCES users(id),   -- tombstone → 승계 프로필
  std_attrs     jsonb NOT NULL DEFAULT '{}',
  custom_attrs  jsonb NOT NULL DEFAULT '{}',
  subscriptions jsonb NOT NULL DEFAULT '{}', -- {push: opted_in|unsubscribed, ...채널별}
  last_seen_at  timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (app_id, external_id),
  UNIQUE (app_id, anon_id)
);
CREATE INDEX users_custom_attrs_idx ON users USING gin (custom_attrs);
CREATE INDEX users_app_status_idx ON users (app_id, status);

CREATE TABLE devices (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL REFERENCES tenants(id),
  app_id         uuid NOT NULL REFERENCES apps(id),
  user_id        uuid NOT NULL REFERENCES users(id), -- 현재 귀속 유저 (재로그인 시 이관)
  platform       device_platform NOT NULL,
  push_token     text,
  token_status   token_status NOT NULL DEFAULT 'active',
  os_permission  os_permission NOT NULL DEFAULT 'undetermined',
  device_meta    jsonb NOT NULL DEFAULT '{}',   -- 모델, OS 버전, 앱 버전, locale
  last_active_at timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (app_id, push_token)                   -- 토큰 재등록 시 소유 이전 (PRD-04 4.4)
);
CREATE INDEX devices_user_idx ON devices (user_id);

-- ---------------------------------------------------------------------------
-- 속성 사전 · 병합 매핑 (DEV-sub-01)
-- ---------------------------------------------------------------------------
CREATE TABLE attribute_registry (
  tenant_id     uuid NOT NULL REFERENCES tenants(id),
  app_id        uuid NOT NULL REFERENCES apps(id),
  key           text NOT NULL,
  type          attr_type NOT NULL,
  first_seen_at timestamptz NOT NULL DEFAULT now(),
  last_seen_at  timestamptz NOT NULL DEFAULT now(),
  seg_ref_count int NOT NULL DEFAULT 0,          -- 참조 중 세그먼트 수 (삭제 확인용, PRD-02 6장)
  PRIMARY KEY (app_id, key)
);

-- ---------------------------------------------------------------------------
-- 세그먼트 (PRD-02) — 정의(DSL)는 jsonb, 멤버십은 저장하지 않음(발송 시 스냅샷)
-- ---------------------------------------------------------------------------
CREATE TYPE segment_status AS ENUM ('active', 'broken');
CREATE TYPE journey_status AS ENUM ('draft', 'active', 'paused', 'archived');
CREATE TYPE journey_state_status AS ENUM ('active', 'waiting', 'claimed', 'completed', 'exited', 'failed');
CREATE TYPE message_category AS ENUM ('marketing', 'transactional');

-- 이메일 HTML 템플릿 (개인화 {{ }} 변수 지원). 발송/미리보기 공통 소스.
CREATE TABLE email_templates (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  app_id       uuid NOT NULL REFERENCES apps(id),
  name         text NOT NULL,
  subject      text NOT NULL,              -- {{ }} 개인화 가능
  html         text NOT NULL,              -- {{ }} 개인화 가능
  created_by   uuid REFERENCES members(id),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (app_id, name)
);

CREATE TABLE segments (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  app_id       uuid NOT NULL REFERENCES apps(id),
  name         text NOT NULL,
  definition   jsonb NOT NULL,              -- DSL (PRD-02 2.1)
  status       segment_status NOT NULL DEFAULT 'active',
  status_detail text,                        -- broken 사유 (삭제된 속성 참조 등)
  last_count       int,                      -- 최근 정기 평가 카운트 (통계용)
  last_evaluated_at timestamptz,
  created_by   uuid REFERENCES members(id),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (app_id, name)
);
CREATE INDEX segments_app_idx ON segments (app_id, status);

-- ---------------------------------------------------------------------------
-- 오케스트레이션 (PRD-03) — 단발 캠페인 = 1노드 저니로 통합 모델링
-- ---------------------------------------------------------------------------
CREATE TABLE journeys (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  app_id       uuid NOT NULL REFERENCES apps(id),
  name         text NOT NULL,
  status       journey_status NOT NULL DEFAULT 'draft',
  category     message_category NOT NULL DEFAULT 'marketing',
  -- 편집 중(draft) 정의. 활성화 시 journey_versions로 불변 스냅샷 (2.2)
  draft_definition jsonb NOT NULL DEFAULT '{}',
  active_version   int,               -- 현재 활성 버전 (journey_versions.version)
  created_by   uuid REFERENCES members(id),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (app_id, name)
);

-- 활성화 시점마다 증가하는 불변 버전 스냅샷 (진행 중 유저는 진입 버전으로 완주 — 2.2)
CREATE TABLE journey_versions (
  journey_id   uuid NOT NULL REFERENCES journeys(id),
  version      int NOT NULL,
  definition   jsonb NOT NULL,        -- 불변
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (journey_id, version)
);

CREATE TABLE journey_states (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL REFERENCES tenants(id),
  app_id         uuid NOT NULL REFERENCES apps(id),
  journey_id     uuid NOT NULL,
  journey_version int NOT NULL,
  user_id        uuid NOT NULL,
  current_node   int NOT NULL DEFAULT 0,
  status         journey_state_status NOT NULL DEFAULT 'active',
  next_wake_at   timestamptz,         -- delay 노드 기상 시각 (즉시 실행이면 NULL/과거)
  claimed_by     text,
  claimed_at     timestamptz,
  claim_token    uuid,                -- v2 stale-worker fencing token
  entry_id       text,                -- v2 stable source admission identity; v1 stays NULL
  entry_seq      bigint,              -- receipt sequence at v2 admission
  fail_reason    text,
  entered_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now()
);
-- 동일 유저는 동일 저니에 동시 1개 인스턴스만 (진행 중 = active|waiting|claimed)
CREATE UNIQUE INDEX journey_states_active_uniq
  ON journey_states (journey_id, user_id)
  WHERE status IN ('active', 'waiting', 'claimed');
-- 스케줄러 클레임 인덱스: 기상 시각 도래한 대기 상태
CREATE INDEX journey_states_wake_idx
  ON journey_states (next_wake_at)
  WHERE status IN ('active', 'waiting');
CREATE UNIQUE INDEX journey_states_entry_uniq
  ON journey_states (tenant_id, app_id, journey_id, journey_version, user_id, entry_id)
  WHERE entry_id IS NOT NULL;

-- One node visit per execution: v2 is a DAG, with exclusive (not parallel) paths.
CREATE TABLE journey_node_executions (
  state_id        uuid NOT NULL REFERENCES journey_states(id) ON DELETE CASCADE,
  node_id         text NOT NULL,
  node_index      int NOT NULL CHECK (node_index BETWEEN 0 AND 65535),
  tenant_id       uuid NOT NULL REFERENCES tenants(id),
  app_id          uuid NOT NULL REFERENCES apps(id),
  journey_id      uuid NOT NULL,
  journey_version int NOT NULL,
  user_id         uuid NOT NULL,
  status          text NOT NULL CHECK (status IN ('arrived', 'waiting', 'retrying', 'resolved', 'failed', 'exited')),
  arrived_at      timestamptz NOT NULL,
  resolved_at     timestamptz,
  output_port     text,
  context         jsonb NOT NULL DEFAULT '{}',
  wait_event      text,
  after_seq       bigint,
  deadline        timestamptz,
  matched_insert_id uuid,
  retry_count     int NOT NULL DEFAULT 0,
  failure_reason  text,
  updated_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (state_id, node_index),
  UNIQUE (state_id, node_id)
);
CREATE INDEX journey_node_executions_wait_idx
  ON journey_node_executions (tenant_id, app_id, user_id, wait_event, after_seq, deadline)
  WHERE status = 'waiting' AND wait_event IS NOT NULL;
CREATE INDEX journey_node_executions_report_idx
  ON journey_node_executions (tenant_id, app_id, journey_id, journey_version, node_id, status);

-- outbox: 상태 전이와 발송 잡 발행의 원자성 (4.3 정확히-한-번의 구현체)
CREATE TABLE journey_outbox (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id    uuid NOT NULL,
  app_id       uuid NOT NULL,
  stream       text NOT NULL,         -- 발행 대상 스트림 (send.push 등)
  idempotency_key text NOT NULL,      -- (journey_id, version, user_id, node_index, device_id)
  payload      jsonb NOT NULL,        -- envelope payload
  created_at   timestamptz NOT NULL DEFAULT now(),
  published_at timestamptz
);
CREATE INDEX journey_outbox_unpublished_idx
  ON journey_outbox (id) WHERE published_at IS NULL;
-- Scope uniqueness to new keys; legacy rows and in-flight keys are untouched.
CREATE UNIQUE INDEX journey_outbox_v2_dedup_idx
  ON journey_outbox (tenant_id, app_id, idempotency_key)
  WHERE idempotency_key LIKE 'v2:%' OR idempotency_key LIKE 'event.%';

-- Receipt ordering is independent of Redis delivery / device time.
CREATE TABLE event_customer_cursors (
  tenant_id  uuid NOT NULL REFERENCES tenants(id),
  app_id     uuid NOT NULL REFERENCES apps(id),
  user_id    uuid NOT NULL REFERENCES users(id),
  last_seq   bigint NOT NULL DEFAULT 0,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, app_id, user_id)
);
CREATE TABLE event_receipts (
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  app_id      uuid NOT NULL REFERENCES apps(id),
  insert_id   uuid NOT NULL,
  user_id     uuid NOT NULL REFERENCES users(id),
  event_name  text NOT NULL,
  properties  jsonb,
  device      jsonb,
  client_ts   timestamptz,
  received_at timestamptz NOT NULL,
  receipt_seq bigint NOT NULL,
  projected_at timestamptz,
  matched_at   timestamptz,
  purged_at    timestamptz,
  PRIMARY KEY (tenant_id, app_id, insert_id),
  UNIQUE (tenant_id, app_id, user_id, receipt_seq)
);
CREATE INDEX event_receipts_wait_idx
  ON event_receipts (tenant_id, app_id, user_id, event_name, receipt_seq, received_at);
CREATE INDEX event_receipts_projection_idx
  ON event_receipts (tenant_id, app_id, user_id, receipt_seq) WHERE projected_at IS NULL;
CREATE INDEX event_receipts_unmatched_idx
  ON event_receipts (received_at) WHERE matched_at IS NULL;

CREATE TABLE user_merges (
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  app_id       uuid NOT NULL REFERENCES apps(id),
  from_user_id uuid PRIMARY KEY,               -- anon(tombstone) 프로필
  to_user_id   uuid NOT NULL REFERENCES users(id),
  merged_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX user_merges_to_idx ON user_merges (to_user_id);

-- 감사 로그 (PRD-06 · DEV-sub-07 T-9) — 크리덴셜 변경·2FA 리셋·키 작업·속성 편집 등 민감 행위.
-- actor_email은 스냅샷(멤버 삭제돼도 보존). append-only 성격.
CREATE TABLE audit_logs (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenants(id),
  actor_member_id uuid REFERENCES members(id) ON DELETE SET NULL, -- 시스템/키 행위는 NULL 가능
  actor_email     text,                     -- 스냅샷
  action          text NOT NULL,            -- 예: credential.upsert · member.totp_reset · apikey.rotate
  target_type     text,                     -- credential | member | apikey | attribute | app ...
  target_id       text,                     -- 대상 식별자(문자열)
  detail          jsonb NOT NULL DEFAULT '{}',
  ip              inet,
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_logs_tenant_idx ON audit_logs (tenant_id, created_at DESC);

-- 테넌트 파기 추적 (T-10) — tenants 행 삭제 후에도 ClickHouse 정리 재시도·완료를 추적하는
-- 시스템 테이블. tenants FK 없음(대상 행이 이미 삭제됨). append-only, ch_purged로 완료 추적.
CREATE TABLE tenant_purges (
  tenant_id     uuid PRIMARY KEY,
  pg_purged_at  timestamptz NOT NULL DEFAULT now(),
  ch_purged     boolean NOT NULL DEFAULT false,
  ch_attempts   int NOT NULL DEFAULT 0,
  ch_last_error text,
  ch_purged_at  timestamptz
);
CREATE INDEX tenant_purges_pending_idx ON tenant_purges (ch_purged, pg_purged_at) WHERE ch_purged = false;

-- 발송 DLQ (R-02) — 재시도 상한 소진된 send.push 원본. 운영 도구(cmd/dlq)로 조회·재처리(replay).
CREATE TABLE send_dlq (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenants(id),
  app_id          uuid NOT NULL REFERENCES apps(id),
  idempotency_key text NOT NULL,
  message_id      uuid,
  failure_class   text NOT NULL,
  failure_detail  text,
  attempts        int NOT NULL,
  envelope        jsonb NOT NULL,   -- 원본 libqueue Envelope (replay용)
  created_at      timestamptz NOT NULL DEFAULT now(),
  replayed_at     timestamptz,
  UNIQUE (tenant_id, idempotency_key)
);
CREATE INDEX send_dlq_tenant_idx ON send_dlq (tenant_id, created_at DESC);
