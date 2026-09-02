import Link from "next/link";
import type { ReactNode } from "react";

const iconPaths = {
  wave: <><path d="M2 13c4-8 7 8 12 0s6-3 8-1" /><path d="M3 17c5 5 11-5 18 0" /></>,
  "arrow-left": <><path d="m12 5-7 7 7 7M5 12h14" /></>,
  "arrow-right": <><path d="m12 5 7 7-7 7M5 12h14" /></>,
  plus: <path d="M12 5v14M5 12h14" />,
  minus: <path d="M5 12h14" />,
  message: <><path d="M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9Z" /><path d="M10 21h4" /></>,
  clock: <><circle cx="12" cy="12" r="9" /><path d="M12 7v5l3 2" /></>,
  branch: <><path d="M12 3v6M6 21v-5a4 4 0 0 1 4-4h4a4 4 0 0 1 4 4v5M3 18l3 3 3-3M15 18l3 3 3-3" /><circle cx="12" cy="9" r="2" /></>,
  "event-wait": <><circle cx="10" cy="13" r="8" /><path d="M10 9v4l3 2M16 2l-3 5h4l-2 5 6-7h-4l2-3" /></>,
  split: <><path d="M5 3v4c0 3 3 5 7 5s7 2 7 5v4M19 3v4c0 3-3 5-7 5s-7 2-7 5v4M2 18l3 3 3-3M16 18l3 3 3-3" /></>,
  undo: <><path d="m8 4-5 5 5 5M3 9h10a7 7 0 0 1 0 14" /></>,
  users: <><circle cx="10" cy="7" r="3" /><path d="M4 21v-3a6 6 0 0 1 12 0v3ZM17 4a3 3 0 0 1 0 6M20 20v-3a5 5 0 0 0-3-4" /></>,
  trigger: <path d="m13 2-8 12h6l-1 8 9-13h-7l1-7Z" />,
  flag: <><path d="M5 22V3c5-4 9 4 14 0v10c-5 4-9-4-14 0" /></>,
  check: <path d="m5 12 4 4L19 6" />,
  search: <><circle cx="10.5" cy="10.5" r="6.5" /><path d="m16 16 5 5" /></>,
  close: <path d="m6 6 12 12M6 18 18 6" />,
  up: <path d="m5 12 7-7 7 7M12 5v14" />,
  down: <path d="m5 12 7 7 7-7M12 5v14" />,
  trash: <><path d="M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7M14 10v7" /></>,
  info: <><circle cx="12" cy="12" r="9" /><path d="M12 11v6M12 7v.5" /></>,
  chart: <><path d="M4 3v17h17M8 16v-4M13 16V7M18 16v-6" /></>,
  fit: <path d="M8 3H3v5M16 3h5v5M3 16v5h5M21 16v5h-5" />,
};

export type JourneyIconName = keyof typeof iconPaths;

export function JourneyIcon({ name, size = 20, className }: {
  name: JourneyIconName;
  size?: number;
  className?: string;
}) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor"
      strokeWidth="1.65" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" className={className}>
      {iconPaths[name]}
    </svg>
  );
}

export function JourneyTopbar({ actions, current }: { actions?: ReactNode; current?: ReactNode }) {
  return (
    <header className="j-topbar">
      <nav className="j-breadcrumbs" aria-label="현재 위치">
        <Link href="/" className="j-brand" aria-label="Onda 대시보드">
          <JourneyIcon name="wave" size={30} /><span>Onda</span>
        </Link>
        <span className="j-breadcrumb-divider" />
        {current ? <Link href="/journeys">캠페인 · 저니</Link> : <span>캠페인 · 저니</span>}
        {current && <><span className="j-breadcrumb-slash">/</span><span className="j-breadcrumb-current">{current}</span></>}
      </nav>
      {actions && <div className="j-topbar-actions">{actions}</div>}
    </header>
  );
}

const statusLabels: Record<string, string> = { draft: "초안", active: "활성", paused: "일시정지", archived: "보관" };

export function JourneyStatus({ status }: { status: string }) {
  return <span className={`j-status j-status-${status}`}><span />{statusLabels[status] ?? status}</span>;
}

export function JourneyState({ title, description, action, error = false }: {
  title: string;
  description?: string;
  action?: ReactNode;
  error?: boolean;
}) {
  return (
    <>
      <JourneyTopbar />
      <main className="j-page-state" role={error ? "alert" : "status"}>
        <span className="j-state-icon"><JourneyIcon name={error ? "info" : "wave"} size={30} /></span>
        <h1>{title}</h1>
        {description && <p>{description}</p>}
        {action}
      </main>
    </>
  );
}
