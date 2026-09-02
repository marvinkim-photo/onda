import {
  BadRequestException,
  Body,
  Controller,
  Get,
  HttpCode,
  Inject,
  Param,
  ParseUUIDPipe,
  Post,
  Query,
  Req,
  UseGuards,
} from "@nestjs/common";
import type { Pool } from "pg";
import { z } from "zod";
import { QueueProducer } from "@onda/libqueue";
import { STREAMS, type AlimtalkTemplateSyncPayload } from "@onda/queue-schemas";
import { PG, QUEUE } from "../infra/infra.module";
import { SessionGuard, type SessionRequest } from "../auth/session.guard";
import { PermissionGuard } from "../authz/permission.guard";
import { RequirePermission } from "../authz/require-permission.decorator";
import { AuditService } from "../audit/audit.service";
import { assertApp, parse } from "./scope";

/** 알림톡 채널 id — send.message.v1 · channel_connectors.channel의 값. */
const ALIMTALK_CHANNEL = "kakao_alimtalk";
/** 알림톡 크리덴셜 kind — 벤더가 아니라 채널 단위 (worker internal/channel/alimtalk/vendor.go). */
const ALIMTALK_CREDENTIAL_KIND = "alimtalk";

const listQuerySchema = z.object({
  sender_id: z.string().uuid().optional(),
});

const syncSchema = z.object({
  /** 미지정이면 기본 발신프로필, 그것도 없고 발신프로필이 하나뿐이면 그 하나. */
  sender_id: z.string().uuid().optional(),
});

interface SenderRow {
  id: string;
  sender_key: string;
  is_default: boolean;
}

/**
 * 알림톡 승인 템플릿 캐시 조회 + 동기화.
 *
 * 템플릿의 단일 출처는 카카오 승인 원본이므로 Onda는 편집하지 않고 벤더에서 읽어 캐시만 한다.
 * 따라서 생성·수정 엔드포인트가 없다.
 */
@Controller("v1/apps/:appId/alimtalk/templates")
@UseGuards(SessionGuard, PermissionGuard)
export class AlimtalkTemplatesController {
  constructor(
    @Inject(PG) private readonly pg: Pool,
    @Inject(QUEUE) private readonly queue: QueueProducer,
    private readonly audit: AuditService,
  ) {}

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
   * 벤더 승인 템플릿 동기화 요청 → 워커 잡 발행(202).
   *
   * 벤더 API 호출은 API 프로세스가 하지 않는다(크리덴셜 복호화는 발송 워커 런타임 전용, PRD-04 3장).
   * 실제 동기화는 워커(internal/templatesync)가 Vendor.ListTemplates로 수행하고 alimtalk_templates에 upsert한다.
   *
   * 202를 주기 전에 "결과가 나올 수 있는 상태인가"를 먼저 확인한다 — 발신프로필·커넥터 배선·크리덴셜 중
   * 하나라도 없으면 워커는 조용히 아무것도 못 하고 사용자는 영원히 빈 목록을 본다. 그 거짓 신호가
   * 이 엔드포인트가 P0에서 501이었던 이유이므로, 없는 것을 이름으로 지목해 400으로 돌려준다.
   */
  @Post("sync")
  @HttpCode(202)
  @RequirePermission("journeys:write")
  async sync(
    @Param("appId", ParseUUIDPipe) appId: string,
    @Body() body: unknown,
    @Req() req: SessionRequest,
  ) {
    await assertApp(this.pg, appId, req);
    const { sender_id: requestedSenderId } = parse(syncSchema, body ?? {});
    const tenantId = req.member.tenantId;

    const sender = await this.pickSender(tenantId, appId, requestedSenderId ?? null);
    const connectorId = await this.assertWired(tenantId, appId);

    const payload: AlimtalkTemplateSyncPayload = {
      app_id: appId,
      sender_id: sender.id,
      sender_key: sender.sender_key,
      connector_id: connectorId,
      requested_by: req.member.memberId,
      requested_at: new Date().toISOString(),
    };
    await this.queue.publish(STREAMS.alimtalkTemplateSync, {
      type: "alimtalk.template.sync",
      tenantId,
      appId,
      payload: payload as unknown as Record<string, unknown>,
    });
    await this.audit.recordAs(req.member, req.ip, "alimtalk_template.sync", {
      targetType: "alimtalk_sender",
      targetId: sender.id,
      detail: { app_id: appId, connector_id: connectorId },
    });
    return { accepted: true as const, sender_id: sender.id };
  }

  /**
   * 동기화 대상 발신프로필 확정. 벤더의 ListTemplates가 발신프로필(senderKey) 단위이므로
   * 요청 하나는 발신프로필 하나다. 여러 개 중 아무거나 고르면 "동기화했는데 그 채널 것만 안 온다"가
   * 되므로, 고를 근거(명시 지정·기본 발신프로필·유일)가 없으면 되묻는다.
   */
  private async pickSender(
    tenantId: string,
    appId: string,
    requestedSenderId: string | null,
  ): Promise<SenderRow> {
    const { rows } = await this.pg.query<SenderRow>(
      `SELECT id, sender_key, is_default
         FROM alimtalk_senders
        WHERE tenant_id = $1 AND app_id = $2 AND ($3::uuid IS NULL OR id = $3)
        ORDER BY is_default DESC, created_at`,
      [tenantId, appId, requestedSenderId],
    );
    if (requestedSenderId) {
      const row = rows[0];
      if (!row) throw new BadRequestException("지정한 발신프로필(sender_id)이 이 앱에 없습니다");
      return row;
    }
    const first = rows[0];
    if (!first) {
      throw new BadRequestException(
        "동기화할 발신프로필이 없습니다. 알림톡 발신프로필(카카오 채널)을 먼저 등록해 주세요",
      );
    }
    if (first.is_default || rows.length === 1) return first;
    throw new BadRequestException(
      "발신프로필이 여러 개입니다. sender_id를 지정하거나 기본 발신프로필을 설정해 주세요",
    );
  }

  /**
   * 워커가 실제로 벤더를 호출할 수 있는 배선인지 확인하고 커넥터 id를 돌려준다.
   * 워커의 해석 경로(internal/message/resolver.go)와 같은 두 테이블을 같은 조건으로 본다.
   */
  private async assertWired(tenantId: string, appId: string): Promise<string> {
    const { rows: wiring } = await this.pg.query<{ connector_id: string; enabled: boolean }>(
      `SELECT connector_id, enabled FROM channel_connectors
        WHERE tenant_id = $1 AND app_id = $2 AND channel = $3`,
      [tenantId, appId, ALIMTALK_CHANNEL],
    );
    const wired = wiring[0];
    if (!wired) {
      throw new BadRequestException(
        `이 앱에는 알림톡(${ALIMTALK_CHANNEL}) 커넥터 배선이 없습니다. 커넥터를 먼저 연결해 주세요`,
      );
    }
    if (!wired.enabled) {
      throw new BadRequestException(
        `알림톡 커넥터 배선(${wired.connector_id})이 비활성 상태입니다. 활성화 후 다시 시도해 주세요`,
      );
    }
    // 크리덴셜까지 봐야 202가 참이 된다 — 워커는 verified 크리덴셜만 복호화한다(resolver.go).
    const { rows: creds } = await this.pg.query<{ status: string }>(
      `SELECT status FROM credentials WHERE tenant_id = $1 AND app_id = $2 AND kind = $3`,
      [tenantId, appId, ALIMTALK_CREDENTIAL_KIND],
    );
    const cred = creds[0];
    if (!cred) {
      throw new BadRequestException(
        "알림톡 크리덴셜이 등록되지 않았습니다. 벤더 API 키를 먼저 등록해 주세요",
      );
    }
    if (cred.status !== "verified") {
      throw new BadRequestException(
        `알림톡 크리덴셜이 아직 검증되지 않았습니다 (status=${cred.status})`,
      );
    }
    return wired.connector_id;
  }
}
