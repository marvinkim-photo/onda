import { unzip, type Unzipped } from "fflate";

const MAX_ZIP_BYTES = 5 * 1024 * 1024;
const MAX_ENTRY_BYTES = 8 * 1024 * 1024;
const MAX_EXPANDED_BYTES = 15 * 1024 * 1024;
const MAX_FILE_COUNT = 80;
const MAX_FINAL_HTML_CHARS = 1_000_000;

const textDecoder = new TextDecoder("utf-8", { fatal: true });

export interface ImportedEmailTemplate {
  archiveName: string;
  archiveSize: number;
  entryPath: string;
  fileCount: number;
  imageCount: number;
  html: string;
  previewHtml: string;
  files: string[];
  warnings: string[];
}

export class EmailTemplateZipError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "EmailTemplateZipError";
  }
}

function archiveError(message: string): EmailTemplateZipError {
  return new EmailTemplateZipError(message);
}

export function normalizeArchivePath(input: string): string | null {
  const slashed = input.replaceAll("\\", "/");
  if (!slashed || slashed.startsWith("/") || /^[a-zA-Z]:\//.test(slashed)) return null;
  const parts = slashed.split("/");
  if (parts.some((part) => part === "..")) return null;
  const normalized = parts.filter((part) => part && part !== ".").join("/");
  return normalized || null;
}

function isMetadataPath(path: string): boolean {
  return path.startsWith("__MACOSX/") || path.endsWith("/.DS_Store") || path === ".DS_Store";
}

function unzipArchive(data: Uint8Array): Promise<Map<string, Uint8Array>> {
  let fileCount = 0;
  let expandedBytes = 0;
  let rejected: EmailTemplateZipError | null = null;

  return new Promise((resolve, reject) => {
    try {
      unzip(data, {
        filter: (entry) => {
          const path = normalizeArchivePath(entry.name);
          if (!path) {
            rejected ??= archiveError("ZIP 안에 안전하지 않은 파일 경로가 있습니다.");
            return false;
          }
          if (entry.name.endsWith("/") || isMetadataPath(path)) return false;
          fileCount += 1;
          expandedBytes += entry.originalSize;
          if (fileCount > MAX_FILE_COUNT) {
            rejected ??= archiveError(`ZIP에는 파일을 최대 ${MAX_FILE_COUNT}개까지 넣을 수 있습니다.`);
            return false;
          }
          if (entry.originalSize > MAX_ENTRY_BYTES) {
            rejected ??= archiveError("ZIP 안에 8MB보다 큰 파일이 있습니다.");
            return false;
          }
          if (expandedBytes > MAX_EXPANDED_BYTES) {
            rejected ??= archiveError("압축을 푼 파일의 전체 크기는 15MB 이하여야 합니다.");
            return false;
          }
          if (entry.originalSize > 1024 * 1024 && entry.size > 0 && entry.originalSize / entry.size > 100) {
            rejected ??= archiveError("압축률이 지나치게 높은 파일이 있어 안전하게 열지 않았습니다.");
            return false;
          }
          if (/\.zip$/i.test(path)) {
            rejected ??= archiveError("ZIP 안에 또 다른 ZIP 파일을 넣을 수 없습니다.");
            return false;
          }
          return true;
        },
      }, (error, entries: Unzipped) => {
        if (rejected) {
          reject(rejected);
          return;
        }
        if (error) {
          reject(archiveError("ZIP 파일을 열 수 없습니다. 손상되지 않은 ZIP인지 확인해 주세요."));
          return;
        }
        const files = new Map<string, Uint8Array>();
        const caseInsensitivePaths = new Set<string>();
        for (const [rawPath, bytes] of Object.entries(entries)) {
          const path = normalizeArchivePath(rawPath);
          if (!path || isMetadataPath(path)) continue;
          const foldedPath = path.toLocaleLowerCase("en-US");
          if (files.has(path) || caseInsensitivePaths.has(foldedPath)) {
            reject(archiveError("ZIP 안에 같은 경로의 파일이 두 개 이상 있습니다."));
            return;
          }
          files.set(path, bytes);
          caseInsensitivePaths.add(foldedPath);
        }
        resolve(files);
      });
    } catch {
      reject(archiveError("ZIP 파일을 열 수 없습니다. 표준 ZIP 형식인지 확인해 주세요."));
    }
  });
}

function pickEntryPath(paths: string[]): string {
  const htmlFiles = paths.filter((path) => /\.html?$/i.test(path));
  if (htmlFiles.length === 0) throw archiveError("ZIP 안에서 HTML 파일을 찾지 못했습니다.");
  const indexFiles = htmlFiles.filter((path) => /(^|\/)index\.html?$/i.test(path));
  if (indexFiles.length === 0 && htmlFiles.length > 1) {
    throw archiveError("여러 HTML 파일이 있습니다. 시작 파일 이름을 index.html로 바꿔 주세요.");
  }
  const candidates = indexFiles.length > 0 ? indexFiles : htmlFiles;
  return candidates.sort((a, b) => a.split("/").length - b.split("/").length || a.localeCompare(b))[0]!;
}

function dirname(path: string): string {
  const index = path.lastIndexOf("/");
  return index < 0 ? "" : path.slice(0, index);
}

function resolveArchiveReference(fromPath: string, reference: string): string | null {
  const trimmed = reference.trim();
  if (!trimmed || trimmed.startsWith("#") || /^(?:data:|blob:|https?:|mailto:|tel:|cid:|\/\/)/i.test(trimmed)) return null;
  const withoutSuffix = trimmed.split(/[?#]/, 1)[0] ?? "";
  let decoded = withoutSuffix;
  try { decoded = decodeURIComponent(withoutSuffix); } catch { /* Keep the original path. */ }
  const base = decoded.startsWith("/") ? decoded.slice(1) : [dirname(fromPath), decoded].filter(Boolean).join("/");
  return normalizeArchivePath(base);
}

function mimeForPath(path: string): string | null {
  const extension = path.split(".").pop()?.toLowerCase();
  switch (extension) {
    case "png": return "image/png";
    case "jpg":
    case "jpeg": return "image/jpeg";
    case "gif": return "image/gif";
    case "webp": return "image/webp";
    case "woff": return "font/woff";
    case "woff2": return "font/woff2";
    case "ttf": return "font/ttf";
    case "otf": return "font/otf";
    default: return null;
  }
}

function bytesToBase64(bytes: Uint8Array): string {
  let binary = "";
  const chunkSize = 0x8000;
  for (let offset = 0; offset < bytes.length; offset += chunkSize) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + chunkSize));
  }
  return btoa(binary);
}

function dataUri(path: string, bytes: Uint8Array): string | null {
  const mime = mimeForPath(path);
  return mime ? `data:${mime};base64,${bytesToBase64(bytes)}` : null;
}

export function hasUnsafeEmailMarkup(html: string): boolean {
  return /<\s*(?:script|iframe|object|embed|applet|form)\b/i.test(html)
      || /\son[a-z]+\s*=/i.test(html)
      || /javascript\s*:/i.test(html)
      || /<meta\b[^>]*http-equiv\s*=\s*["']?refresh/i.test(html);
}

function assertSafeMarkup(html: string): void {
  if (hasUnsafeEmailMarkup(html)) {
    throw archiveError("실행 코드나 폼이 포함된 HTML은 사용할 수 없습니다. 스크립트를 제거해 주세요.");
  }
}

function rewriteCssAssets(css: string, fromPath: string, files: Map<string, Uint8Array>, warnings: Set<string>): string {
  const withoutImports = css.replace(/@import\s+(?:url\()?[^;]+;?/gi, () => {
    warnings.add("외부 스타일 가져오기를 제거했습니다.");
    return "";
  });
  return withoutImports.replace(/url\(\s*(["']?)(.*?)\1\s*\)/gi, (full, _quote: string, rawReference: string) => {
    const reference = rawReference.trim();
    if (!reference || reference.startsWith("data:") || reference.startsWith("#")) return full;
    const path = resolveArchiveReference(fromPath, reference);
    if (!path) {
      warnings.add("ZIP 밖의 이미지·폰트 주소를 미리보기에서 차단했습니다.");
      return "url(\"\")";
    }
    const bytes = files.get(path);
    const uri = bytes && dataUri(path, bytes);
    if (!uri) {
      warnings.add(`${reference} 파일을 찾지 못해 표시하지 않았습니다.`);
      return "url(\"\")";
    }
    return `url(\"${uri}\")`;
  });
}

function buildStandaloneHtml(rawHtml: string, entryPath: string, files: Map<string, Uint8Array>) {
  assertSafeMarkup(rawHtml);
  if (typeof DOMParser === "undefined") throw archiveError("이 브라우저에서는 HTML 미리보기를 만들 수 없습니다.");

  const warnings = new Set<string>();
  const parser = new DOMParser();
  const document = parser.parseFromString(rawHtml, "text/html");

  document.querySelectorAll("script, iframe, object, embed, applet, form, base, meta[http-equiv='refresh' i]").forEach((node) => node.remove());
  document.querySelectorAll("*").forEach((element) => {
    for (const attribute of [...element.attributes]) {
      if (/^on/i.test(attribute.name) || attribute.name.toLowerCase() === "srcdoc") element.removeAttribute(attribute.name);
    }
    const style = element.getAttribute("style");
    if (style) element.setAttribute("style", rewriteCssAssets(style, entryPath, files, warnings));
  });

  document.querySelectorAll<HTMLLinkElement>("link[rel~='stylesheet' i]").forEach((link) => {
    const reference = link.getAttribute("href") ?? "";
    const path = resolveArchiveReference(entryPath, reference);
    const bytes = path ? files.get(path) : undefined;
    if (!path || !bytes) {
      warnings.add("ZIP 밖의 스타일시트를 제거했습니다.");
      link.remove();
      return;
    }
    const style = document.createElement("style");
    try {
      style.textContent = rewriteCssAssets(textDecoder.decode(bytes), path, files, warnings);
    } catch {
      throw archiveError(`${path} 파일은 UTF-8 CSS여야 합니다.`);
    }
    link.replaceWith(style);
  });

  document.querySelectorAll("style").forEach((style) => {
    style.textContent = rewriteCssAssets(style.textContent ?? "", entryPath, files, warnings);
  });

  document.querySelectorAll<HTMLElement>("img[src], source[src], [background]").forEach((element) => {
    const attribute = element.hasAttribute("src") ? "src" : "background";
    const reference = element.getAttribute(attribute) ?? "";
    if (reference.startsWith("data:") || reference.startsWith("cid:")) return;
    const path = resolveArchiveReference(entryPath, reference);
    const bytes = path ? files.get(path) : undefined;
    const uri = path && bytes ? dataUri(path, bytes) : null;
    if (!uri) {
      warnings.add("ZIP 밖의 이미지를 미리보기에서 차단했습니다.");
      element.removeAttribute(attribute);
      return;
    }
    element.setAttribute(attribute, uri);
  });
  document.querySelectorAll("[srcset]").forEach((element) => element.removeAttribute("srcset"));

  document.querySelectorAll<HTMLAnchorElement>("a[href]").forEach((anchor) => {
    const href = anchor.getAttribute("href")?.trim() ?? "";
    if (!/^(?:https?:|mailto:|tel:|#)/i.test(href)) anchor.removeAttribute("href");
    anchor.setAttribute("rel", "noopener noreferrer");
  });

  if (!document.head.querySelector("meta[charset]")) {
    const charset = document.createElement("meta");
    charset.setAttribute("charset", "utf-8");
    document.head.prepend(charset);
  }
  return { html: `<!doctype html>\n${document.documentElement.outerHTML}`, warnings: [...warnings] };
}

export function createSandboxPreviewHtml(html: string): string {
  const csp = `<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src data:; style-src 'unsafe-inline' data:; font-src data:; base-uri 'none'; form-action 'none'">`;
  if (/<head(?:\s[^>]*)?>/i.test(html)) return html.replace(/<head(?:\s[^>]*)?>/i, (head) => `${head}${csp}`);
  return `<!doctype html><html><head>${csp}<meta charset="utf-8"></head><body>${html}</body></html>`;
}

export async function importEmailTemplateZipBytes(data: Uint8Array, archiveName: string, archiveSize = data.byteLength): Promise<ImportedEmailTemplate> {
  if (archiveSize > MAX_ZIP_BYTES || data.byteLength > MAX_ZIP_BYTES) {
    throw archiveError("ZIP 파일은 5MB 이하여야 합니다.");
  }
  const files = await unzipArchive(data);
  if (files.size === 0) throw archiveError("ZIP 안에 사용할 수 있는 파일이 없습니다.");
  const paths = [...files.keys()].sort();
  const entryPath = pickEntryPath(paths);
  let rawHtml: string;
  try {
    rawHtml = textDecoder.decode(files.get(entryPath)!);
  } catch {
    throw archiveError(`${entryPath} 파일은 UTF-8 HTML이어야 합니다.`);
  }
  const standalone = buildStandaloneHtml(rawHtml, entryPath, files);
  if (standalone.html.length > MAX_FINAL_HTML_CHARS) {
    throw archiveError("이미지를 포함한 최종 HTML은 1,000,000자 이하여야 합니다.");
  }
  const imageCount = paths.filter((path) => /^image\//.test(mimeForPath(path) ?? "")).length;
  return {
    archiveName,
    archiveSize,
    entryPath,
    fileCount: files.size,
    imageCount,
    html: standalone.html,
    previewHtml: createSandboxPreviewHtml(standalone.html),
    files: paths,
    warnings: standalone.warnings,
  };
}

export async function importEmailTemplateZip(file: File): Promise<ImportedEmailTemplate> {
  if (!/\.zip$/i.test(file.name)) throw archiveError(".zip 파일을 선택해 주세요.");
  if (file.size > MAX_ZIP_BYTES) throw archiveError("ZIP 파일은 5MB 이하여야 합니다.");
  return importEmailTemplateZipBytes(new Uint8Array(await file.arrayBuffer()), file.name, file.size);
}
