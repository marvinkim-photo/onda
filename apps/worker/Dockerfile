# onda-worker + onda-migrate + 운영 CLI (Go distroless)
# 빌드 컨텍스트: 저장소 루트 (go.work·packages 접근 필요)
FROM golang:1.25 AS build
# GO_TAGS — 선택적 빌드 태그. 기본 빌드는 비어 있다.
# `onda_mock`을 주면 목 알림톡 커넥터가 포함된다(E2E 전용 — 운영에는 쓰지 않는다).
ARG GO_TAGS=""
WORKDIR /src
# go.work 기반 — worker와 libqueue-go 모듈 함께
COPY go.work ./
COPY apps/worker/go.mod apps/worker/go.sum ./apps/worker/
COPY packages/libqueue-go/go.mod packages/libqueue-go/go.sum ./packages/libqueue-go/
RUN cd apps/worker && go mod download
COPY apps/worker ./apps/worker
COPY packages/libqueue-go ./packages/libqueue-go
RUN cd apps/worker && CGO_ENABLED=0 go build -tags "$GO_TAGS" -o /out/onda-worker ./cmd/worker \
 && CGO_ENABLED=0 go build -o /out/onda-migrate ./cmd/migrate \
 && CGO_ENABLED=0 go build -o /out/onda-seed ./cmd/seed \
 && CGO_ENABLED=0 go build -o /out/onda-dlq ./cmd/dlq

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/onda-worker /onda-worker
COPY --from=build /out/onda-migrate /onda-migrate
COPY --from=build /out/onda-seed /onda-seed
COPY --from=build /out/onda-dlq /onda-dlq
# 마이그레이터가 참조할 SQL (migrator는 /db 인자로 실행)
COPY db /db
COPY LICENSE NOTICE TRADEMARKS.md THIRD_PARTY_NOTICES.md /licenses/onda/
COPY docs-public/LICENSING.md /licenses/onda/LICENSING.md
USER nonroot:nonroot
ENTRYPOINT ["/onda-worker"]
