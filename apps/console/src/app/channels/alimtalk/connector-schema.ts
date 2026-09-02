/**
 * 커넥터 매니페스트의 JSON Schema → 폼 모델.
 *
 * 알림톡 벤더(딜러사)는 서로 다른 비밀을 요구한다(NHN app_key+secret_key, 알리고 apikey+userid,
 * Solapi api_key+api_secret). 벤더별 폼을 손으로 짜면 커넥터를 하나 추가할 때마다 콘솔을 고쳐야 하므로,
 * 배포의 매니페스트가 선언한 `credentials.schema` / `config.schema`를 그대로 렌더한다.
 *
 * React·API 클라이언트 없이 단위 테스트가 돌도록 순수 함수만 둔다
 * (콘솔 vitest는 `@/` 별칭을 해석하지 못한다 — email-provider-links.ts와 같은 이유).
 */

/** 폼 한 줄. kind가 곧 입력 위젯이다. */
export interface SchemaField {
  name: string;
  label: string;
  description?: string;
  required: boolean;
  /** 비밀 — password 입력으로 그리고, 저장 후에는 다시 보여주지 않는다. */
  secret: boolean;
  kind: "text" | "password" | "select";
  /** kind === "select"일 때의 선택지 */
  options?: string[];
  placeholder?: string;
  defaultValue?: string;
}

interface RawProperty {
  type?: unknown;
  title?: unknown;
  description?: unknown;
  enum?: unknown;
  format?: unknown;
  examples?: unknown;
  default?: unknown;
  writeOnly?: unknown;
  "x-secret"?: unknown;
}

function asRecord(value: unknown): Record<string, unknown> | null {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

/**
 * 문자열 필드인가. type 미지정은 문자열로 본다(JSON Schema에서 type은 선택이고,
 * 매니페스트가 생략해도 텍스트 한 줄로 그리는 편이 필드를 통째로 빠뜨리는 것보다 낫다).
 * type이 배열이면 "string"을 포함할 때만 받는다(["string","null"] 관용).
 */
function isStringProperty(type: unknown): boolean {
  if (type === undefined || type === null) return true;
  if (typeof type === "string") return type === "string";
  if (Array.isArray(type)) return type.includes("string");
  return false;
}

/** x-secret · writeOnly · format:password 중 하나라도 있으면 비밀로 취급한다. */
function isSecret(prop: RawProperty): boolean {
  return prop["x-secret"] === true || prop.writeOnly === true || prop.format === "password";
}

function stringEnum(value: unknown): string[] | null {
  if (!Array.isArray(value) || value.length === 0) return null;
  const out = value.filter((v): v is string => typeof v === "string");
  return out.length === value.length ? out : null;
}

/**
 * `type: "object"` 스키마의 문자열 속성을 선언 순서대로 폼 필드로 바꾼다.
 * 스키마가 없거나 객체가 아니면 빈 배열 — 폼이 사라질 뿐 페이지는 그대로 뜬다.
 */
export function schemaFields(schema: unknown): SchemaField[] {
  const root = asRecord(schema);
  if (!root) return [];
  if (root.type !== undefined && root.type !== "object") return [];
  const properties = asRecord(root.properties);
  if (!properties) return [];
  const required = new Set(
    (Array.isArray(root.required) ? root.required : []).filter((r): r is string => typeof r === "string"),
  );

  const fields: SchemaField[] = [];
  for (const [name, rawProp] of Object.entries(properties)) {
    const prop = (asRecord(rawProp) ?? {}) as RawProperty;
    if (!isStringProperty(prop.type)) continue;
    const options = stringEnum(prop.enum);
    const secret = isSecret(prop);
    const example = Array.isArray(prop.examples)
      ? prop.examples.find((e): e is string => typeof e === "string")
      : undefined;
    fields.push({
      name,
      label: typeof prop.title === "string" && prop.title ? prop.title : name,
      description: typeof prop.description === "string" ? prop.description : undefined,
      required: required.has(name),
      secret,
      // enum이면 선택지가 곧 검증이므로 비밀이어도 select로 그린다(값이 이미 공개 목록이다).
      kind: options ? "select" : secret ? "password" : "text",
      options: options ?? undefined,
      placeholder: example,
      defaultValue: typeof prop.default === "string" ? prop.default : undefined,
    });
  }
  return fields;
}

/** 필드 목록의 초기값 — default가 있으면 채우고, 없으면 빈 문자열. */
export function initialValues(fields: SchemaField[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const f of fields) out[f.name] = f.defaultValue ?? (f.kind === "select" ? (f.options?.[0] ?? "") : "");
  return out;
}

/** API가 받는 알림톡 크리덴셜의 고정 슬롯 (PUT /v1/apps/:id/credentials). */
export const CREDENTIAL_SLOTS = ["api_key", "secret_key", "sender_key", "base_url"] as const;
export type CredentialSlot = (typeof CREDENTIAL_SLOTS)[number];

/**
 * 매니페스트 필드명 → 크리덴셜 슬롯. 벤더가 같은 비밀을 다른 이름으로 부르기 때문에 필요하다
 * (NHN은 app_key, 알리고는 apikey, Solapi는 api_secret).
 * 비교 전에 소문자화하고 영숫자만 남기므로 `appKey` · `app_key` · `APP-KEY`가 모두 같은 키가 된다.
 */
const SLOT_ALIASES: Record<CredentialSlot, string[]> = {
  api_key: ["apikey", "appkey", "accesskey", "clientkey"],
  secret_key: ["secretkey", "apisecret", "secret", "clientsecret", "password"],
  sender_key: ["senderkey", "senderprofilekey", "plusfriendid"],
  base_url: ["baseurl", "apiurl", "endpoint", "apiendpoint"],
};

function normalizeName(name: string): string {
  return name.toLowerCase().replace(/[^a-z0-9]/g, "");
}

/** 이 필드가 어느 슬롯에 담기는가. 담을 곳이 없으면 null. */
export function credentialSlot(name: string): CredentialSlot | null {
  const key = normalizeName(name);
  for (const slot of CREDENTIAL_SLOTS) {
    if (SLOT_ALIASES[slot].includes(key)) return slot;
  }
  return null;
}

export interface CredentialPlan {
  /**
   * 벤더가 실제로 읽는 값 — 매니페스트가 선언한 필드명 그대로.
   * NHN은 app_key를, mock은 api_key를 자기 이름으로 읽으므로 이것이 정본이다.
   */
  extra: Record<string, string>;
  /** 이름 있는 슬롯(호환용). 서버가 extra 위에 얹으므로 같은 키면 이 값이 이긴다. */
  credential: Partial<Record<CredentialSlot, string>>;
  /** 값이 비어 있는 필수 필드 */
  missingRequired: string[];
  /**
   * 보낼 값이 하나도 없다. 서버는 "슬롯이든 extra든 비밀이 하나는 있어야 한다"까지만 강제하므로
   * 특정 슬롯이 비었는지는 따지지 않는다 — 무엇이 필요한지는 매니페스트가 정한다.
   */
  empty: boolean;
}

/** 저장 가능한 계획인가 — 버튼 활성화 조건. */
export function canSubmit(plan: CredentialPlan): boolean {
  return plan.missingRequired.length === 0 && !plan.empty;
}

/**
 * 폼 값 → 저장 계획.
 *
 * 크리덴셜 스키마가 선언한 필드는 **모두** 제 이름으로 extra에 싣는다. 벤더가 그 이름으로 읽기
 * 때문이고, 슬롯 이름으로만 보내면 이름이 다른 벤더는 "필드 누락"으로 검증에서 떨어진다
 * (NHN이 app_key를 읽는데 콘솔이 api_key로만 저장하던 결함이 정확히 이것이었다).
 * 슬롯 매핑은 흔한 형태를 위한 호환으로 함께 보낸다 — 서버가 extra 위에 얹는다.
 *
 * config(비밀 아닌 앱 설정)는 config_schema가 따로 정하므로 여기서 나누지 않는다.
 */
export function planCredential(fields: SchemaField[], values: Record<string, string>): CredentialPlan {
  const extra: Record<string, string> = {};
  const credential: Partial<Record<CredentialSlot, string>> = {};
  const missingRequired: string[] = [];

  for (const field of fields) {
    const value = (values[field.name] ?? "").trim();
    if (!value) {
      if (field.required) missingRequired.push(field.label);
      continue;
    }
    extra[field.name] = value;
    const slot = credentialSlot(field.name);
    if (slot) credential[slot] = value;
  }

  return { extra, credential, missingRequired, empty: Object.keys(extra).length === 0 };
}

/** 이 필드의 값이 어디에 저장되는지 — 화면에 그대로 밝힌다(조용히 버리지 않는다는 원칙). */
export function fieldDestination(field: SchemaField): string {
  const slot = credentialSlot(field.name);
  return slot && slot !== field.name
    ? `크리덴셜에 ${field.name}(으)로 저장 · ${slot} 슬롯에도 함께`
    : "크리덴셜에 이 이름 그대로 저장";
}

/** config 스키마 폼 값 → 배선 config. 빈 값은 보내지 않는다(서버가 기본값을 쓰게). */
export function planConfig(fields: SchemaField[], values: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const field of fields) {
    const value = (values[field.name] ?? "").trim();
    if (value) out[field.name] = value;
  }
  return out;
}
