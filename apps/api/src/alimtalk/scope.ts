import { BadRequestException, NotFoundException } from "@nestjs/common";
import type { Pool } from "pg";
import type { z } from "zod";
import type { SessionRequest } from "../auth/session.guard";

/**
 * 알림톡 모듈 공통 헬퍼. 테넌트 격리·zod 파싱·PG 유니크 충돌 매핑을 한 곳에 모은다
 * (credentials·email-templates 컨트롤러의 private 헬퍼와 동일 동작).
 */

/** 테넌트 격리 — 타 테넌트 앱 접근은 404 (존재 여부 비노출, PRD-06 8장) */
export async function assertApp(pg: Pool, appId: string, req: SessionRequest): Promise<void> {
  const { rowCount } = await pg.query(`SELECT 1 FROM apps WHERE id = $1 AND tenant_id = $2`, [
    appId,
    req.member.tenantId,
  ]);
  if (!rowCount) throw new NotFoundException("앱을 찾을 수 없습니다");
}

/** zod 출력 타입(기본값 적용 후)을 그대로 돌려준다 — 입력 타입으로 좁혀지면 안 된다. */
export function parse<S extends z.ZodTypeAny>(schema: S, body: unknown): z.output<S> {
  const r = schema.safeParse(body);
  if (!r.success) throw new BadRequestException(r.error.flatten());
  return r.data as z.output<S>;
}

/** PG 23505(unique_violation)를 사용자 메시지가 있는 400으로 바꾼다. */
export function mapUnique(message: string) {
  return (e: unknown): never => {
    if (e && typeof e === "object" && "code" in e && (e as { code: string }).code === "23505") {
      throw new BadRequestException(message);
    }
    throw e;
  };
}
