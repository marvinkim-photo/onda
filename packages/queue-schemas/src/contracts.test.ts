import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import Ajv2020 from "ajv/dist/2020";
import addFormats from "ajv-formats";
import { describe, expect, it } from "vitest";
import { payloadSchemas } from "./index";

/**
 * 커넥터 계약(send.message.v1 · connector.manifest.v0 · message.lifecycle.v1) 검증.
 * - 스키마 자체가 draft 2020-12로 컴파일되는지
 * - examples/ 의 모든 예시가 대응 스키마를 통과하는지
 * - 계약의 핵심 불변식(멱등 키 endpoint 포함, unsupported content 거절 등)
 */
const root = join(__dirname, "..");
const schemasDir = join(root, "schemas");
const examplesDir = join(root, "examples");

const load = (dir: string, f: string) => JSON.parse(readFileSync(join(dir, f), "utf8"));

const ajv = new Ajv2020({ strict: false, allErrors: true });
addFormats(ajv);
for (const f of readdirSync(schemasDir).filter((n) => n.endsWith(".schema.json"))) {
  ajv.addSchema(load(schemasDir, f));
}

const validate = (id: string, data: unknown) => {
  const v = ajv.getSchema(id);
  if (!v) throw new Error(`schema not registered: ${id}`);
  const ok = v(data);
  return { ok, errors: v.errors };
};

const SEND = "https://onda.dev/schemas/queue/send.message.v1.json";
const MANIFEST = "https://onda.dev/schemas/connector/manifest.v0.json";
const LIFECYCLE = "https://onda.dev/schemas/queue/message.lifecycle.v1.json";

describe("schemas compile", () => {
  it("모든 schemas/*.schema.json 이 2020-12로 컴파일된다", () => {
    for (const f of readdirSync(schemasDir).filter((n) => n.endsWith(".schema.json"))) {
      const s = load(schemasDir, f);
      expect(ajv.getSchema(s.$id), f).toBeTruthy();
    }
  });
});

describe("examples validate", () => {
  it("send.message 알림톡+SMS 폴백 예시", () => {
    const r = validate(SEND, load(examplesDir, "send.message.kakao_alimtalk.json"));
    expect(r.errors).toBeNull();
    expect(r.ok).toBe(true);
  });
  it("connector manifest 알림톡 예시", () => {
    const r = validate(MANIFEST, load(examplesDir, "connector.manifest.kakao_alimtalk_nhn.json"));
    expect(r.errors).toBeNull();
    expect(r.ok).toBe(true);
  });
  it("message.lifecycle delivered 예시", () => {
    const r = validate(LIFECYCLE, load(examplesDir, "message.lifecycle.delivered.json"));
    expect(r.errors).toBeNull();
    expect(r.ok).toBe(true);
  });
});

describe("contract invariants", () => {
  const base = () => load(examplesDir, "send.message.kakao_alimtalk.json");

  it("target.endpoint_id 누락은 거절된다 (규칙 6: device/endpoint 누락 금지)", () => {
    const m = base();
    delete m.target.endpoint_id;
    expect(validate(SEND, m).ok).toBe(false);
  });

  it("consent 없는 발송은 거절된다", () => {
    const m = base();
    delete m.consent;
    expect(validate(SEND, m).ok).toBe(false);
  });

  it("content가 비어 있으면 거절된다", () => {
    const m = base();
    m.content = {};
    expect(validate(SEND, m).ok).toBe(false);
  });

  it("복호화된 크리덴셜을 큐에 싣는 필드는 존재하지 않는다", () => {
    const m = base();
    m.connector.credentials = { secret: "x" };
    expect(validate(SEND, m).ok).toBe(false);
  });

  it("fallback은 최대 3단계", () => {
    const m = base();
    m.fallback = [m.fallback[0], m.fallback[0], m.fallback[0], m.fallback[0]];
    expect(validate(SEND, m).ok).toBe(false);
  });

  it("manifest의 content capability에 없는 content는 엔진이 사전 거절할 수 있도록 둘 다 같은 enum을 쓴다", () => {
    const send = load(schemasDir, "send.message.v1.schema.json");
    const manifest = load(schemasDir, "connector.manifest.v0.schema.json");
    const contentKinds = Object.keys(send.$defs.Content.properties).sort();
    const capKinds = [...manifest.properties.capabilities.properties.content.items.enum].sort();
    expect(capKinds).toEqual(contentKinds);
    expect(send.$defs.ChannelId.enum).toEqual(manifest.properties.channel.enum);
    expect(send.$defs.Target.properties.type.enum).toEqual(manifest.properties.target_types.items.enum);
  });

  it("remote_http 런타임은 endpoint·auth가 필수", () => {
    const m = load(examplesDir, "connector.manifest.kakao_alimtalk_nhn.json");
    m.runtime = { type: "remote_http" };
    expect(validate(MANIFEST, m).ok).toBe(false);
    m.runtime = { type: "remote_http", endpoint: "https://connector.example.com", auth: "hmac_sha256" };
    expect(validate(MANIFEST, m).ok).toBe(true);
  });

  it("send.message · message.lifecycle이 payloadSchemas에 등록되어 있다 (등록 누락 = 런타임 무검증)", () => {
    for (const type of ["send.message", "message.lifecycle"] as const) {
      const schema = payloadSchemas[type];
      expect(schema, type).toBeTruthy();
      expect(ajv.getSchema((schema as { $id: string }).$id), type).toBeTruthy();
    }
    expect((payloadSchemas["send.message"] as { $id: string }).$id).toBe(SEND);
    expect((payloadSchemas["message.lifecycle"] as { $id: string }).$id).toBe(LIFECYCLE);
  });

  it("lifecycle failed 이벤트 상태 enum은 manifest.lifecycle.reports enum과 같다", () => {
    const lc = load(schemasDir, "message.lifecycle.v1.schema.json");
    const manifest = load(schemasDir, "connector.manifest.v0.schema.json");
    expect(lc.properties.status.enum).toEqual(manifest.properties.lifecycle.properties.reports.items.enum);
  });
});
