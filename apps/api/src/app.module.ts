import { Module } from "@nestjs/common";
import { InfraModule } from "./infra/infra.module";
import { HealthController } from "./health/health.controller";
import { ApiKeyGuard } from "./auth/api-key.guard";
import { ApiKeyService } from "./auth/api-key.service";
import { AuthController } from "./auth/auth.controller";
import { BootstrapController } from "./auth/bootstrap.controller";
import { AuthService } from "./auth/auth.service";
import { SessionService } from "./auth/session.service";
import { TotpService } from "./auth/totp.service";
import { TotpController, MemberTotpController } from "./auth/totp.controller";
import { AuditService } from "./audit/audit.service";
import { AuditController } from "./audit/audit.controller";
import { TenantController } from "./tenant/tenant.controller";
import { EmailTemplatesController } from "./email/email-templates.controller";
import { TestEmailController } from "./email/test-email.controller";
import { ResendWebhookController } from "./email/resend-webhook.controller";
import { UninstallController } from "./messaging/uninstall.controller";
import {
  AlimtalkSendersController,
  AlimtalkTemplatesController,
  ChannelConnectorsController,
} from "./alimtalk";
import { MembersController } from "./members/members.controller";
import { MembersService } from "./members/members.service";
import { IngestionService } from "./ingestion/ingestion.service";
import { TrackController } from "./ingestion/track.controller";
import { CredentialsController } from "./credentials/credentials.controller";
import { TestPushController } from "./messaging/test-push.controller";
import { MessageLogController } from "./messaging/message-log.controller";
import { UsersController } from "./users/users.controller";
import { AnalyticsController } from "./analytics/analytics.controller";
import { DataController } from "./data/data.controller";
import { SessionGuard } from "./auth/session.guard";
import { AppsController } from "./apps/apps.controller";
import { AppSettingsController } from "./apps/app-settings.controller";
import { SegmentsController } from "./segments/segments.controller";
import { JourneysController } from "./journeys/journeys.controller";
import { RateLimitGuard } from "./rate-limit/rate-limit.guard";
import { RateLimitService } from "./rate-limit/rate-limit.service";
import { PermissionGuard } from "./authz/permission.guard";

@Module({
  imports: [InfraModule],
  controllers: [
    HealthController,
    AuthController,
    BootstrapController,
    TotpController,
    MemberTotpController,
    AuditController,
    TenantController,
    MembersController,
    TrackController,
    CredentialsController,
    TestPushController,
    EmailTemplatesController,
    TestEmailController,
    ResendWebhookController,
    UninstallController,
    ChannelConnectorsController,
    AlimtalkSendersController,
    AlimtalkTemplatesController,
    AppsController,
    AppSettingsController,
    SegmentsController,
    JourneysController,
    MessageLogController,
    UsersController,
    AnalyticsController,
    DataController,
  ],
  providers: [
    ApiKeyGuard,
    SessionGuard,
    PermissionGuard,
    RateLimitGuard,
    RateLimitService,
    ApiKeyService,
    AuthService,
    SessionService,
    TotpService,
    AuditService,
    MembersService,
    IngestionService,
  ],
})
export class AppModule {}
