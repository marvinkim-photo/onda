import { mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { loadCatalog } from "./catalog.controller";

const manifest = (over: Record<string, unknown> = {}) => JSON.stringify({
  manifest_version: 0,
  id: "alimtalk_nhn",
  name: "카카오 알림톡 (NHN Cloud)",
  version: "0.1.0",
  channel: "kakao_alimtalk",
  vendor: { name: "Onda" },
  license: "Apache-2.0",
  runtime: { type: "in_process_go" },
  target_types: ["phone"],
  capabilities: { content: ["template"], substitution: "variables", lifecycle_mode: "polling" },
  credentials: { schema: { type: "object", required: ["app_key"], properties: { app_key: { type: "string" } } } },
  lifecycle: { reports: ["accepted", "sent", "delivered", "failed"] },
  ...over,
});

async function dirWith(files: Record<string, string>) {
  const dir = await mkdtemp(join(tmpdir(), "onda-catalog-"));
  for (const [name, body] of Object.entries(files)) await writeFile(join(dir, name), body);
  return dir;
}

describe("loadCatalog", () => {
  it("매니페스트를 콘솔이 폼을 그릴 수 있는 형태로 내보낸다", async () => {
    const dir = await dirWith({ "nhn.json": manifest() });
    const entry = (await loadCatalog(dir))[0]!;
    expect(entry.id).toBe("alimtalk_nhn");
    expect(entry.channel).toBe("kakao_alimtalk");
    expect(entry.runtime).toBe("in_process_go");
    // 벤더마다 크리덴셜 필드가 다르므로 폼은 이 스키마로 그린다.
    expect(entry.credentials_schema).toMatchObject({ required: ["app_key"] });
    // 리포트가 "미지원"과 "0"을 구분하려면 이 목록이 필요하다.
    expect(entry.reports).toContain("delivered");
    expect(entry.reports).not.toContain("opened");
  });

  it("폴링형 커넥터는 등록할 콜백 경로가 없다", async () => {
    const dir = await dirWith({ "nhn.json": manifest() });
    const entry = (await loadCatalog(dir))[0]!;
    expect(entry.callback_path).toBeUndefined();
  });

  it("콜백형 커넥터는 등록할 경로를 알려준다", async () => {
    const dir = await dirWith({
      "cb.json": manifest({ lifecycle: { reports: ["sent"], callback: { path: "nhn/result" } } }),
    });
    const entry = (await loadCatalog(dir))[0]!;
    expect(entry.callback_path).toBe("nhn/result");
  });

  it("채널 필터는 호출자가 하지만 채널 값은 그대로 실린다", async () => {
    const dir = await dirWith({
      "a.json": manifest(),
      "b.json": manifest({ id: "email_x", channel: "email" }),
    });
    const list = await loadCatalog(dir);
    expect(list.map((c) => c.channel).sort()).toEqual(["email", "kakao_alimtalk"]);
  });

  // 깨진 파일 하나가 목록 전체를 막으면 콘솔에서 아무 벤더도 고를 수 없게 된다.
  // 기동 실패로 알리는 일은 워커가 한다.
  it("깨진 매니페스트는 건너뛰고 나머지를 돌려준다", async () => {
    const dir = await dirWith({ "ok.json": manifest(), "broken.json": "{ not json" });
    expect((await loadCatalog(dir)).map((c) => c.id)).toEqual(["alimtalk_nhn"]);
  });

  it("필수 필드가 빠진 매니페스트는 제외한다", async () => {
    const dir = await dirWith({
      "ok.json": manifest(),
      "nocred.json": manifest({ id: "no_cred", credentials: undefined }),
      "noruntime.json": manifest({ id: "no_runtime", runtime: { type: "wat" } }),
    });
    expect((await loadCatalog(dir)).map((c) => c.id)).toEqual(["alimtalk_nhn"]);
  });

  it("*.json이 아닌 파일은 읽지 않는다 — .disabled로 꺼둔 사본이 켜지면 안 된다", async () => {
    const dir = await dirWith({ "off.json.disabled": manifest(), "README.md": "# hi" });
    expect(await loadCatalog(dir)).toEqual([]);
  });

  it("디렉터리가 없으면 빈 목록 — 커넥터를 안 쓰는 배포가 깨지면 안 된다", async () => {
    expect(await loadCatalog("/nonexistent/onda/connectors")).toEqual([]);
  });
});
