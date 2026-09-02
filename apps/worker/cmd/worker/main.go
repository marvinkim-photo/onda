// onda-worker — Go 단일 바이너리 + --role 플래그 (PRD-08 2장).
// roles: ingest-consumer | scheduler | trigger-matcher | segment | channel | all
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"

	"github.com/ondahq/onda/apps/worker/internal/channel"
	"github.com/ondahq/onda/apps/worker/internal/channel/alimtalk"
	"github.com/ondahq/onda/apps/worker/internal/clock"
	"github.com/ondahq/onda/apps/worker/internal/config"
	"github.com/ondahq/onda/apps/worker/internal/connector"
	"github.com/ondahq/onda/apps/worker/internal/ingest"
	"github.com/ondahq/onda/apps/worker/internal/journey"
	"github.com/ondahq/onda/apps/worker/internal/lifecycle"
	"github.com/ondahq/onda/apps/worker/internal/message"
	"github.com/ondahq/onda/apps/worker/internal/segment"
	"github.com/ondahq/onda/apps/worker/internal/trigger"
	libqueue "github.com/ondahq/onda/packages/libqueue-go"
)

var validRoles = map[string]bool{
	"ingest-consumer": true, "scheduler": true, "trigger-matcher": true,
	"segment": true, "channel": true, "all": true,
}

type workerComponent struct {
	name       string
	initialize func(context.Context) error
	run        func(context.Context) error
}

const workerComponentRetryInterval = time.Second

func main() {
	role := flag.String("role", "all", "worker 역할: ingest-consumer|scheduler|trigger-matcher|segment|channel|all")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("role", *role)
	if !validRoles[*role] {
		logger.Error("알 수 없는 role", "role", *role)
		os.Exit(2)
	}

	if err := run(*role, logger); err != nil && err != context.Canceled {
		logger.Error("worker 종료", "err", err)
		os.Exit(1)
	}
}

func run(role string, logger *slog.Logger) error {
	cfg, err := config.Load("DATABASE_URL", "REDIS_URL", "CLICKHOUSE_URL")
	if err != nil {
		return fmt.Errorf("설정 로드: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- 인프라 연결 (조립 지점 — Real clock은 여기서만) ---
	clk := clock.Real{}

	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("REDIS_URL 파싱: %w", err)
	}
	rdb := redis.NewClient(redisOpts)
	defer rdb.Close()

	pg, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("PG 연결: %w", err)
	}
	defer pg.Close()

	chOpts, err := clickhouse.ParseDSN(cfg.ClickHouseURL)
	if err != nil {
		return fmt.Errorf("CLICKHOUSE_URL 파싱: %w", err)
	}
	ch, err := clickhouse.Open(chOpts)
	if err != nil {
		return fmt.Errorf("ClickHouse 연결: %w", err)
	}
	defer ch.Close()

	g, gctx := errgroup.WithContext(ctx)

	checks := map[string]readinessCheck{
		"postgres": pg.Ping,
	}
	if role != "segment" {
		checks["redis"] = func(ctx context.Context) error { return rdb.Ping(ctx).Err() }
	}
	if role != "trigger-matcher" {
		checks["clickhouse"] = ch.Ping
	}
	probe := newReadinessProbe(checks, requiredWorkerComponents(role))

	// --- 헬스 엔드포인트 ---
	g.Go(func() error { return serveHealth(gctx, cfg.HealthAddr, logger, probe) })

	has := func(r string) bool { return role == "all" || role == r }

	hostname, _ := os.Hostname()

	if has("ingest-consumer") {
		queue := libqueue.NewConsumer(rdb, libqueue.StreamIngest, libqueue.GroupIngest, "ingest-"+hostname)
		consumer := ingest.NewConsumer(
			queue,
			libqueue.NewProducer(rdb, 0),
			ingest.NewDeduper(rdb),
			pg, ch, clk, logger.With("component", "ingest-consumer"),
		)
		startWorkerComponent(g, gctx, probe, logger, workerComponent{
			name:       "consumer_ingest",
			initialize: queue.EnsureGroup,
			run:        consumer.Run,
		})
	}

	if has("channel") {
		// 발송 수명주기 소비자 — 마스터키 불필요(복호화 없음). 커넥터·콜백·SDK 이벤트 → message_lifecycle.
		lifecycleQueue := libqueue.NewConsumer(rdb, libqueue.StreamLifecycle, libqueue.GroupLifecycle, "lifecycle-"+hostname)
		lc := lifecycle.NewConsumer(
			lifecycleQueue,
			ch, clk, logger.With("component", "lifecycle"),
		)
		startWorkerComponent(g, gctx, probe, logger, workerComponent{
			name:       "consumer_lifecycle",
			initialize: lifecycleQueue.EnsureGroup,
			run:        lc.Run,
		})

		masterKey, err := channel.LoadMasterKey()
		if err != nil {
			probe.markFailed("connector_runtime")
			probe.markFailed("consumer_push")
			probe.markFailed("consumer_email")
			probe.markFailed("consumer_message")
			probe.markFailed("poller_receipts")
			if role == "channel" {
				logger.Error("channel 역할은 마스터키 필수 — readiness 차단", "err", err)
			} else {
				logger.Warn("마스터키 미설정 — channel 역할 비활성 (ONDA_MASTER_KEY 설정 필요)", "err", err)
			}
		} else {
			// 커넥터 매니페스트 로드 — 여기에 놓인 *.json이 곧 "켜진 커넥터" 목록이다.
			// 디렉터리가 없으면 빈 목록(커넥터를 안 쓰는 배포가 기동에 실패하면 안 된다).
			manifests, merr := connector.LoadDir(connectorManifestDir())
			if merr != nil {
				return fmt.Errorf("커넥터 매니페스트 로드: %w", merr)
			}
			registry, rerr := alimtalk.NewRegistry(manifests)
			if rerr != nil {
				return fmt.Errorf("알림톡 커넥터 레지스트리: %w", rerr)
			}
			logger.Info("커넥터 등록", "manifests", len(manifests), "alimtalk", registry.IDs())

			plugin := channel.NewPushPlugin(clk)
			emailPlugin := channel.NewEmailPlugin(clk)
			alimtalkPlugin := message.NewCredentialPlugin(registry)
			pushQueue := libqueue.NewConsumer(rdb, libqueue.StreamSendPush, libqueue.GroupChannel, "channel-"+hostname)
			emailQueue := libqueue.NewConsumer(rdb, libqueue.StreamSendEmail, libqueue.GroupChannelEmail, "email-"+hostname)
			messageQueue := libqueue.NewConsumer(rdb, libqueue.StreamSendMessage, libqueue.GroupChannelMessage, "message-"+hostname)
			// 크리덴셜 kind → 검증 플러그인 (push_fcm/push_apns=push, email_*=email, alimtalk=벤더 위임)
			verifier := channel.NewVerifier(pg, map[string]channel.ChannelPlugin{
				"push_fcm":     plugin,
				"push_apns":    plugin,
				"email_smtp":   emailPlugin,
				"email_nhn":    emailPlugin,
				"email_resend": emailPlugin,
				"alimtalk":     alimtalkPlugin,
			}, masterKey, logger.With("component", "credential-verifier"))
			worker := channel.NewWorker(
				pushQueue,
				rdb, pg, ch, plugin, masterKey, clk, logger.With("component", "channel"),
			)
			emailWorker := channel.NewEmailWorker(
				emailQueue,
				rdb, pg, ch, emailPlugin, masterKey, clk, logger.With("component", "channel-email"),
			)
			probe.markReady("connector_runtime")
			g.Go(func() error { return verifier.Run(gctx) })
			startWorkerComponent(g, gctx, probe, logger, workerComponent{
				name:       "consumer_push",
				initialize: pushQueue.EnsureGroup,
				run:        worker.Run,
			})
			startWorkerComponent(g, gctx, probe, logger, workerComponent{
				name:       "consumer_email",
				initialize: emailQueue.EnsureGroup,
				run:        emailWorker.Run,
			})

			// send.message.v1 — 채널 중립 발송. 알림톡이 첫 채널이다.
			lifecycleProducer := libqueue.NewProducer(rdb, 0)
			messageWorker := message.NewWorker(pg, registry, lifecycleProducer, masterKey, clk,
				logger.With("component", "channel-message"))
			messageLoop := channel.NewSendLoop[*message.Job]("send.message", messageWorker, messageQueue,
				rdb, ch, clk, logger.With("component", "channel-message"))
			startWorkerComponent(g, gctx, probe, logger, workerComponent{
				name:       "consumer_message",
				initialize: messageQueue.EnsureGroup,
				run:        messageLoop.Run,
			})

			// 폴링형 벤더의 결과 확정기. 폴링 커넥터가 없어도 돌지만 만기 행이 없어 무해하다.
			poller := message.NewResultPoller(pg, registry, lifecycleProducer, masterKey, clk,
				logger.With("component", "receipt-poller"))
			startWorkerComponent(g, gctx, probe, logger, workerComponent{
				name:       "poller_receipts",
				initialize: func(context.Context) error { return nil },
				run:        poller.Run,
			})
		}
	}

	var sched *journey.Scheduler
	var journeyEntryQueue *libqueue.Consumer
	if has("scheduler") || has("trigger-matcher") {
		journeyEntryQueue = libqueue.NewConsumer(rdb, libqueue.StreamJourneyEntry, libqueue.GroupScheduler, "sched-"+hostname)
		sched = journey.NewScheduler(
			journeyEntryQueue,
			libqueue.NewProducer(rdb, 0),
			pg, ch, rdb, clk, "sched-"+hostname, logger.With("component", "scheduler"),
		)
	}
	if has("scheduler") {
		startWorkerComponent(g, gctx, probe, logger, workerComponent{
			name:       "consumer_journey_entry",
			initialize: journeyEntryQueue.EnsureGroup,
			run:        sched.RunEntryConsumer,
		})
		g.Go(func() error { return sched.RunTick(gctx) })
		g.Go(func() error { return sched.RunRelay(gctx) })
		g.Go(func() error { return sched.RunMaintenance(gctx) }) // 테넌트 유예 파기 (T-10)
		g.Go(func() error { return sched.RunReaper(gctx) })
	}

	if has("trigger-matcher") {
		triggerQueue := libqueue.NewConsumer(rdb, libqueue.StreamEvents, libqueue.GroupTriggerMatcher, "trig-"+hostname)
		matcher := trigger.NewMatcher(
			triggerQueue,
			libqueue.NewProducer(rdb, 0),
			rdb, pg, clk, logger.With("component", "trigger-matcher"),
		)
		matcher.SetRuntime(sched)
		startWorkerComponent(g, gctx, probe, logger, workerComponent{
			name:       "consumer_trigger",
			initialize: triggerQueue.EnsureGroup,
			run:        matcher.Run,
		})
	}

	if has("segment") {
		runner := segment.NewRunner(pg, ch, clk, logger.With("component", "segment"))
		g.Go(func() error { return runner.RunMaintenance(gctx) })
	}

	logger.Info("onda-worker 기동", "roles", roleList(role))
	return g.Wait()
}

// connectorManifestDir — 커넥터 매니페스트 위치. 컨테이너 이미지 바깥에서 볼륨으로 얹는
// 경로가 기본값이다(커넥터 설치가 재빌드를 요구하면 안 된다).
func connectorManifestDir() string {
	if dir := os.Getenv("ONDA_CONNECTOR_MANIFESTS"); dir != "" {
		return dir
	}
	return "/etc/onda/connectors"
}

func roleList(role string) string {
	if role == "all" {
		return strings.Join([]string{"ingest-consumer", "channel", "scheduler", "trigger-matcher"}, ",") + " (+미구현 stub 생략)"
	}
	return role
}

func requiredWorkerComponents(role string) []string {
	has := func(r string) bool { return role == "all" || role == r }
	components := make([]string, 0, 12)
	if has("ingest-consumer") {
		components = append(components, "consumer_ingest")
	}
	if has("channel") {
		components = append(components,
			"consumer_lifecycle", "connector_runtime", "consumer_push", "consumer_email",
			"consumer_message", "poller_receipts",
		)
	}
	if has("scheduler") {
		components = append(components, "consumer_journey_entry")
	}
	if has("trigger-matcher") {
		components = append(components, "consumer_trigger")
	}
	return components
}

func startWorkerComponent(
	g *errgroup.Group,
	ctx context.Context,
	probe *readinessProbe,
	logger *slog.Logger,
	component workerComponent,
) {
	g.Go(func() error {
		for {
			if err := component.initialize(ctx); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				probe.markFailed(component.name)
				logger.Warn("worker component 초기화 실패 — 재시도", "component", component.name, "err", err)
				if err := waitForComponentRetry(ctx); err != nil {
					return err
				}
				continue
			}

			probe.markReady(component.name)
			err := component.run(ctx)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			probe.markFailed(component.name)
			if err != nil {
				logger.Warn("worker component 종료 — 재시도", "component", component.name, "err", err)
			} else {
				logger.Warn("worker component 비정상 종료 — 재시도", "component", component.name)
			}
			if err := waitForComponentRetry(ctx); err != nil {
				return err
			}
		}
	})
}

func waitForComponentRetry(ctx context.Context) error {
	timer := time.NewTimer(workerComponentRetryInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
