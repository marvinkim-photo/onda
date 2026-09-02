#!/usr/bin/env bash
# CLAUDE.md 절대 규칙의 기계 강제 (DEV-sub-08 §1).
# CI와 로컬 양쪽에서 실행 가능. 위반 발견 시 exit 1.
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0

# 검사 대상 소스 파일 (생성 코드·의존성 제외)
src_files() {
  find apps packages -type f \( -name '*.ts' -o -name '*.tsx' -o -name '*.go' \) \
    ! -path '*/node_modules/*' ! -path '*/dist/*' ! -path '*/.next/*' \
    ! -path '*/generated/*' ! -name '*_gen.go' ! -name '*.gen.ts' \
    ! -name '*.d.ts' 2>/dev/null || true
}

# 규칙 1: 파일 1,000라인 제한 (생성 코드 제외)
while IFS= read -r f; do
  [ -z "$f" ] && continue
  lines=$(wc -l <"$f")
  if [ "$lines" -gt 1000 ]; then
    echo "RULE-1 위반: $f — ${lines}라인 (제한 1,000)"
    fail=1
  fi
done < <(src_files)

# 규칙 2: libqueue 외부에서 Redis Streams 직접 호출 금지
# (xadd/xreadgroup/xack/xautoclaim/xgroup 호출을 libqueue 패키지 밖에서 금지)
while IFS= read -r f; do
  [ -z "$f" ] && continue
  case "$f" in
    packages/libqueue-ts/*|packages/libqueue-go/*) continue ;;
  esac
  if grep -nEi '\.\s*x(add|readgroup|ack|autoclaim|group)' "$f" >/dev/null; then
    echo "RULE-2 위반: $f — Redis Streams 직접 호출 (libqueue 경유 필수)"
    grep -nEi '\.\s*x(add|readgroup|ack|autoclaim|group)' "$f" | head -3
    fail=1
  fi
done < <(src_files)

# 규칙 3: Go에서 time.Now() 직접 호출 금지 (주입 Clock 강제)
# 예외: clock 패키지 자신, *_test.go, 조립 지점(cmd/worker),
#       시간가속 불필요한 일회성 CLI 운영 도구(cmd/seed·cmd/loadgen)
while IFS= read -r f; do
  [ -z "$f" ] && continue
  case "$f" in
    *ated/*|*/clock/*|*_test.go) continue ;;
    */cmd/worker/*|*/cmd/seed/*|*/cmd/loadgen/*) continue ;;
  esac
  # 주석은 제외한다(sed로 //~줄끝 제거 — 행 수는 보존되므로 줄번호가 맞다).
  # 규칙 자체를 설명하는 주석이 위반으로 잡히던 오탐을 막는다.
  if [[ "$f" == *.go ]] && sed 's://.*::' "$f" | grep -n 'time\.Now()' >/dev/null; then
    echo "RULE-3 위반: $f — time.Now() 직접 호출 (clock.Clock 주입 사용)"
    sed 's://.*::' "$f" | grep -n 'time\.Now()' | head -3
    fail=1
  fi
done < <(src_files)

# 규칙 4: 콘솔에서 수기 fetch 금지 (openapi 생성 클라이언트만)
while IFS= read -r f; do
  [ -z "$f" ] && continue
  case "$f" in
    apps/console/*) ;;
    *) continue ;;
  esac
  if grep -nE '(^|[^.a-zA-Z])fetch\(' "$f" >/dev/null; then
    echo "RULE-4 위반: $f — 콘솔 수기 fetch (openapi 생성 클라이언트 사용)"
    grep -nE '(^|[^.a-zA-Z])fetch\(' "$f" | head -3
    fail=1
  fi
done < <(src_files)

# 규칙 5: 테넌트 격리 — 모든 PG/CH 쿼리에 tenant_id 필터 (정적 스캔).
#          정당한 예외는 scripts/tenant-scan-allowlist.txt에 사유와 함께 등록.
if command -v python3 >/dev/null 2>&1; then
  if ! python3 scripts/tenant-scan.py; then
    echo "RULE-5 위반: 위 쿼리에 tenant_id 필터가 없습니다 (allowlist 미등록)"
    fail=1
  fi
else
  echo "RULE-5 스킵: python3 없음 (CI에서는 반드시 실행되어야 함)"
fi

if [ "$fail" -ne 0 ]; then
  echo ""
  echo "절대 규칙 위반이 발견되었습니다. CLAUDE.md를 참조하세요."
  exit 1
fi
echo "절대 규칙 검사 통과 ✓"
