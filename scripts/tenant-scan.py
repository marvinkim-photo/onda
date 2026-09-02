#!/usr/bin/env python3
"""규칙 5(테넌트 격리) 정적 스캔 — CLAUDE.md.

테넌트 스코프 테이블을 참조하는 SQL이 tenant_id 필터를 포함하는지 검사한다.
누락 시 위반. 정당한 예외(pre-auth 조회·app_id/PK 스코프 등)는
scripts/tenant-scan-allowlist.txt에 (파일::쿼리해시)와 사유로 등록해야 통과한다.

CI/로컬 공용. 위반 발견 시 exit 1.
"""
import hashlib
import os
import pathlib
import re
import sys

# 테넌트 스코프 테이블 (PG + CH). tenants(루트, id로 스코프)·전역 테이블은 제외.
SCOPED = {
    "members", "sessions", "apps", "api_keys", "credentials", "users", "devices",
    "attribute_registry", "segments", "journeys", "journey_versions",
    "journey_states", "journey_outbox", "user_merges", "audit_logs",
    "member_backup_codes",
    # 신규 테이블(receipt/cursor·저니 노드 실행) — 재검증 R-20
    "event_receipts", "event_customer_cursors", "journey_node_executions",
    # 알림톡(P0) — 커넥터 배선·발신프로필·승인 템플릿·폴링 접수
    "channel_connectors", "alimtalk_senders", "alimtalk_templates", "pending_receipts",
    # ClickHouse
    "events", "message_log", "message_lifecycle", "ingestion_errors", "raw_ingestions",
    "attr_changes", "profiles_mirror",
}

ALLOWLIST_FILE = "scripts/tenant-scan-allowlist.txt"
SQL_KW = re.compile(r"\b(SELECT|INSERT|UPDATE|DELETE)\b", re.I)
TENANT_RE = re.compile(r"\btenant_id\b", re.I)


def load_allowlist():
    allow = {}
    p = pathlib.Path(ALLOWLIST_FILE)
    if p.exists():
        for raw in p.read_text().splitlines():
            line = raw.split("#", 1)[0].strip()
            if line:
                allow[line] = raw
    return allow


def normalize(sql):
    return re.sub(r"\s+", " ", sql).strip().lower()


def qhash(sql):
    return hashlib.sha1(normalize(sql).encode()).hexdigest()[:12]


def scoped_tables(sql):
    hits = set()
    for t in SCOPED:
        if re.search(r"\b(FROM|JOIN|UPDATE|INTO|TABLE)\s+(onda\.)?" + t + r"\b", sql, re.I):
            hits.add(t)
    return hits


def extract_blocks(text):
    for m in re.finditer(r"`([^`]*)`", text, re.S):
        s = m.group(1)
        if SQL_KW.search(s):
            yield text[: m.start()].count("\n") + 1, s


def iter_sources():
    for root, _, files in os.walk("."):
        if any(x in root for x in ("node_modules", "/dist", "/.next", "/.git", "generated")):
            continue
        for fn in files:
            if not (fn.endswith(".ts") or fn.endswith(".go")):
                continue
            if fn.endswith(".d.ts") or fn.endswith("_gen.go") or fn.endswith(".gen.ts") \
               or fn.endswith("_test.go") or fn.endswith(".test.ts") or fn.endswith(".spec.ts"):
                continue
            rel = os.path.relpath(os.path.join(root, fn), ".")
            if rel.startswith("apps/") or rel.startswith("packages/"):
                yield rel


def main():
    allow = load_allowlist()
    used = set()
    violations = []
    for rel in iter_sources():
        text = pathlib.Path(rel).read_text(errors="ignore")
        for line, sql in extract_blocks(text):
            if not scoped_tables(sql):
                continue
            if TENANT_RE.search(sql):
                continue
            key = f"{rel}::{qhash(sql)}"
            if key in allow:
                used.add(key)
                continue
            violations.append((rel, line, key, normalize(sql)[:90]))

    # 오래된(사라진) allowlist 항목 경고 — 조용한 예외 누적 방지
    stale = [k for k in allow if k not in used]

    if violations:
        print("RULE-5 위반 (tenant_id 필터 누락 — allowlist 미등록):")
        for rel, line, key, snip in violations:
            print(f"  {rel}:{line}")
            print(f"    key: {key}")
            print(f"    sql: {snip}")
        print("")
        print(f"정당한 예외면 {ALLOWLIST_FILE}에 위 key와 사유(# ...)를 등록하세요.")
        return 1
    if stale:
        print(f"경고: 사용되지 않는 allowlist 항목 {len(stale)}건 (쿼리 변경/삭제됨) — 정리 권장:")
        for k in stale:
            print(f"  {k}")
    print("RULE-5 (tenant_id 정적 스캔) 통과 ✓")
    return 0


if __name__ == "__main__":
    sys.exit(main())
