import { readdir, readFile } from "node:fs/promises";
import { join } from "node:path";
import { Controller, Get, Query, UseGuards } from "@nestjs/common";
import { SessionGuard } from "../auth/session.guard";

/**
 * 커넥터 카탈로그 — 이 배포에서 쓸 수 있는 발송기 목록.
 *
 * 콘솔의 "알림톡 설정 → 벤더 선택 → 설정 입력" 흐름이 이 응답만으로 그려진다.
 * 벤더마다 크리덴셜 필드가 다르므로 폼을 손으로 짤 수 없고, manifest의 JSON Schema로 렌더한다.
 *
 * 단일 출처는 매니페스트 디렉터리다(`ONDA_CONNECTOR_MANIFESTS`, 기본 `/etc/onda/connectors`).
 * 워커도 같은 디렉터리를 읽어 레지스트리를 만든다 — 목록과 실제 발송 가능 여부가 갈리지 않게 하려면
 * 두 프로세스에 같은 디렉터리를 마운트해야 한다.
 *
 * 주의: 여기 있다고 발송이 되는 것은 아니다. in_process_go 커넥터는 워커 바이너리에 구현이
 * 포함돼 있어야 하고, 없으면 워커가 기동에서 실패한다(조용히 실패하지 않는다).
 */

const MANIFEST_DIR = process.env.ONDA_CONNECTOR_MANIFESTS ?? "/etc/onda/connectors";

/** 카탈로그로 내보내는 필드. 비밀도 아니고 운영 정보도 아니지만, 필요한 것만 고른다. */
export interface CatalogEntry {
  id: string;
  name: string;
  description?: string;
  version: string;
  channel: string;
  vendor: { name: string; url?: string; support?: string };
  tier?: string;
  runtime: "in_process_go" | "remote_http";
  /** 콘솔이 크리덴셜 입력 폼을 그리는 JSON Schema */
  credentials_schema: unknown;
  /** 비밀이 아닌 앱 단위 설정 폼 (발신번호·기본 발신프로필 등) */
  config_schema?: unknown;
  capabilities: Record<string, unknown>;
  /** 이 커넥터가 실제로 보고할 수 있는 상태. 리포트가 "미지원"과 "0"을 구분하는 근거 */
  reports: string[];
  /** 웹훅을 받아야 하는 커넥터면 등록할 경로 조각. 없으면 폴링형이라 등록할 것이 없다 */
  callback_path?: string;
  compliance?: Record<string, unknown>;
  cost?: Record<string, unknown>;
}

interface RawManifest {
  manifest_version?: number;
  id?: string;
  name?: string;
  description?: string;
  version?: string;
  channel?: string;
  vendor?: { name?: string; url?: string; support?: string };
  tier?: string;
  runtime?: { type?: string };
  credentials?: { schema?: unknown };
  config?: { schema?: unknown };
  capabilities?: Record<string, unknown>;
  lifecycle?: { reports?: string[]; callback?: { path?: string } };
  compliance?: Record<string, unknown>;
  cost?: Record<string, unknown>;
}

/** 카탈로그로 내보내기에 충분한 최소 형태인지. 깨진 매니페스트 하나가 목록 전체를 막지 않게 한다. */
function toEntry(raw: RawManifest): CatalogEntry | null {
  if (!raw.id || !raw.name || !raw.version || !raw.channel || !raw.credentials?.schema) return null;
  if (raw.runtime?.type !== "in_process_go" && raw.runtime?.type !== "remote_http") return null;
  return {
    id: raw.id,
    name: raw.name,
    description: raw.description,
    version: raw.version,
    channel: raw.channel,
    vendor: { name: raw.vendor?.name ?? "", url: raw.vendor?.url, support: raw.vendor?.support },
    tier: raw.tier,
    runtime: raw.runtime.type,
    credentials_schema: raw.credentials.schema,
    config_schema: raw.config?.schema,
    capabilities: raw.capabilities ?? {},
    reports: raw.lifecycle?.reports ?? [],
    callback_path: raw.lifecycle?.callback?.path,
    compliance: raw.compliance,
    cost: raw.cost,
  };
}

export async function loadCatalog(dir: string = MANIFEST_DIR): Promise<CatalogEntry[]> {
  let names: string[];
  try {
    names = await readdir(dir);
  } catch {
    return []; // 커넥터를 안 쓰는 배포에서 목록 조회가 실패하면 안 된다
  }
  const out: CatalogEntry[] = [];
  for (const name of names.filter((n) => n.endsWith(".json")).sort()) {
    try {
      const entry = toEntry(JSON.parse(await readFile(join(dir, name), "utf8")) as RawManifest);
      if (entry) out.push(entry);
    } catch {
      // 깨진 파일 하나가 나머지를 가리지 않게 건너뛴다. 기동 실패는 워커가 담당한다.
    }
  }
  return out;
}

/** 발송기 카탈로그 (세션 인증 — 테넌트별로 다르지 않은 배포 정보) */
@Controller("v1/connectors")
@UseGuards(SessionGuard)
export class ConnectorCatalogController {
  @Get()
  async list(@Query("channel") channel?: string) {
    const all = await loadCatalog();
    const connectors = channel ? all.filter((c) => c.channel === channel) : all;
    return { connectors };
  }
}
