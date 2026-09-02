import {
  Body,
  Controller,
  Delete,
  Get,
  Inject,
  NotFoundException,
  Param,
  ParseUUIDPipe,
  Patch,
  Post,
  Req,
  UseGuards,
} from "@nestjs/common";
import type { Pool, PoolClient } from "pg";
import { z } from "zod";
import { PG } from "../infra/infra.module";
import { SessionGuard, type SessionRequest } from "../auth/session.guard";
import { PermissionGuard } from "../authz/permission.guard";
import { RequirePermission } from "../authz/require-permission.decorator";
import { AuditService } from "../audit/audit.service";
import { assertApp, mapUnique, parse } from "./scope";

const DUP = "이미 등록된 발신프로필 키입니다";

interface SenderRow {
  id: string;
  sender_key: string;
  channel_name: string;
  status: string;
  is_default: boolean;
}

const createSchema = z.object({
  sender_key: z.string().trim().min(1).max(256),
  channel_name: z.string().trim().max(128).default(""),
  status: z.enum(["active", "disabled"]).default("active"),
  is_default: z.boolean().default(false),
});
const updateSchema = createSchema.partial().omit({ sender_key: true });

/**
 * 알림톡 발신프로필(카카오 채널) CRUD. 앱당 여러 개 — 그래서 credentials가 아니라 별도 테이블이다
 * (credentials는 UNIQUE (app_id, kind)로 kind당 1개).
 * 발신프로필 키 자체는 비밀이 아니므로 journeys:read/write로 다룬다(발송 설계의 일부).
 */
@Controller("v1/apps/:appId/alimtalk/senders")
@UseGuards(SessionGuard, PermissionGuard)
export class AlimtalkSendersController {
  constructor(
    @Inject(PG) private readonly pg: Pool,
    private readonly audit: AuditService,
  ) {}

  @Get()
  @RequirePermission("journeys:read")
  async list(@Param("appId", ParseUUIDPipe) appId: string, @Req() req: SessionRequest) {
    await assertApp(this.pg, appId, req);
    const { rows } = await this.pg.query(
      `SELECT id, sender_key, channel_name, status, is_default, created_at, updated_at
         FROM alimtalk_senders
        WHERE tenant_id = $1 AND app_id = $2
        ORDER BY is_default DESC, created_at`,
      [req.member.tenantId, appId],
    );
    return { senders: rows };
  }

  @Post()
  @RequirePermission("journeys:write")
  async create(
    @Param("appId", ParseUUIDPipe) appId: string,
    @Body() body: unknown,
    @Req() req: SessionRequest,
  ) {
    await assertApp(this.pg, appId, req);
    const data = parse(createSchema, body);
    const row = await this.inDefaultTx(appId, req, data.is_default, (client) =>
      client
        .query(
          `INSERT INTO alimtalk_senders (tenant_id, app_id, sender_key, channel_name, status, is_default)
           VALUES ($1, $2, $3, $4, $5, $6)
           RETURNING id, sender_key, channel_name, status, is_default`,
          [
            req.member.tenantId,
            appId,
            data.sender_key,
            data.channel_name,
            data.status,
            data.is_default,
          ],
        )
        .catch(mapUnique(DUP)),
    );
    await this.audit.recordAs(req.member, req.ip, "alimtalk_sender.create", {
      targetType: "alimtalk_sender",
      targetId: row.id,
      detail: { app_id: appId, sender_key: data.sender_key },
    });
    return row;
  }

  @Patch(":id")
  @RequirePermission("journeys:write")
  async update(
    @Param("appId", ParseUUIDPipe) appId: string,
    @Param("id", ParseUUIDPipe) id: string,
    @Body() body: unknown,
    @Req() req: SessionRequest,
  ) {
    await assertApp(this.pg, appId, req);
    const data = parse(updateSchema, body);
    const row = await this.inDefaultTx(appId, req, data.is_default === true, (client) =>
      client.query(
        `UPDATE alimtalk_senders
            SET channel_name = COALESCE($4, channel_name),
                status       = COALESCE($5, status),
                is_default   = COALESCE($6, is_default),
                updated_at   = now()
          WHERE tenant_id = $1 AND app_id = $2 AND id = $3
        RETURNING id, sender_key, channel_name, status, is_default`,
        [
          req.member.tenantId,
          appId,
          id,
          data.channel_name ?? null,
          data.status ?? null,
          data.is_default ?? null,
        ],
      ),
    );
    await this.audit.recordAs(req.member, req.ip, "alimtalk_sender.update", {
      targetType: "alimtalk_sender",
      targetId: id,
      detail: { app_id: appId },
    });
    return row;
  }

  /** 삭제 — 승인 템플릿 캐시(alimtalk_templates)는 FK ON DELETE CASCADE로 함께 지워진다. */
  @Delete(":id")
  @RequirePermission("journeys:write")
  async remove(
    @Param("appId", ParseUUIDPipe) appId: string,
    @Param("id", ParseUUIDPipe) id: string,
    @Req() req: SessionRequest,
  ) {
    await assertApp(this.pg, appId, req);
    const { rowCount } = await this.pg.query(
      `DELETE FROM alimtalk_senders WHERE tenant_id = $1 AND app_id = $2 AND id = $3`,
      [req.member.tenantId, appId, id],
    );
    if (!rowCount) throw new NotFoundException("발신프로필을 찾을 수 없습니다");
    await this.audit.recordAs(req.member, req.ip, "alimtalk_sender.delete", {
      targetType: "alimtalk_sender",
      targetId: id,
      detail: { app_id: appId },
    });
    return { ok: true as const };
  }

  /**
   * 기본 발신프로필은 앱당 하나여야 한다. 부분 유니크 인덱스로 강제하면 "교체" 자체가 불가능해지므로
   * (기존 해제와 신규 지정 사이에 위반), 같은 트랜잭션에서 기존 기본을 먼저 내리고 쓴다.
   */
  private async inDefaultTx(
    appId: string,
    req: SessionRequest,
    becomesDefault: boolean,
    write: (client: PoolClient) => Promise<{ rows: SenderRow[] }>,
  ): Promise<SenderRow> {
    const client = await this.pg.connect();
    try {
      await client.query("BEGIN");
      if (becomesDefault) {
        await client.query(
          `UPDATE alimtalk_senders SET is_default = false, updated_at = now()
            WHERE tenant_id = $1 AND app_id = $2 AND is_default`,
          [req.member.tenantId, appId],
        );
      }
      const { rows } = await write(client);
      if (!rows[0]) throw new NotFoundException("발신프로필을 찾을 수 없습니다");
      await client.query("COMMIT");
      return rows[0];
    } catch (err) {
      await client.query("ROLLBACK").catch(() => undefined);
      throw err;
    } finally {
      client.release();
    }
  }
}
