// 首页仪表盘（dokploy Home 式）：统计卡（应用 / 状态 / 组件 / 节点）
// + 最近活动（事件流）+ 需要关注（不健康应用）。全部由既有读面客户端
// 派生——Console 不新增端点；查询键与列表/系统页共享（导航即热数据）。

import { useQuery } from "@tanstack/react-query";
import {
  Activity,
  AlertTriangle,
  ArrowRight,
  CheckCircle2,
} from "lucide-react";
import type { LucideIcon } from "lucide-react";
import { Link } from "react-router-dom";

import { listApps, getSystemStatus, listNodes } from "@/api/endpoints";
import { eventTone, useEventStream } from "@/hooks/use-event-stream";
import { useSubjectResolver } from "@/hooks/use-subject-resolver";
import { EmptyState } from "@/components/empty-state";
import { StatCard } from "@/components/stat-card";
import { StateBadge, type StateTone } from "@/components/state-badge";
import { StatusDot } from "@/components/status-dot";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { timeAgo } from "@/lib/utils";

/** 需要关注的派生状态（不可用/部分可用面）。 */
const ATTENTION_STATES = new Set(["degraded", "blocked", "down", "failed"]);

function StatusRow({
  tone,
  count,
  label,
}: {
  tone: StateTone;
  count: number;
  label: string;
}) {
  return (
    <li className="flex items-center gap-2.5 text-sm">
      <StatusDot tone={tone} />
      <span className="w-8 font-semibold tabular-nums">{count}</span>
      <span className="text-muted-foreground">{label}</span>
    </li>
  );
}

function FeedCard({
  title,
  icon: Icon,
  viewAllTo,
  children,
  className,
}: {
  title: string;
  icon: LucideIcon;
  viewAllTo?: string;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <Card className={className}>
      <CardHeader className="flex-row items-center justify-between space-y-0 border-b pb-3">
        <CardTitle className="flex items-center gap-2 text-sm font-semibold">
          <Icon aria-hidden className="h-4 w-4 text-muted-foreground" />
          {title}
        </CardTitle>
        {viewAllTo ? (
          <Link
            to={viewAllTo}
            className="text-xs text-muted-foreground transition-colors hover:text-foreground"
          >
            view all →
          </Link>
        ) : null}
      </CardHeader>
      <CardContent className="p-0">{children}</CardContent>
    </Card>
  );
}

export function HomePage() {
  const appsQuery = useQuery({
    queryKey: ["apps"],
    queryFn: () => listApps(),
    refetchInterval: 5000,
  });
  const statusQuery = useQuery({
    queryKey: ["system", "status"],
    queryFn: getSystemStatus,
    refetchInterval: 10000,
  });
  const nodesQuery = useQuery({
    queryKey: ["system", "nodes"],
    queryFn: listNodes,
    refetchInterval: 20000,
  });
  const { events } = useEventStream();

  // 生成类型口径：gateway EmitUnpopulated=true 下空 repeated 显式输出 []。
  const apps = appsQuery.data?.apps ?? [];
  const activeApps = apps.filter((a) => (a.lifecycle ?? "active") === "active");
  const deletingCount = apps.length - activeApps.length;

  const running = activeApps.filter((a) => a.derived_state === "running").length;
  const degraded = activeApps.filter((a) => a.derived_state === "degraded").length;
  const unavailable = activeApps.filter((a) =>
    ["blocked", "down", "failed"].includes(a.derived_state ?? ""),
  ).length;
  const transitioning = activeApps.filter(
    (a) =>
      !["running", "degraded"].includes(a.derived_state ?? "") &&
      !["blocked", "down", "failed"].includes(a.derived_state ?? ""),
  ).length;

  const components = statusQuery.data?.components ?? [];
  const okComponents = components.filter((c) => c.ok).length;

  const nodes = nodesQuery.data?.nodes ?? [];
  const managers = nodes.filter((n) => n.is_manager).length;
  const stale = nodes.filter((n) => n.stale).length;

  // 事件 subject 可读化（W2-7）：kind:平台ID → kind:业务名（反解出口单一，
  // 见 lib/subject.ts；反解不到降级 kind，不裸显 ULID）。
  const resolveSubject = useSubjectResolver();

  const attention = activeApps
    .filter((a) => ATTENTION_STATES.has(a.derived_state ?? ""))
    .sort((a, b) => (a.updated_at ?? "").localeCompare(b.updated_at ?? ""));
  const recentEvents = [...events].reverse().slice(0, 8);

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div className="space-y-1">
          <h1 className="text-2xl font-semibold tracking-tight">Fleet overview</h1>
          <p className="text-sm text-muted-foreground">
            Health and recent activity across the platform.
          </p>
        </div>
        <Button asChild variant="secondary">
          <Link to="/apps">
            View applications
            <ArrowRight aria-hidden className="h-4 w-4" />
          </Link>
        </Button>
      </div>

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <StatCard
          label="Applications"
          value={appsQuery.isPending ? "—" : apps.length}
          sub={
            deletingCount > 0
              ? `${running} active · ${deletingCount} deleting`
              : `${apps.length} active`
          }
        />
        <StatCard label="Status">
          <ul className="space-y-1.5">
            <StatusRow tone="green" count={running} label="running" />
            <StatusRow tone="amber" count={degraded} label="degraded" />
            <StatusRow tone="red" count={unavailable} label="unavailable" />
            <StatusRow tone="neutral-blue" count={transitioning} label="transitioning" />
          </ul>
        </StatCard>
        <StatCard
          label="Components"
          value={statusQuery.isPending ? "—" : `${okComponents}/${components.length}`}
          sub={
            statusQuery.data
              ? `${statusQuery.data.service ?? "fleetlyd"}${
                  statusQuery.data.version ? ` v${statusQuery.data.version}` : ""
                }`
              : undefined
          }
        />
        <StatCard
          label="Nodes"
          value={nodesQuery.isPending ? "—" : nodes.length}
          sub={`${managers} manager${stale > 0 ? ` · ${stale} stale` : ""}`}
        />
      </div>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <FeedCard
          title="Recent activity"
          icon={Activity}
          viewAllTo="/events"
          className="lg:col-span-2"
        >
          {recentEvents.length === 0 ? (
            <EmptyState
              icon={Activity}
              title={events.length === 0 ? "No recent activity." : "All caught up."}
              hint="Platform events (deploys, lifecycle changes) stream in here live."
            />
          ) : (
            <ul className="divide-y">
              {recentEvents.map((e) => (
                <li key={e.seq}>
                  <Link
                    to="/events"
                    className="flex items-center gap-3 px-5 py-3 transition-colors hover:bg-muted/40"
                  >
                    <StatusDot tone={eventTone(e.name)} />
                    <span className="min-w-0 flex-1">
                      <span className="block truncate text-sm">{e.name}</span>
                      <span className="block truncate text-xs text-muted-foreground">
                        {resolveSubject(e.subject)}
                      </span>
                    </span>
                    <span className="shrink-0 text-xs text-muted-foreground">
                      {timeAgo(e.at)}
                    </span>
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </FeedCard>

        <FeedCard title="Needs attention" icon={AlertTriangle}>
          {appsQuery.isPending ? (
            <p className="p-5 text-sm text-muted-foreground">Loading apps…</p>
          ) : attention.length === 0 ? (
            <EmptyState
              icon={CheckCircle2}
              title="All applications healthy."
              hint="Applications that are degraded or unavailable will surface here."
            />
          ) : (
            <ul className="divide-y">
              {attention.map((a) => (
                <li key={a.id}>
                  <Link
                    to={`/apps/${encodeURIComponent(a.id ?? "")}`}
                    className="flex items-center gap-3 px-5 py-3 transition-colors hover:bg-muted/40"
                  >
                    <StateBadge state={a.derived_state ?? ""} />
                    <span className="min-w-0 flex-1 truncate text-sm">{a.name}</span>
                    <span className="shrink-0 text-xs text-muted-foreground">
                      {timeAgo(a.updated_at)}
                    </span>
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </FeedCard>
      </div>
    </div>
  );
}
