// 알림톡 P0 E2E — 저니 알림톡 노드 → send.message.v1 → mock 벤더 → message.lifecycle → 리포트.
//
// 전제:
//   1) 워커를 GO_TAGS=onda_mock으로 빌드하고 mock 매니페스트를 커넥터 디렉터리에 배치할 것
//      (deploy/connectors/alimtalk_mock.json). 목은 기본 빌드에 들어가지 않는다.
//   2) docker compose --profile full --profile app 기동.
// 사용: API_URL=http://localhost:18085 node tests/e2e/alimtalk.mjs
import { randomUUID } from "node:crypto";
import { execFileSync } from "node:child_process";

const BASE = process.env.API_URL ?? "http://localhost:8080";
const CH = process.env.CLICKHOUSE_HTTP ?? "http://localhost:8123";
const COMPOSE = process.env.COMPOSE_FILE ?? "deploy/compose.yaml";
const CONNECTOR = "alimtalk_mock";
const SENDER_KEY = "a".repeat(40);

// mock 조종 규약 — 수신번호 끝 4자리가 결과를 정한다 (mock 패키지 주석과 동일).
const N = (suffix) => `+821000${suffix}`;
const DELIVERED = N("0001");
const INVALID = N("0002");
const RATELIMIT = N("0429");

let failures = 0;
function ok(cond, msg) {
  if (cond) { console.log("✓", msg); return true; }
  console.error("✗", msg); failures++; return false;
}

async function req(method, path, { cookie, body } = {}) {
  const res = await fetch(`${BASE}${path}`, {
    method,
    headers: { ...(body ? { "content-type": "application/json" } : {}), ...(cookie ? { cookie } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let json; try { json = text ? JSON.parse(text) : null; } catch { json = text; }
  return { status: res.status, json, setCookie: res.headers.get("set-cookie") };
}

async function ch(sql) {
  const res = await fetch(`${CH}/?database=onda`, {
    method: "POST",
    headers: { authorization: "Basic " + Buffer.from("onda:onda").toString("base64") },
    body: sql,
  });
  const t = await res.text();
  if (!res.ok) throw new Error(`CH ${res.status}: ${t}`);
  return t.trim();
}

function psql(sql) {
  return execFileSync("docker", ["compose", "-f", COMPOSE, "exec", "-T", "postgres",
    "psql", "-U", "onda", "-d", "onda", "-tAc", sql], { encoding: "utf8" }).trim();
}

async function until(fn, label, tries = 40) {
  for (let i = 0; i < tries; i++) {
    const v = await fn();
    if (v) return v;
    await new Promise((r) => setTimeout(r, 1000));
  }
  throw new Error(`timeout: ${label}`);
}

const main = async () => {
  // ── 1. 테넌트·앱
  const email = `alimtalk-e2e-${Date.now()}@example.com`;
  const s = await req("POST", "/v1/auth/signup", {
    body: { email, password: "password123", name: "알림톡 E2E", tenant_name: "알림톡 E2E" },
  });
  ok(s.status === 201 || s.status === 200, `signup ${s.status}`);
  const cookie = s.setCookie.split(";")[0];
  const appId = s.json.app_id;
  const tenantId = s.json.tenant_id;

  // ── 2. 알림톡 크리덴셜 + 벤더 배선 (콘솔의 "알림톡 설정 → 벤더 선택" 흐름)
  const cred = await req("PUT", `/v1/apps/${appId}/credentials`, {
    cookie,
    body: { kind: "alimtalk", connector_id: CONNECTOR, api_key: "mock_key", sender_key: SENDER_KEY },
  });
  ok(cred.status === 200 || cred.status === 201, `알림톡 크리덴셜 등록 ${cred.status} ${JSON.stringify(cred.json)}`);

  const wiring = await req("PUT", `/v1/apps/${appId}/channels/kakao_alimtalk/connector`, {
    cookie,
    body: { connector_id: CONNECTOR, config: { sender_key: SENDER_KEY } },
  });
  ok(wiring.status === 200 || wiring.status === 201, `벤더 배선 ${wiring.status} ${JSON.stringify(wiring.json)}`);

  // 크리덴셜 검증 — 워커의 verifier가 mock 벤더로 실검증한다.
  const verified = await until(async () => {
    const r = await req("GET", `/v1/apps/${appId}/credentials`, { cookie });
    const c = (r.json?.credentials ?? []).find((x) => x.kind === "alimtalk");
    return c && c.status !== "unverified" ? c : null;
  }, "알림톡 크리덴셜 검증");
  ok(verified.status === "verified", `크리덴셜 검증 완료 (got ${verified.status} ${verified.status_detail ?? ""})`);

  // ── 3. 발신프로필 + 승인 템플릿 (P0에서는 동기화가 없어 직접 넣는다)
  const senderRes = await req("POST", `/v1/apps/${appId}/alimtalk/senders`, {
    cookie,
    body: { sender_key: SENDER_KEY, channel_name: "@온다샵", is_default: true },
  });
  ok(senderRes.status === 201 || senderRes.status === 200, `발신프로필 등록 ${senderRes.status} ${JSON.stringify(senderRes.json)}`);
  const senderId = senderRes.json?.id ?? psql(
    `SELECT id FROM alimtalk_senders WHERE app_id='${appId}' AND sender_key='${SENDER_KEY}'`);

  // 정보성(BA)과 광고성(AD) 두 템플릿 — 정책 분기를 검증하기 위함.
  // 코드·본문은 mock 벤더의 픽스처와 일치해야 한다. 실제 딜러사도 자기에게 등록된 승인 템플릿만 받는다.
  const info = "ONDA_ORDER_01";
  const promo = "ONDA_PROMO_01";
  const infoBody = "#{고객명}님, 주문 #{주문번호}이 정상 접수되었습니다.\n결제금액: #{결제금액}원\n\n주문 상세는 아래 버튼에서 확인하실 수 있습니다.";
  const promoBody = "(광고) #{고객명}님께 #{쿠폰명} 쿠폰을 드립니다.\n사용기한: #{사용기한}\n\n무료수신거부 080-000-0000";
  psql(`INSERT INTO alimtalk_templates
    (tenant_id, app_id, sender_id, template_code, name, content, message_type, emphasize_type, variables, status)
    VALUES
    ('${tenantId}','${appId}','${senderId}','${info}','주문 접수',
     $body$${infoBody}$body$,'BA','NONE',ARRAY['고객명','주문번호','결제금액'],'approved'),
    ('${tenantId}','${appId}','${senderId}','${promo}','쿠폰 안내',
     $body$${promoBody}$body$,'AD','NONE',ARRAY['고객명','쿠폰명','사용기한'],'approved')
    ON CONFLICT DO NOTHING`);
  ok(psql(`SELECT count(*) FROM alimtalk_templates WHERE app_id='${appId}'`) === "2", "승인 템플릿 2건");

  const list = await req("GET", `/v1/apps/${appId}/alimtalk/templates?sender_id=${senderId}`, { cookie });
  ok(list.status === 200 && (list.json?.templates ?? []).length === 2, `템플릿 목록 API ${list.status}`);

  // ── 4. 저니 — 알림톡 노드
  const journeyRes = await req("POST", `/v1/apps/${appId}/journeys`, {
    cookie,
    body: {
      name: "주문 접수 알림톡",
      definition: {
        entry: { type: "trigger", trigger_event: "order_placed" },
        nodes: [{
          type: "message",
          alimtalk: {
            sender_id: senderId, template_code: info,
            variables: { "고객명": "{{name}}", "주문번호": "{{order_no}}", "결제금액": "{{amount}}" },
          },
        }],
        exit: {},
        settings: { category: "transactional", reentry: "always" },
      },
    },
  });
  ok(journeyRes.status === 201, `알림톡 저니 생성 ${journeyRes.status} ${JSON.stringify(journeyRes.json)}`);
  const journeyId = journeyRes.json?.id;

  // 채널 정확히 하나 규칙 — 푸시와 알림톡을 함께 주면 거절돼야 한다.
  const bad = await req("POST", `/v1/apps/${appId}/journeys`, {
    cookie,
    body: {
      name: "잘못된 노드 " + randomUUID().slice(0, 8),
      definition: {
        entry: { type: "trigger", trigger_event: "x" },
        nodes: [{
          type: "message",
          push: { title: "a", body: "b" },
          alimtalk: { sender_id: senderId, template_code: info },
        }],
        exit: {}, settings: { category: "transactional", reentry: "never" },
      },
    },
  });
  ok(bad.status === 400, `푸시+알림톡 동시 지정 거절 ${bad.status}`);

  const activated = await req("POST", `/v1/apps/${appId}/journeys/${journeyId}/activate`, {
    cookie, body: { revision: journeyRes.json.revision },
  });
  ok(activated.status === 200 || activated.status === 201, `저니 활성화 ${activated.status} ${JSON.stringify(activated.json)}`);

  // ── 5. 대상 유저 3명 (도달 / 미가입 / 레이트리밋)
  const keys = await req("GET", `/v1/apps/${appId}/keys`, { cookie });
  const sdkKey = process.env.SDK_KEY ?? keys.json?.keys?.find((k) => k.kind === "sdk")?.plaintext;
  const users = [
    { ext: "u-delivered", phone: DELIVERED },
    { ext: "u-invalid", phone: INVALID },
    { ext: "u-ratelimit", phone: RATELIMIT },
  ];
  for (const u of users) {
    psql(`INSERT INTO users (tenant_id, app_id, external_id, std_attrs, custom_attrs, subscriptions, status)
          VALUES ('${tenantId}','${appId}','${u.ext}',
                  '{"phone":"${u.phone}","name":"온다","order_no":"A-1","amount":"12000","coupon":"가을맞이","expires":"2026-12-31"}'::jsonb,'{}'::jsonb,'{}'::jsonb,'active')
          ON CONFLICT DO NOTHING`);
  }
  ok(psql(`SELECT count(*) FROM users WHERE app_id='${appId}'`) === "3", "대상 유저 3명");
  if (!sdkKey) console.log("  (SDK 키 평문 없음 — 저니 진입은 DB 직접 삽입으로 대신한다)");

  // 저니 진입 — 트리거 이벤트 대신 상태를 직접 만들어 스케줄러가 집게 한다.
  const version = Number(psql(`SELECT active_version FROM journeys WHERE id='${journeyId}'`));
  for (const u of users) {
    const uid = psql(`SELECT id FROM users WHERE app_id='${appId}' AND external_id='${u.ext}'`);
    psql(`INSERT INTO journey_states
      (tenant_id, app_id, journey_id, journey_version, user_id, current_node, status, next_wake_at)
      VALUES ('${tenantId}','${appId}','${journeyId}',${version},'${uid}',0,'active', now())
      ON CONFLICT DO NOTHING`);
  }
  ok(psql(`SELECT count(*) FROM journey_states WHERE journey_id='${journeyId}'`) === "3", "저니 진입 3건");

  // ── 6. 발송 결과
  const sent = await until(async () => {
    const n = await ch(`SELECT count() FROM message_log WHERE tenant_id='${tenantId}' AND app_id='${appId}' AND channel='kakao_alimtalk'`);
    return Number(n) >= 2 ? n : null;
  }, "message_log 알림톡 행");
  console.log("  message_log 행:", sent);
  const rows = await ch(`SELECT status, failure_class, provider_message_id FROM message_log
    WHERE tenant_id='${tenantId}' AND app_id='${appId}' AND channel='kakao_alimtalk' ORDER BY status FORMAT TSV`);
  console.log(rows.split("\n").map((l) => "  " + l).join("\n"));
  if (!rows.includes("sent")) {
    // 가장 흔한 원인: 아래 본문이 mock 픽스처(alimtalk/mock/templates.go)와 어긋났다.
    // 목은 자기 픽스처로 필수 치환자를 도출하므로 한 글자만 달라도 발송 전에 거절된다.
    const why = await ch(`SELECT DISTINCT failure_detail FROM message_log
      WHERE tenant_id='${tenantId}' AND app_id='${appId}' AND channel='kakao_alimtalk' FORMAT TSV`);
    console.error("  실패 사유:", why.replace(/\n/g, " | "));
    if (/치환자|승인된 템플릿/.test(why)) {
      console.error("  → 이 E2E의 infoBody/promoBody가 mock 픽스처(apps/worker/internal/channel/alimtalk/mock/templates.go)와");
      console.error("    일치하는지 확인하세요. 목은 자기 픽스처 본문에서 필수 치환자를 도출합니다.");
    }
  }
  ok(rows.includes("sent"), "성공 발송 기록");
  ok(/mock_/.test(rows), "provider_message_id 기록 (공급자 접수 식별자)");

  // ── 7. 수명주기 — 워커가 accepted/sent/failed를 발행한다
  const lc = await until(async () => {
    const n = await ch(`SELECT count() FROM message_lifecycle FINAL WHERE tenant_id='${tenantId}' AND app_id='${appId}'`);
    return Number(n) >= 2 ? n : null;
  }, "message_lifecycle 행");
  console.log("  message_lifecycle 행:", lc);
  const lcRows = await ch(`SELECT status, source, connector_id, channel FROM message_lifecycle FINAL
    WHERE tenant_id='${tenantId}' AND app_id='${appId}' ORDER BY status FORMAT TSV`);
  console.log(lcRows.split("\n").map((l) => "  " + l).join("\n"));
  ok(lcRows.includes(CONNECTOR), "lifecycle에 커넥터 id 기록");
  ok(lcRows.includes("kakao_alimtalk"), "lifecycle에 채널 기록");
  ok(lcRows.includes("sent"), "lifecycle sent 발행");

  // ── 8. 정책 — 광고성 템플릿은 야간에 막혀야 한다
  //     앱 조용시간을 지금을 포함하도록 설정하고 광고 저니를 돌린다.
  const nowH = new Date().getUTCHours();
  const start = `${String((nowH + 23) % 24).padStart(2, "0")}:00`;
  const end = `${String((nowH + 2) % 24).padStart(2, "0")}:00`;
  const settings = await req("PUT", `/v1/apps/${appId}/settings`, {
    cookie, body: { timezone: "UTC", quiet_hours: { enabled: true, start, end, policy: "skip" },
                    frequency_cap: { enabled: false, max_per_24h: 1 } },
  });
  ok(settings.status === 200, `조용시간 설정 ${settings.status} ${JSON.stringify(settings.json)}`);
  ok(psql(`SELECT quiet_hours->>'enabled' FROM apps WHERE id='${appId}'`) === "true", "조용시간 활성 반영");
  const promoJourney = await req("POST", `/v1/apps/${appId}/journeys`, {
    cookie,
    body: {
      name: "쿠폰 알림톡",
      definition: {
        entry: { type: "trigger", trigger_event: "promo" },
        nodes: [{ type: "message", alimtalk: { sender_id: senderId, template_code: promo,
          variables: { "고객명": "{{name}}", "쿠폰명": "{{coupon}}", "사용기한": "{{expires}}" } } }],
        exit: {},
        // 저니는 transactional로 선언했지만 템플릿이 광고성(AD)이므로 야간 제한이 적용돼야 한다.
        settings: { category: "transactional", reentry: "always" },
      },
    },
  });
  ok(promoJourney.status === 201, `광고 저니 생성 ${promoJourney.status}`);
  await req("POST", `/v1/apps/${appId}/journeys/${promoJourney.json.id}/activate`, {
    cookie, body: { revision: promoJourney.json.revision },
  });
  const pv = Number(psql(`SELECT active_version FROM journeys WHERE id='${promoJourney.json.id}'`));
  const uid = psql(`SELECT id FROM users WHERE app_id='${appId}' AND external_id='u-delivered'`);
  psql(`INSERT INTO journey_states
    (tenant_id, app_id, journey_id, journey_version, user_id, current_node, status, next_wake_at)
    VALUES ('${tenantId}','${appId}','${promoJourney.json.id}',${pv},'${uid}',0,'active', now())
    ON CONFLICT DO NOTHING`);

  const skipped = await until(async () => {
    const n = await ch(`SELECT count() FROM message_log WHERE tenant_id='${tenantId}' AND app_id='${appId}'
                        AND journey_id='${promoJourney.json.id}' AND status='skipped_quiet_hours'`);
    return Number(n) >= 1 ? n : null;
  }, "광고성 템플릿 야간 차단");
  ok(Number(skipped) >= 1, "광고성(AD) 템플릿은 저니가 transactional이어도 야간에 차단된다");
  const detail = await ch(`SELECT failure_detail FROM message_log WHERE tenant_id='${tenantId}'
    AND journey_id='${promoJourney.json.id}' AND status='skipped_quiet_hours' LIMIT 1`);
  ok(detail.includes("광고"), `생략 사유 기록 (${detail})`);

  console.log(failures === 0 ? "\nALIMTALK E2E: PASS" : `\nALIMTALK E2E: FAIL (${failures}건)`);
  process.exit(failures === 0 ? 0 : 1);
};

main().catch((e) => { console.error(e.message); process.exit(1); });
