import "reflect-metadata";
import { randomUUID } from "node:crypto";
import { describe, expect, it, vi } from "vitest";
import type { Pool } from "pg";
import type { QueueProducer } from "@onda/libqueue";
import { STREAMS } from "@onda/queue-schemas";
import type { AuditService } from "../audit/audit.service";
import type { SessionRequest } from "../auth/session.guard";
import { ChannelConnectorsController } from "./connectors.controller";
import { AlimtalkSendersController } from "./senders.controller";
import { AlimtalkTemplatesController } from "./templates.controller";

const tenantId = randomUUID(), appId = randomUUID(), otherAppId = randomUUID();

const request = (role: string): SessionRequest =>
  ({
    ip: "127.0.0.1",
    member: {
      tenantId,
      memberId: randomUUID(),
      role,
      name: "QA",
      email: "alimtalk-qa@example.test",
      totpEnabled: false,
      requires2fa: false,
    },
  }) as unknown as SessionRequest;

/**
 * 가짜 PG. 쿼리 텍스트로 응답을 고르되, 테넌트 격리를 실제로 흉내 낸다:
 * apps 조회는 (id, tenant_id)가 모두 맞아야 행을 준다.
 */
function fakePg(handlers: Array<[RegExp, (params: unknown[]) => { rows: unknown[]; rowCount?: number }]>) {
  const calls: Array<{ sql: string; params: unknown[] }> = [];
  const query = vi.fn(async (sql: string, params: unknown[] = []) => {
    calls.push({ sql, params });
    for (const [re, handler] of handlers) {
      if (re.test(sql)) {
        const r = handler(params);
        return { rows: r.rows, rowCount: r.rowCount ?? r.rows.length };
      }
    }
    return { rows: [], rowCount: 0 };
  });
  const client = { query, release: vi.fn() };
  const pg = { query, connect: vi.fn(async () => client) } as unknown as Pool;
  return { pg, query, calls, client };
}

/** 실제 앱 소유 검사와 같은 규칙: id와 tenant_id가 모두 일치할 때만 1행. */
const appsHandler: [RegExp, (p: unknown[]) => { rows: unknown[] }] = [
  /FROM apps WHERE id = \$1 AND tenant_id = \$2/,
  ([id, tid]) => ({ rows: id === appId && tid === tenantId ? [{ "?column?": 1 }] : [] }),
];

const audit = { recordAs: vi.fn(async () => undefined) } as unknown as AuditService;

describe("ChannelConnectorsController", () => {
  const connector = { id: randomUUID(), channel: "kakao_alimtalk", connector_id: "kakao_alimtalk_nhn", config: {}, enabled: true };

  it("PUT은 배선을 upsert하고 tenant_id로 스코프하며 감사 로그를 남긴다", async () => {
    const { pg, calls } = fakePg([appsHandler, [/INSERT INTO channel_connectors/, () => ({ rows: [connector] })]]);
    const ctl = new ChannelConnectorsController(pg, audit);
    const req = request("admin");
    await expect(
      ctl.upsert(appId, "kakao_alimtalk", { connector_id: "kakao_alimtalk_nhn", config: { sender_no: "0212345678" } }, req),
    ).resolves.toEqual(connector);
    const insert = calls.find((c) => /INSERT INTO channel_connectors/.test(c.sql))!;
    expect(insert.params[0]).toBe(tenantId);
    expect(insert.sql).toContain("tenant_id");
    expect(audit.recordAs).toHaveBeenCalledWith(
      req.member, "127.0.0.1", "channel_connector.upsert",
      expect.objectContaining({ targetType: "channel_connector" }),
    );
  });

  it("editor는 배선을 바꿀 수 없다 (403) — 크리덴셜과 같은 민감도", async () => {
    const { pg } = fakePg([appsHandler]);
    const ctl = new ChannelConnectorsController(pg, audit);
    await expect(
      ctl.upsert(appId, "kakao_alimtalk", { connector_id: "kakao_alimtalk_nhn" }, request("editor")),
    ).rejects.toMatchObject({ status: 403 });
  });

  it("타 테넌트 앱은 404 (존재 여부 비노출) — 역할 검사보다 먼저", async () => {
    const { pg, calls } = fakePg([appsHandler]);
    const ctl = new ChannelConnectorsController(pg, audit);
    await expect(ctl.get(otherAppId, "kakao_alimtalk", request("owner"))).rejects.toMatchObject({ status: 404 });
    await expect(
      ctl.upsert(otherAppId, "kakao_alimtalk", { connector_id: "kakao_alimtalk_nhn" }, request("admin")),
    ).rejects.toMatchObject({ status: 404 });
    expect(calls.some((c) => /INSERT INTO channel_connectors/.test(c.sql))).toBe(false);
  });

  it("미배선 채널 조회는 404", async () => {
    const { pg } = fakePg([appsHandler]);
    const ctl = new ChannelConnectorsController(pg, audit);
    await expect(ctl.get(appId, "kakao_alimtalk", request("owner"))).rejects.toMatchObject({ status: 404 });
  });

  it("잘못된 connector_id 형식은 400", async () => {
    const { pg } = fakePg([appsHandler]);
    const ctl = new ChannelConnectorsController(pg, audit);
    for (const bad of ["NHN", "9nhn", "a", "nhn-cloud"]) {
      await expect(
        ctl.upsert(appId, "kakao_alimtalk", { connector_id: bad }, request("owner")),
      ).rejects.toMatchObject({ status: 400 });
    }
  });
});

describe("AlimtalkSendersController", () => {
  const sender = { id: randomUUID(), sender_key: "sk-1", channel_name: "@onda", status: "active", is_default: true };

  it("목록은 tenant_id + app_id로 스코프한다", async () => {
    const { pg, calls } = fakePg([appsHandler, [/FROM alimtalk_senders/, () => ({ rows: [sender] })]]);
    const ctl = new AlimtalkSendersController(pg, audit);
    await expect(ctl.list(appId, request("viewer"))).resolves.toEqual({ senders: [sender] });
    const select = calls.find((c) => /FROM alimtalk_senders/.test(c.sql))!;
    expect(select.sql).toContain("tenant_id = $1");
    expect(select.params).toEqual([tenantId, appId]);
  });

  it("기본 발신프로필 지정은 같은 트랜잭션에서 기존 기본을 먼저 내린다", async () => {
    const { pg, calls } = fakePg([appsHandler, [/INSERT INTO alimtalk_senders/, () => ({ rows: [sender] })]]);
    const ctl = new AlimtalkSendersController(pg, audit);
    await ctl.create(appId, { sender_key: "sk-1", is_default: true }, request("editor"));
    const sqls = calls.map((c) => c.sql.replace(/\s+/g, " ").trim());
    const begin = sqls.indexOf("BEGIN");
    const clear = sqls.findIndex((s) => /UPDATE alimtalk_senders SET is_default = false/.test(s));
    const insert = sqls.findIndex((s) => /INSERT INTO alimtalk_senders/.test(s));
    expect(begin).toBeGreaterThanOrEqual(0);
    expect(begin).toBeLessThan(clear);
    expect(clear).toBeLessThan(insert);
    expect(sqls).toContain("COMMIT");
  });

  it("is_default가 아니면 기존 기본을 건드리지 않는다", async () => {
    const { pg, calls } = fakePg([appsHandler, [/INSERT INTO alimtalk_senders/, () => ({ rows: [sender] })]]);
    const ctl = new AlimtalkSendersController(pg, audit);
    await ctl.create(appId, { sender_key: "sk-2" }, request("editor"));
    expect(calls.some((c) => /SET is_default = false/.test(c.sql))).toBe(false);
  });

  it("sender_key 중복(23505)은 400으로 바뀌고 트랜잭션은 롤백된다", async () => {
    const { pg, calls } = fakePg([
      appsHandler,
      [/INSERT INTO alimtalk_senders/, () => { throw Object.assign(new Error("dup"), { code: "23505" }); }],
    ]);
    const ctl = new AlimtalkSendersController(pg, audit);
    await expect(ctl.create(appId, { sender_key: "sk-1" }, request("editor"))).rejects.toMatchObject({ status: 400 });
    expect(calls.map((c) => c.sql)).toContain("ROLLBACK");
  });

  it("없는 발신프로필 수정·삭제는 404", async () => {
    const { pg } = fakePg([appsHandler]);
    const ctl = new AlimtalkSendersController(pg, audit);
    const id = randomUUID();
    await expect(ctl.update(appId, id, { channel_name: "@x" }, request("editor"))).rejects.toMatchObject({ status: 404 });
    await expect(ctl.remove(appId, id, request("editor"))).rejects.toMatchObject({ status: 404 });
  });

  it("타 테넌트 앱은 404이고 발신프로필 쿼리에 닿지 않는다", async () => {
    const { pg, calls } = fakePg([appsHandler]);
    const ctl = new AlimtalkSendersController(pg, audit);
    await expect(ctl.list(otherAppId, request("owner"))).rejects.toMatchObject({ status: 404 });
    expect(calls.some((c) => /alimtalk_senders/.test(c.sql))).toBe(false);
  });

  it("잘못된 입력은 400 (빈 sender_key · 미지의 status)", async () => {
    const { pg } = fakePg([appsHandler]);
    const ctl = new AlimtalkSendersController(pg, audit);
    await expect(ctl.create(appId, { sender_key: "" }, request("editor"))).rejects.toMatchObject({ status: 400 });
    await expect(ctl.create(appId, { sender_key: "k", status: "paused" }, request("editor"))).rejects.toMatchObject({ status: 400 });
  });
});

describe("AlimtalkTemplatesController", () => {
  const template = { id: randomUUID(), sender_id: randomUUID(), template_code: "T-1" };
  const senderId = randomUUID();

  /** 동기화가 202가 되는 완전 배선 상태. 케이스별로 한 조각씩 빼서 400을 확인한다. */
  const wiredHandlers = (over: {
    senders?: unknown[];
    connector?: unknown[];
    credential?: unknown[];
  } = {}) =>
    [
      appsHandler,
      [
        /FROM alimtalk_senders/,
        ([, , requested]: unknown[]) => {
          const rows = over.senders ?? [{ id: senderId, sender_key: "sk-1", is_default: true }];
          return { rows: requested ? rows.filter((r) => (r as { id: string }).id === requested) : rows };
        },
      ],
      [/FROM channel_connectors/, () => ({ rows: over.connector ?? [{ connector_id: "kakao_alimtalk_nhn", enabled: true }] })],
      [/FROM credentials/, () => ({ rows: over.credential ?? [{ status: "verified" }] })],
    ] as Array<[RegExp, (params: unknown[]) => { rows: unknown[] }]>;

  const templatesCtl = (handlers: Array<[RegExp, (params: unknown[]) => { rows: unknown[] }]>) => {
    const { pg, calls } = fakePg(handlers);
    const queue = { publish: vi.fn(async () => ({})) };
    const ctl = new AlimtalkTemplatesController(pg, queue as unknown as QueueProducer, audit);
    return { ctl, calls, queue };
  };

  it("sender_id 필터는 선택이며, 없으면 앱 전체를 반환한다", async () => {
    const { ctl, calls } = templatesCtl([appsHandler, [/FROM alimtalk_templates/, () => ({ rows: [template] })]]);
    await expect(ctl.list(appId, {}, request("viewer"))).resolves.toEqual({ templates: [template] });
    expect(calls.find((c) => /alimtalk_templates/.test(c.sql))!.params).toEqual([tenantId, appId, null]);
    await ctl.list(appId, { sender_id: template.sender_id }, request("viewer"));
    expect(calls.at(-1)!.params).toEqual([tenantId, appId, template.sender_id]);
  });

  it("sender_id가 uuid가 아니면 400", async () => {
    const { ctl } = templatesCtl([appsHandler]);
    await expect(ctl.list(appId, { sender_id: "not-a-uuid" }, request("viewer"))).rejects.toMatchObject({ status: 400 });
  });

  it("동기화는 202 + 잡 발행 — 벤더 호출은 API가 아니라 워커가 한다", async () => {
    const { ctl, queue } = templatesCtl(wiredHandlers());
    const req = request("editor");
    await expect(ctl.sync(appId, {}, req)).resolves.toEqual({ accepted: true, sender_id: senderId });
    expect(queue.publish).toHaveBeenCalledTimes(1);
    const [stream, input] = queue.publish.mock.calls[0] as unknown as [
      string,
      { type: string; tenantId: string; appId: string; payload: Record<string, unknown> },
    ];
    expect(stream).toBe(STREAMS.alimtalkTemplateSync);
    expect(input.type).toBe("alimtalk.template.sync");
    expect(input.tenantId).toBe(tenantId);
    expect(input.payload).toEqual({
      app_id: appId,
      sender_id: senderId,
      sender_key: "sk-1",
      connector_id: "kakao_alimtalk_nhn",
      requested_by: req.member.memberId,
      requested_at: expect.any(String),
    });
    expect(audit.recordAs).toHaveBeenCalledWith(
      req.member, "127.0.0.1", "alimtalk_template.sync",
      expect.objectContaining({ targetType: "alimtalk_sender", targetId: senderId }),
    );
  });

  it("발신프로필 조회는 tenant_id로 스코프한다", async () => {
    const { ctl, calls } = templatesCtl(wiredHandlers());
    await ctl.sync(appId, {}, request("editor"));
    const select = calls.find((c) => /FROM alimtalk_senders/.test(c.sql))!;
    expect(select.sql).toContain("tenant_id = $1");
    expect(select.params.slice(0, 2)).toEqual([tenantId, appId]);
  });

  it("배선이 없으면 400 — 소비될 수 없는 잡을 202로 받아주면 거짓 신호다", async () => {
    for (const over of [
      { senders: [] },
      { connector: [] },
      { connector: [{ connector_id: "kakao_alimtalk_nhn", enabled: false }] },
      { credential: [] },
      { credential: [{ status: "unverified" }] },
    ]) {
      const { ctl, queue } = templatesCtl(wiredHandlers(over));
      await expect(ctl.sync(appId, {}, request("editor"))).rejects.toMatchObject({ status: 400 });
      expect(queue.publish).not.toHaveBeenCalled();
    }
  });

  it("400 메시지는 무엇이 없는지 한국어로 지목한다", async () => {
    const cases: Array<[Parameters<typeof wiredHandlers>[0], RegExp]> = [
      [{ senders: [] }, /발신프로필/],
      [{ connector: [] }, /커넥터 배선이 없습니다/],
      [{ credential: [] }, /크리덴셜이 등록되지 않았습니다/],
    ];
    for (const [over, re] of cases) {
      const { ctl } = templatesCtl(wiredHandlers(over));
      await expect(ctl.sync(appId, {}, request("editor"))).rejects.toMatchObject({
        response: expect.objectContaining({ message: expect.stringMatching(re) }),
      });
    }
  });

  it("발신프로필이 여러 개인데 기본이 없으면 400 — 아무거나 고르지 않는다", async () => {
    const senders = [
      { id: senderId, sender_key: "sk-1", is_default: false },
      { id: randomUUID(), sender_key: "sk-2", is_default: false },
    ];
    const { ctl, queue } = templatesCtl(wiredHandlers({ senders }));
    await expect(ctl.sync(appId, {}, request("editor"))).rejects.toMatchObject({ status: 400 });
    expect(queue.publish).not.toHaveBeenCalled();
  });

  it("sender_id를 지정하면 그 발신프로필로 발행하고, 이 앱에 없으면 400", async () => {
    const other = randomUUID();
    const senders = [
      { id: senderId, sender_key: "sk-1", is_default: false },
      { id: other, sender_key: "sk-2", is_default: false },
    ];
    const { ctl, queue } = templatesCtl(wiredHandlers({ senders }));
    await expect(ctl.sync(appId, { sender_id: other }, request("editor"))).resolves.toEqual({
      accepted: true, sender_id: other,
    });
    expect((queue.publish.mock.calls[0] as unknown as [string, { payload: { sender_key: string } }])[1].payload.sender_key).toBe("sk-2");

    const missing = templatesCtl(wiredHandlers({ senders }));
    await expect(missing.ctl.sync(appId, { sender_id: randomUUID() }, request("editor"))).rejects.toMatchObject({ status: 400 });
    expect(missing.queue.publish).not.toHaveBeenCalled();
  });

  it("타 테넌트 앱은 동기화도 404 (앱 존재 여부가 먼저)", async () => {
    const { ctl, calls, queue } = templatesCtl(wiredHandlers());
    await expect(ctl.sync(otherAppId, {}, request("owner"))).rejects.toMatchObject({ status: 404 });
    expect(calls.some((c) => /alimtalk_senders/.test(c.sql))).toBe(false);
    expect(queue.publish).not.toHaveBeenCalled();
  });
});
