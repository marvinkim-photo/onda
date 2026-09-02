import {
  Body,
  Controller,
  ForbiddenException,
  Get,
  Inject,
  NotFoundException,
  Param,
  ParseUUIDPipe,
  Put,
  Req,
  UseGuards,
} from "@nestjs/common";
import type { Pool } from "pg";
import { z } from "zod";
import { PG } from "../infra/infra.module";
import { SessionGuard, type SessionRequest } from "../auth/session.guard";
import { AuditService } from "../audit/audit.service";
import { assertApp, parse } from "./scope";

/** send.message.v1의 channel 값. 커넥터 배선의 키다. */
const CHANNEL_RE = /^[a-z][a-z0-9_]{1,63}$/;

/**
 * config는 비밀이 아닌 앱 단위 설정(발신번호·기본 발신프로필 등)만 담는다.
 * 비밀은 credentials(봉투 암호화)에만 저장하므로, 여기에 키를 넣지 않도록 콘솔이 폼을 분리한다.
 */
const upsertSchema = z.object({
  connector_id: z
    .string()
    .regex(CHANNEL_RE, "connector_id는 ^[a-z][a-z0-9_]{1,63}$ 형식이어야 합니다"),
  config: z.record(z.unknown()).default({}),
  enabled: z.boolean().default(true),
});

/**
 * 채널 → 커넥터 배선 (channel_connectors).
 *
 * 크리덴셜과 같은 민감도(어느 벤더로 고객 메시지가 나가는지를 결정)라 Owner/Admin만 쓴다.
 * 크리덴셜 컨트롤러와 동일하게 SessionGuard + 인라인 역할 검사를 쓴다.
 */
@Controller("v1/apps/:appId/channels/:channel/connector")
@UseGuards(SessionGuard)
export class ChannelConnectorsController {
  constructor(
    @Inject(PG) private readonly pg: Pool,
    private readonly audit: AuditService,
  ) {}

  @Get()
  async get(
    @Param("appId", ParseUUIDPipe) appId: string,
    @Param("channel") channel: string,
    @Req() req: SessionRequest,
  ) {
    await assertApp(this.pg, appId, req);
    const { rows } = await this.pg.query(
      `SELECT id, channel, connector_id, config, enabled, created_at, updated_at
         FROM channel_connectors
        WHERE tenant_id = $1 AND app_id = $2 AND channel = $3`,
      [req.member.tenantId, appId, channel],
    );
    if (!rows[0]) throw new NotFoundException("배선된 커넥터가 없습니다");
    return rows[0];
  }

  /** 배선 등록/교체 — 채널당 1개 (UNIQUE (app_id, channel)) */
  @Put()
  async upsert(
    @Param("appId", ParseUUIDPipe) appId: string,
    @Param("channel") channel: string,
    @Body() body: unknown,
    @Req() req: SessionRequest,
  ) {
    await assertApp(this.pg, appId, req);
    if (!CHANNEL_RE.test(channel)) {
      throw new NotFoundException("알 수 없는 채널입니다");
    }
    if (!["owner", "admin"].includes(req.member.role)) {
      throw new ForbiddenException("커넥터 배선은 Owner/Admin만 변경할 수 있습니다");
    }
    const data = parse(upsertSchema, body);
    const { rows } = await this.pg.query(
      `INSERT INTO channel_connectors (tenant_id, app_id, channel, connector_id, config, enabled)
       VALUES ($1, $2, $3, $4, $5, $6)
       ON CONFLICT (app_id, channel) DO UPDATE SET
         connector_id = EXCLUDED.connector_id, config = EXCLUDED.config,
         enabled = EXCLUDED.enabled, updated_at = now()
       RETURNING id, channel, connector_id, config, enabled`,
      [
        req.member.tenantId,
        appId,
        channel,
        data.connector_id,
        JSON.stringify(data.config),
        data.enabled,
      ],
    );
    await this.audit.recordAs(req.member, req.ip, "channel_connector.upsert", {
      targetType: "channel_connector",
      targetId: `${appId}:${channel}`,
      detail: { app_id: appId, channel, connector_id: data.connector_id, enabled: data.enabled },
    });
    return rows[0];
  }
}
