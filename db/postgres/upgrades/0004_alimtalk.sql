-- 카카오 알림톡(P0) — channel_kind enum + 커넥터 배선·발신프로필·템플릿·폴링 접수 테이블.
--
-- 설계 근거:
--  - enum에는 'alimtalk' 하나만 넣는다. 벤더(NHN·알리고·Solapi…) 식별은 channel_connectors.connector_id가
--    하므로, 제3자 벤더가 들어와도 enum 마이그레이션이 필요 없다 (worker internal/channel/alimtalk/vendor.go).
--  - 발신프로필은 credentials가 아니라 별도 테이블이다. credentials는 UNIQUE (app_id, kind)로 앱당 1개인데,
--    알림톡 발신프로필은 앱당 여러 개(브랜드별 채널)여야 한다.
-- ADD VALUE는 트랜잭션 밖에서만 가능 — 마이그레이터는 문 단위 autocommit이라 안전.
ALTER TYPE channel_kind ADD VALUE IF NOT EXISTS 'alimtalk';

-- 앱별 채널 → 커넥터 배선 + 비밀이 아닌 설정(발신번호·기본 발신프로필 등).
-- 비밀은 credentials(봉투 암호화)에 남고, 여기에는 절대 넣지 않는다.
CREATE TABLE IF NOT EXISTS channel_connectors (
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
CREATE TABLE IF NOT EXISTS alimtalk_senders (
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
CREATE INDEX IF NOT EXISTS alimtalk_senders_app_idx ON alimtalk_senders (app_id, status);

-- 승인 템플릿 캐시. 카카오 승인 본문이 단일 출처이므로 우리는 벤더에서 동기화만 하고 편집하지 않는다.
-- content는 알리고처럼 완성 텍스트를 요구하는 벤더를 위한 렌더 원본이다(vendor.go SendRequest.RenderedText).
CREATE TABLE IF NOT EXISTS alimtalk_templates (
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
CREATE INDEX IF NOT EXISTS alimtalk_templates_sender_idx ON alimtalk_templates (sender_id, status);

-- 폴링형 벤더(NHN·알리고)의 미종결 접수. 콜백형은 이 테이블을 쓰지 않는다
-- (manifest.capabilities.lifecycle_mode = polling|both일 때만 적재).
-- PK가 (app_id, message_id)라 같은 발송의 재적재가 자연히 멱등이다.
-- PK가 (tenant_id, message_id)인 이유: 워커가 ON CONFLICT (tenant_id, message_id)로 재적재를
-- 멱등화하고 (tenant_id, message_id)로 백오프·삭제한다(internal/message/worker.go·poller.go).
-- tenants/apps FK는 일부러 두지 않는다 — 폴러가 소유하는 임시 행이고, 테넌트 파기(journey/purge.go)의
-- 삭제 목록에 없어 FK가 있으면 파기를 막는다. 미종결 접수는 폴러가 확정 후 스스로 지운다.
CREATE TABLE IF NOT EXISTS pending_receipts (
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
CREATE INDEX IF NOT EXISTS pending_receipts_due_idx ON pending_receipts (next_poll_at);
