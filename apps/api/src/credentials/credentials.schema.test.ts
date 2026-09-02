import "reflect-metadata";
import { describe, expect, it } from "vitest";
import { credentialSchema } from "./credentials.controller";

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
