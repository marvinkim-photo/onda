import { describe, expect, it } from "vitest";
import {
  CREDENTIAL_SLOTS,
  canSubmit,
  credentialSlot,
  fieldDestination,
  initialValues,
  planConfig,
  planCredential,
  schemaFields,
} from "./connector-schema";

/** 배포에 실제로 들어 있는 NHN 매니페스트의 credentials.schema. */
const nhnSchema = {
  type: "object",
  required: ["app_key", "secret_key"],
  properties: {
    app_key: { type: "string", title: "AppKey (URL 경로에 실린다)" },
    secret_key: { type: "string", title: "SecretKey (X-Secret-Key 헤더)", "x-secret": true },
    sender_key: { type: "string", title: "카카오 발신프로필 키 (senderKey)" },
    base_url: { type: "string", title: "API 베이스 URL (미지정 시 공식 엔드포인트)" },
  },
  additionalProperties: false,
};

describe("schemaFields", () => {
  it("선언 순서대로 필드를 만들고 title을 라벨로 쓴다", () => {
    const fields = schemaFields(nhnSchema);
    expect(fields.map((f) => f.name)).toEqual(["app_key", "secret_key", "sender_key", "base_url"]);
    expect(fields[0]!.label).toBe("AppKey (URL 경로에 실린다)");
  });

  it("required 목록의 필드만 필수로 표시한다", () => {
    const fields = schemaFields(nhnSchema);
    expect(fields.filter((f) => f.required).map((f) => f.name)).toEqual(["app_key", "secret_key"]);
  });

  it("x-secret은 password 입력이 된다", () => {
    const fields = schemaFields(nhnSchema);
    expect(fields.find((f) => f.name === "secret_key")).toMatchObject({ secret: true, kind: "password" });
    expect(fields.find((f) => f.name === "app_key")).toMatchObject({ secret: false, kind: "text" });
  });

  it("writeOnly·format:password도 비밀로 본다", () => {
    const fields = schemaFields({
      type: "object",
      properties: {
        a: { type: "string", writeOnly: true },
        b: { type: "string", format: "password" },
      },
    });
    expect(fields.map((f) => f.kind)).toEqual(["password", "password"]);
  });

  it("enum은 select가 되고 선택지를 보존한다", () => {
    const fields = schemaFields({
      type: "object",
      properties: { region: { type: "string", enum: ["kr1", "kr2"], title: "리전" } },
    });
    expect(fields[0]).toMatchObject({ kind: "select", options: ["kr1", "kr2"], label: "리전" });
  });

  it("description을 도움말로, examples를 placeholder로, default를 초기값으로 옮긴다", () => {
    const fields = schemaFields({
      type: "object",
      properties: {
        base_url: {
          type: "string",
          description: "미지정 시 공식 엔드포인트",
          examples: ["https://api.example.com"],
          default: "https://api.example.com",
        },
      },
    });
    expect(fields[0]!.description).toBe("미지정 시 공식 엔드포인트");
    expect(fields[0]!.placeholder).toBe("https://api.example.com");
    expect(initialValues(fields)).toEqual({ base_url: "https://api.example.com" });
  });

  it("title이 없으면 속성 이름을 라벨로 쓴다 (필드를 빠뜨리지 않는다)", () => {
    expect(schemaFields({ type: "object", properties: { user_id: { type: "string" } } })[0]!.label).toBe("user_id");
  });

  it("type을 생략한 속성도 문자열로 그린다", () => {
    expect(schemaFields({ type: "object", properties: { token: {} } })).toHaveLength(1);
  });

  it("문자열이 아닌 속성은 건너뛴다 (그릴 위젯이 없다)", () => {
    const fields = schemaFields({
      type: "object",
      properties: { name: { type: "string" }, retries: { type: "integer" }, opts: { type: "object" } },
    });
    expect(fields.map((f) => f.name)).toEqual(["name"]);
  });

  it("스키마가 없거나 객체가 아니면 빈 목록 — 페이지는 계속 뜬다", () => {
    expect(schemaFields(undefined)).toEqual([]);
    expect(schemaFields(null)).toEqual([]);
    expect(schemaFields("nope")).toEqual([]);
    expect(schemaFields({ type: "array", items: {} })).toEqual([]);
    expect(schemaFields({ type: "object" })).toEqual([]);
  });

  it("select의 초기값은 첫 선택지다", () => {
    const fields = schemaFields({ type: "object", properties: { r: { type: "string", enum: ["a", "b"] } } });
    expect(initialValues(fields)).toEqual({ r: "a" });
  });
});

describe("credentialSlot", () => {
  it("벤더가 다르게 부르는 이름을 같은 슬롯으로 모은다", () => {
    expect(credentialSlot("app_key")).toBe("api_key");
    expect(credentialSlot("apiKey")).toBe("api_key");
    expect(credentialSlot("APIKEY")).toBe("api_key");
    expect(credentialSlot("api_secret")).toBe("secret_key");
    expect(credentialSlot("senderKey")).toBe("sender_key");
    expect(credentialSlot("endpoint")).toBe("base_url");
  });

  it("슬롯이 없는 필드는 null이다", () => {
    expect(credentialSlot("user_id")).toBeNull();
    expect(credentialSlot("sms_fallback_sender")).toBeNull();
  });

  it("슬롯 목록은 서버 크리덴셜 스키마와 같다", () => {
    expect([...CREDENTIAL_SLOTS]).toEqual(["api_key", "secret_key", "sender_key", "base_url"]);
  });
});

describe("planCredential", () => {
  const fields = schemaFields(nhnSchema);

  it("매니페스트가 선언한 이름 그대로 extra에 싣는다 — 벤더가 그 이름으로 읽는다", () => {
    const plan = planCredential(fields, { app_key: "AK", secret_key: "SK" });
    expect(plan.extra).toEqual({ app_key: "AK", secret_key: "SK" });
  });

  it("회귀: NHN을 저장하면 app_key가 실제로 실린다 (슬롯 이름으로만 보내 검증 실패하던 결함)", () => {
    const plan = planCredential(fields, { app_key: "AK", secret_key: "SK" });
    // 워커 nhn.go의 credential 구조체가 읽는 이름
    expect(plan.extra.app_key).toBe("AK");
    expect(plan.extra.secret_key).toBe("SK");
    expect(canSubmit(plan)).toBe(true);
  });

  it("슬롯 매핑도 함께 보낸다 — 서버가 extra 위에 얹는 호환용이다", () => {
    const plan = planCredential(fields, {
      app_key: "AK", secret_key: "SK", sender_key: "@onda", base_url: "https://x.example.com",
    });
    expect(plan.credential).toEqual({
      api_key: "AK", secret_key: "SK", sender_key: "@onda", base_url: "https://x.example.com",
    });
  });

  it("빈 필수 필드를 라벨로 알려 주고 저장을 막는다", () => {
    const plan = planCredential(fields, { app_key: "AK", secret_key: "   " });
    expect(plan.missingRequired).toEqual(["SecretKey (X-Secret-Key 헤더)"]);
    expect(canSubmit(plan)).toBe(false);
  });

  it("값 앞뒤 공백은 저장 전에 지운다", () => {
    expect(planCredential(fields, { app_key: "  AK  ", secret_key: "SK" }).extra.app_key).toBe("AK");
  });

  it("선택 필드가 비면 아예 보내지 않는다 (서버 기본값을 쓰게)", () => {
    const plan = planCredential(fields, { app_key: "AK", secret_key: "SK", base_url: "" });
    expect(plan.extra.base_url).toBeUndefined();
    expect(plan.credential.base_url).toBeUndefined();
  });

  it("슬롯이 없는 비밀도 이제 저장된다 — 갈 곳이 생겼으므로 막지 않는다", () => {
    const odd = schemaFields({
      type: "object",
      required: ["api_key", "signing_seed"],
      properties: {
        api_key: { type: "string" },
        signing_seed: { type: "string", title: "서명 시드", "x-secret": true },
      },
    });
    const plan = planCredential(odd, { api_key: "ak", signing_seed: "seed" });
    expect(plan.extra).toEqual({ api_key: "ak", signing_seed: "seed" });
    expect(canSubmit(plan)).toBe(true);
  });

  it("비(非)비밀 필드도 크리덴셜에 남는다 — config로 새지 않는다", () => {
    const withExtra = schemaFields({
      type: "object",
      required: ["api_key"],
      properties: {
        api_key: { type: "string", "x-secret": true },
        user_id: { type: "string", title: "발신 계정 ID" },
      },
    });
    const plan = planCredential(withExtra, { api_key: "ak", user_id: "onda" });
    expect(plan.extra).toEqual({ api_key: "ak", user_id: "onda" });
  });

  it("api_key에 매핑되는 필드가 없어도 저장한다 — 무엇이 필요한지는 매니페스트가 정한다", () => {
    const odd = schemaFields({
      type: "object", required: ["user_id"], properties: { user_id: { type: "string", title: "계정" } },
    });
    const plan = planCredential(odd, { user_id: "onda" });
    expect(plan.extra).toEqual({ user_id: "onda" });
    expect(canSubmit(plan)).toBe(true);
  });

  it("매핑되는 필드가 없으면 슬롯을 아무 값으로도 채우지 않는다 (콘솔이 서버 제약을 우회하지 않는다)", () => {
    const odd = schemaFields({
      type: "object", required: ["user_id"], properties: { user_id: { type: "string" } },
    });
    expect(planCredential(odd, { user_id: "onda" }).credential).toEqual({});
  });

  it("아무 값도 없으면 저장할 수 없다 — 서버가 빈 크리덴셜을 거절한다", () => {
    const plan = planCredential(fields, {});
    expect(plan.empty).toBe(true);
    expect(canSubmit(plan)).toBe(false);
  });

  it("비밀이 secret_key 슬롯에만 있어도 저장할 수 있다 (서버는 하나만 있으면 받는다)", () => {
    const odd = schemaFields({
      type: "object",
      required: ["user_id", "password"],
      properties: { user_id: { type: "string" }, password: { type: "string", "x-secret": true } },
    });
    const plan = planCredential(odd, { user_id: "onda", password: "pw" });
    expect(plan.credential).toEqual({ secret_key: "pw" });
    expect(plan.credential.api_key).toBeUndefined();
    expect(canSubmit(plan)).toBe(true);
  });
});

describe("fieldDestination", () => {
  it("이름이 슬롯과 다르면 둘 다 밝힌다 (조용히 버리지 않는다)", () => {
    const [appKey] = schemaFields(nhnSchema);
    expect(fieldDestination(appKey!)).toBe("크리덴셜에 app_key(으)로 저장 · api_key 슬롯에도 함께");
  });

  it("이름이 슬롯과 같으면 한 마디로 끝낸다", () => {
    const [secretKey] = schemaFields({ type: "object", properties: { secret_key: { type: "string" } } });
    expect(fieldDestination(secretKey!)).toBe("크리덴셜에 이 이름 그대로 저장");
  });

  it("슬롯이 없는 필드도 제 이름으로 저장된다고 말한다", () => {
    const [seed] = schemaFields({ type: "object", properties: { signing_seed: { type: "string" } } });
    expect(fieldDestination(seed!)).toBe("크리덴셜에 이 이름 그대로 저장");
  });
});

describe("planConfig", () => {
  it("빈 값은 배선 config에 넣지 않는다", () => {
    const fields = schemaFields({
      type: "object",
      properties: {
        sms_fallback_sender: { type: "string" },
        base_url: { type: "string" },
      },
    });
    expect(planConfig(fields, { sms_fallback_sender: " 025550000 ", base_url: "  " })).toEqual({
      sms_fallback_sender: "025550000",
    });
  });
});
