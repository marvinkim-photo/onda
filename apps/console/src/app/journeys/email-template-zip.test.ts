import { describe, expect, it } from "vitest";
import { strToU8, zipSync } from "fflate";
import { createSandboxPreviewHtml, hasUnsafeEmailMarkup, importEmailTemplateZipBytes, normalizeArchivePath } from "./email-template-zip";

describe("normalizeArchivePath", () => {
  it("normalizes safe relative paths", () => {
    expect(normalizeArchivePath("template\\assets/./hero.png")).toBe("template/assets/hero.png");
  });

  it("rejects traversal and absolute paths", () => {
    expect(normalizeArchivePath("../index.html")).toBeNull();
    expect(normalizeArchivePath("/index.html")).toBeNull();
    expect(normalizeArchivePath("C:\\template\\index.html")).toBeNull();
  });
});

describe("email HTML safety", () => {
  it("preserves Onda variables while rejecting executable markup", () => {
    expect(hasUnsafeEmailMarkup("<p>안녕하세요 {{ name }}님</p>")).toBe(false);
    expect(hasUnsafeEmailMarkup("<script>alert(1)</script>")).toBe(true);
    expect(hasUnsafeEmailMarkup("<img src=x onerror=alert(1)>")).toBe(true);
    expect(hasUnsafeEmailMarkup("<a href=javascript:alert(1)>열기</a>")).toBe(true);
    expect(hasUnsafeEmailMarkup("<meta http-equiv='refresh' content='0;url=https://example.com'>")).toBe(true);
  });

  it("injects a network-blocking CSP into the sandbox document", () => {
    const preview = createSandboxPreviewHtml("<html><head><title>리뷰</title></head><body>{{ name }}</body></html>");
    expect(preview).toContain("Content-Security-Policy");
    expect(preview).toContain("default-src 'none'");
    expect(preview).toContain("form-action 'none'");
    expect(preview).toContain("{{ name }}");
  });
});

describe("ZIP entry validation", () => {
  it("requires an HTML entry", async () => {
    const zip = zipSync({ "readme.txt": strToU8("hello") });
    await expect(importEmailTemplateZipBytes(zip, "empty.zip")).rejects.toThrow("HTML 파일을 찾지 못했습니다");
  });

  it("requires index.html when several HTML files exist", async () => {
    const zip = zipSync({ "a.html": strToU8("<p>A</p>"), "b.html": strToU8("<p>B</p>") });
    await expect(importEmailTemplateZipBytes(zip, "many.zip")).rejects.toThrow("시작 파일 이름을 index.html");
  });

  it("rejects path traversal, nested ZIPs, and case-colliding paths", async () => {
    const traversal = zipSync({ "../index.html": strToU8("<p>unsafe</p>") });
    await expect(importEmailTemplateZipBytes(traversal, "traversal.zip")).rejects.toThrow("안전하지 않은 파일 경로");

    const nested = zipSync({ "index.html": strToU8("<p>safe</p>"), "assets/more.zip": strToU8("PK") });
    await expect(importEmailTemplateZipBytes(nested, "nested.zip")).rejects.toThrow("또 다른 ZIP");

    const collision = zipSync({ "index.html": strToU8("<p>safe</p>"), "assets/HERO.PNG": strToU8("a"), "assets/hero.png": strToU8("b") });
    await expect(importEmailTemplateZipBytes(collision, "collision.zip")).rejects.toThrow("같은 경로의 파일");
  });
});
