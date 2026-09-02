import {
  Controller,
  Get,
  Inject,
  NotImplementedException,
  Param,
  ParseUUIDPipe,
  Post,
  Query,
  Req,
  UseGuards,
} from "@nestjs/common";
import type { Pool } from "pg";
import { z } from "zod";
import { PG } from "../infra/infra.module";
import { SessionGuard, type SessionRequest } from "../auth/session.guard";
import { PermissionGuard } from "../authz/permission.guard";
import { RequirePermission } from "../authz/require-permission.decorator";
import { assertApp, parse } from "./scope";

const listQuerySchema = z.object({
  sender_id: z.string().uuid().optional(),
});

/**
 * 알림톡 승인 템플릿 캐시 조회 + 동기화.
 *
 * 템플릿의 단일 출처는 카카오 승인 원본이므로 Onda는 편집하지 않고 벤더에서 읽어 캐시만 한다.
 * 따라서 생성·수정 엔드포인트가 없다.
 */
@Controller("v1/apps/:appId/alimtalk/templates")
@UseGuards(SessionGuard, PermissionGuard)
export class AlimtalkTemplatesController {
  constructor(@Inject(PG) private readonly pg: Pool) {}

  @Get()
  @RequirePermission("journeys:read")
  async list(
    @Param("appId", ParseUUIDPipe) appId: string,
    @Query() query: unknown,
    @Req() req: SessionRequest,
  ) {
    await assertApp(this.pg, appId, req);
    const { sender_id: senderId } = parse(listQuerySchema, query ?? {});
    const { rows } = await this.pg.query(
      `SELECT id, sender_id, template_code, name, content, message_type, emphasize_type,
              variables, buttons, quick_replies, status, vendor_status, synced_at, updated_at
         FROM alimtalk_templates
        WHERE tenant_id = $1 AND app_id = $2 AND ($3::uuid IS NULL OR sender_id = $3)
        ORDER BY template_code`,
      [req.member.tenantId, appId, senderId ?? null],
    );
    return { templates: rows };
  }

  /**
   * 벤더 승인 템플릿 동기화. P0 미구현 — 501.
   *
   * 벤더 API 호출은 API 프로세스가 하지 않는다(크리덴셜 복호화는 발송 워커 런타임 전용, PRD-04 3장).
   * 실제 동기화는 워커가 Vendor.ListTemplates로 수행해야 하는데, 이를 받을 큐 컨슈머가 아직 없다.
   * 소비자 없는 큐 메시지를 발행하고 202를 돌려주면 "요청됐다"는 거짓 신호가 되므로 501로 정직하게 막는다.
   * TODO(P1): alimtalk.template.sync 스트림 + 워커 컨슈머 추가 후 202 + 잡 발행으로 교체.
   */
  @Post("sync")
  @RequirePermission("journeys:write")
  async sync(@Param("appId", ParseUUIDPipe) appId: string, @Req() req: SessionRequest) {
    await assertApp(this.pg, appId, req);
    throw new NotImplementedException(
      "템플릿 동기화는 아직 지원하지 않습니다 (P1). 현재는 벤더 콘솔에서 승인된 템플릿 코드를 저니에 직접 입력해 주세요.",
    );
  }
}
