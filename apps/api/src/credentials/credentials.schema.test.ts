import "reflect-metadata";
import { describe, expect, it } from "vitest";
import { credentialSchema, flattenCredentialPayload } from "./credentials.controller";

/** 알림톡 크리덴셜 폼 계약 (0004_alimtalk.sql: channel_kind에 'alimtalk' 하나). */
describe("alimtalk 크리덴셜 스키마", () => {
  const valid = { kind: "alimtalk", connector_id: "kakao_alimtalk_nhn", api_key: "ak" };

  it("connector_id + api_key만으로 통과한다 (벤더별 나머지는 워커가 manifest로 검증)", () => {
    const r = credentialSchema.safeParse(valid);
    expect(r.success).toBe(true);
  });

  it("선택 필드를 모두 실어도 통과한다", () => {
    const r = credentialSchema.safeParse({
      ...valid,
      secret_key: "sk",
      sender_key: "profile-1",
      base_url: "https://api-alimtalk.cloud.toast.com",
    });
    expect(r.success).toBe(true);
  });

  it("connector_id는 manifest ID 규칙(^[a-z][a-z0-9_]{1,63}$)을 따른다", () => {
    for (const bad of ["NHN", "9nhn", "a", "nhn-cloud", "nhn cloud", ""]) {
      expect(credentialSchema.safeParse({ ...valid, connector_id: bad }).success, bad).toBe(false);
    }
    for (const good of ["nhn", "kakao_alimtalk_nhn", "aligo1"]) {
      expect(credentialSchema.safeParse({ ...valid, connector_id: good }).success, good).toBe(true);
    }
  });

  it("connector_id·api_key 누락은 거절 — 워커가 벤더를 못 찾는 상태로 저장되면 안 된다", () => {
    expect(credentialSchema.safeParse({ kind: "alimtalk", api_key: "ak" }).success).toBe(false);
    expect(credentialSchema.safeParse({ kind: "alimtalk", connector_id: "nhn" }).success).toBe(false);
  });

  it("base_url이 URL이 아니면 거절", () => {
    expect(credentialSchema.safeParse({ ...valid, base_url: "api.example.com" }).success).toBe(false);
  });

  it("union이 kind로 갈라지므로 이메일 크리덴셜은 그대로 동작한다", () => {
    expect(
      credentialSchema.safeParse({ kind: "email_resend", api_key: "re_x", from_email: "a@b.co" }).success,
    ).toBe(true);
    expect(credentialSchema.safeParse({ kind: "sms", api_key: "x" }).success).toBe(false);
  });
});

describe("flattenCredentialPayload", () => {
  // 벤더는 자기 manifest가 선언한 필드명 그대로 읽는다. NHN은 app_key를 읽는데
  // 콘솔 슬롯은 api_key라서, extra가 없으면 콘솔에서 저장한 NHN 크리덴셜은
  // 벤더 검증에서 "app_key 누락"으로 떨어진다 — 화면에서 설정 자체가 불가능해진다.
  it("extra의 벤더 고유 필드가 저장 payload에 이름 그대로 남는다", () => {
    const out = flattenCredentialPayload({
      connector_id: "alimtalk_nhn",
      api_key: "AK",
      secret_key: "SK",
      extra: { app_key: "AK", partner_id: "P-1" },
    });
    expect(out.app_key).toBe("AK");
    expect(out.partner_id).toBe("P-1");
    expect(out.extra).toBeUndefined();
  });

  it("같은 키는 이름 있는 슬롯이 이긴다", () => {
    const out = flattenCredentialPayload({ api_key: "slot", extra: { api_key: "extra" } });
    expect(out.api_key).toBe("slot");
  });

  it("예약 키는 extra가 덮어쓸 수 없다", () => {
    const out = flattenCredentialPayload({
      connector_id: "alimtalk_nhn",
      extra: { connector_id: "hijack", kind: "push_fcm", extra: "nested" },
    });
    expect(out.connector_id).toBe("alimtalk_nhn");
    expect(out.kind).toBeUndefined();
    expect(out.extra).toBeUndefined();
  });

  it("extra가 없으면 그대로 통과한다 — 기존 크리덴셜에 영향 없음", () => {
    const out = flattenCredentialPayload({ host: "smtp.example.com", port: 587 });
    expect(out).toEqual({ host: "smtp.example.com", port: 587 });
  });
});

describe("알림톡 크리덴셜 — 벤더마다 필드 이름이 다르다", () => {
  const base = { kind: "alimtalk" as const, connector_id: "alimtalk_nhn" };

  // api_key는 흔한 이름일 뿐이다. 이걸 필수로 두면 대응 필드가 없는 벤더는
  // 콘솔이 아무 값이나 슬롯에 밀어 넣어야 저장돼, 콘솔이 서버 제약을 우회하게 된다.
  it("extra만으로도 저장된다 — api_key 슬롯을 쓰지 않는 벤더", () => {
    const r = credentialSchema.safeParse({ ...base, extra: { app_key: "AK", secret_key: "SK" } });
    expect(r.success).toBe(true);
  });

  it("슬롯만으로도 저장된다 — 기존 형태", () => {
    expect(credentialSchema.safeParse({ ...base, api_key: "AK" }).success).toBe(true);
  });

  it("비밀이 하나도 없으면 거절한다", () => {
    const r = credentialSchema.safeParse({ ...base, extra: {} });
    expect(r.success).toBe(false);
  });
});
