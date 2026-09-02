"use client";

import { useQuery, useMutation } from "@tanstack/react-query";
import { useRouter } from "next/navigation";
import { useEffect } from "react";
import { api } from "@/lib/api";
import { useAppId } from "./use-app-id";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

export default function DashboardPage() {
  const router = useRouter();
  const me = useQuery({ queryKey: ["me"], queryFn: () => api.auth.me(), retry: false });
  const appId = useAppId();
  const dashboard = useQuery({
    queryKey: ["dashboard", appId],
    queryFn: () => api.analytics.dashboard(appId!),
    enabled: !!appId,
  });
  const usage = useQuery({
    queryKey: ["usage", appId],
    queryFn: () => api.analytics.usage(appId!),
    enabled: !!appId,
  });
  const uninstalls = useQuery({
    queryKey: ["uninstalls", appId],
    queryFn: () => api.analytics.uninstalls(appId!, 30),
    enabled: !!appId,
  });
  const logout = useMutation({
    mutationFn: () => api.auth.logout(),
    onSuccess: () => router.push("/login"),
  });
  const sweep = useMutation({
    mutationFn: () => api.analytics.uninstallSweep(appId!),
    onSuccess: () => uninstalls.refetch(),
  });

  useEffect(() => {
    if (me.isError) router.push("/login");
  }, [me.isError, router]);

  if (me.isPending || me.isError) {
    return (
      <main className="flex min-h-screen items-center justify-center">
        <p className="text-sm text-muted-foreground">불러오는 중…</p>
      </main>
    );
  }

  return (
    <main className="mx-auto max-w-4xl p-8">
      <header className="mb-8 flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold">Onda 콘솔</h1>
          <p className="text-sm text-muted-foreground">
            {me.data.name} ({me.data.email}) · {me.data.role}
          </p>
        </div>
        <Button variant="outline" onClick={() => logout.mutate()}>
          로그아웃
        </Button>
      </header>

      {/* 오늘 지표 위젯 (PRD-07) */}
      <div className="mb-6 grid grid-cols-2 gap-4 md:grid-cols-4">
        <Stat label="오늘 발송" value={dashboard.data?.today.sent ?? 0} />
        <Stat label="오늘 실패" value={dashboard.data?.today.failed ?? 0} accent={(dashboard.data?.today.failed ?? 0) > 0} />
        <Stat label="오늘 생략" value={dashboard.data?.today.skipped ?? 0} />
        <Stat label="활성 저니" value={dashboard.data?.active_journeys ?? 0} />
      </div>
      <div className="mb-6 grid grid-cols-2 gap-4 md:grid-cols-3">
        <Stat label="DAU (오늘)" value={usage.data?.dau_today ?? 0} />
        <Stat label="MAU (30일)" value={usage.data?.mau_30d ?? 0} />
        <Stat
          label={`앱 삭제 (30일) ${((uninstalls.data?.uninstall_rate ?? 0) * 100).toFixed(2)}%`}
          value={uninstalls.data?.uninstalls ?? 0}
          accent={(uninstalls.data?.uninstalls ?? 0) > 0}
        />
        <Stat
          label="발송량 (30일)"
          value={usage.data?.sends_30d.reduce((a, b) => a + b.sent, 0) ?? 0}
        />
      </div>

      <Card>
        <CardHeader>
          <CardTitle>시작하기</CardTitle>
          <CardDescription>
            SDK Key → 크리덴셜 등록 → 첫 이벤트 감지 → 테스트 발송 (4단계)
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-wrap gap-2 [&>button]:shrink-0 [&>button]:whitespace-nowrap">
          <Button onClick={() => router.push("/onboarding")}>온보딩 위저드 열기</Button>
          <Button variant="outline" onClick={() => router.push("/segments")}>
            세그먼트
          </Button>
          <Button variant="outline" onClick={() => router.push("/journeys")}>
            캠페인 · 저니
          </Button>
          <Button variant="outline" onClick={() => router.push("/logs")}>
            메시지 로그
          </Button>
          <Button variant="outline" onClick={() => router.push("/users")}>
            유저 검색
          </Button>
          <Button variant="outline" onClick={() => router.push("/data")}>
            데이터
          </Button>
          <Button variant="outline" onClick={() => router.push("/settings")}>
            앱 설정
          </Button>
          {me.data.permissions?.includes("journeys:read") && (
            <>
              <Button variant="outline" onClick={() => router.push("/email-templates")}>
                이메일 템플릿
              </Button>
              <Button variant="outline" onClick={() => router.push("/channels/alimtalk")}>
                알림톡 설정
              </Button>
            </>
          )}
          {me.data.permissions?.includes("journeys:activate") && (
            <Button variant="outline" disabled={sweep.isPending} onClick={() => sweep.mutate()}>
              {sweep.isPending ? "삭제 감지 중…" : "앱 삭제 감지 스윕"}
            </Button>
          )}
          {me.data.permissions?.includes("team:read") && (
            <>
              <Button variant="outline" onClick={() => router.push("/team")}>
                팀 관리
              </Button>
              <Button variant="outline" onClick={() => router.push("/audit")}>
                감사 로그
              </Button>
            </>
          )}
        </CardContent>
      </Card>
    </main>
  );
}

function Stat({ label, value, accent }: { label: string; value: number; accent?: boolean }) {
  return (
    <Card>
      <CardContent className="p-4">
        <p className="text-xs text-muted-foreground">{label}</p>
        <p className={`mt-1 text-2xl font-bold ${accent ? "text-destructive" : ""}`}>
          {value.toLocaleString()}
        </p>
      </CardContent>
    </Card>
  );
}
