// 构建台账页（P1-6「Builds 全无 UI」）：BuildsService 读面的 Console 呈现
// ——ListBuilds（GET /v1/apps/{app}/builds，路径参数即 app 过滤，created_at
// 倒序）+ 行展开取构建日志。进行中构建行随列表轮询刷新（复用部署页的
// refetchInterval 模式）。
//
// 契约事实（builds.proto / logs.proto；scope 登记见 internal/api/scope.go）：
// - BuildView 无触发来源字段（webhook/manual 不可辨）、无 commit
//   sha、亦无部署关联键（DeploymentView 同样无 build 反向引用）——对应列
//   与部署页互链均不硬造。
// - 构建日志不在构建响应内（log_path 是宿主归档路径）；按 logs.proto 的
//   History 契约经 listHistoryLogs(source=build) 取：行按 app+service 归
//   属、行时间取构建开始时刻（internal/logs buildLogEntries），检索窗取
//   该构建 started_at..finished_at。
// - TriggerBuild REST 面存在（POST /v1/builds）但 = admin scope + compose
//   字节载荷（app 可自动建行）——CLI `fleetly build <compose>` 的构建入
//   口，不是「对既有应用的手动重建」，Console 不设 Trigger 按钮。本页纯
//   读面（GetBuild/ListBuilds = read），所有角色可见、无角色门。

import { useQuery } from "@tanstack/react-query";
import { Hammer, Loader2 } from "lucide-react";
import { useState } from "react";
import { useParams } from "react-router-dom";

import { listBuilds, listHistoryLogs } from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import type { BuildView } from "@/api/types";
import { EnvelopeAlert } from "@/components/envelope-alert";
import { StateBadge } from "@/components/state-badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatTime, timeAgo } from "@/lib/utils";

/** 构建终态集（BuildView.status 词表 queued/building/succeeded/failed；
 *  之外的状态随轮询跟踪）。 */
const TERMINAL = new Set(["succeeded", "failed"]);

/**
 * formatDuration 渲染构建时长（started/finished 成对才可计算——未终态行
 * finished_at 缺省，不猜进度渲染「—」）。
 */
function formatDuration(
  startedIso: string | undefined,
  finishedIso: string | undefined,
): string {
  if (!startedIso || !finishedIso) return "—";
  const seconds = Math.max(
    0,
    Math.round((Date.parse(finishedIso) - Date.parse(startedIso)) / 1000),
  );
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${seconds % 60}s`;
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
}

/**
 * 行展开的构建日志：经 ListHistoryLogs(source=build) 按构建时间窗取
 *（契约：日志行按 app+service 归属、行时间 = 构建开始时刻——不伪造逐行
 * 时间，说明文案如实披露）。queued 行 started_at 缺省 = 日志尚未落产物，
 * 不发起请求；进行中构建的日志随轮询刷新（与列表轮询同周期）。
 */
function BuildLog({ build }: { build: BuildView }) {
  const query = useQuery({
    queryKey: ["build-log", build.id],
    queryFn: () =>
      listHistoryLogs(build.app ?? "", {
        service: build.service ?? "",
        source: "build",
        since: build.started_at,
        until: build.finished_at ?? undefined,
        limit: 1000,
      }),
    enabled: Boolean(build.started_at),
    refetchInterval: TERMINAL.has(build.status ?? "") ? false : 2000,
  });

  if (!build.started_at) {
    return (
      <p className="text-xs text-muted-foreground" data-testid="build-log-empty">
        No build log yet — the build has not started.
      </p>
    );
  }

  if (query.isError) {
    const envelope = errorEnvelopeFrom(query.error);
    return (
      <EnvelopeAlert
        code={envelope.code}
        message={envelope.message}
        suggestion={envelope.suggestion}
        docs={envelope.docs}
      />
    );
  }

  const entries = query.data?.entries ?? [];

  return (
    <div className="space-y-1.5">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="text-xs font-medium">Build log</span>
        <span className="text-xs text-muted-foreground">
          Archived on the daemon host; line timestamps reflect the build start
          time.
        </span>
      </div>
      {query.isPending ? (
        <div className="flex h-20 items-center justify-center rounded-md border">
          <Loader2 aria-hidden className="h-4 w-4 animate-spin text-muted-foreground" />
        </div>
      ) : entries.length === 0 ? (
        <p className="text-xs text-muted-foreground" data-testid="build-log-empty">
          No log lines in the archive for this build window.
        </p>
      ) : (
        <div
          data-testid="build-log"
          className="max-h-72 overflow-auto rounded-lg border border-zinc-800 bg-zinc-950 p-3 font-mono text-xs leading-5 text-zinc-100"
        >
          {entries.map((entry, i) => (
            <div key={i} className="whitespace-pre-wrap break-all">
              {entry.stderr ? (
                <span className="text-red-400">{entry.line}</span>
              ) : (
                entry.line
              )}
            </div>
          ))}
        </div>
      )}
      {build.log_path ? (
        <p className="break-all font-mono text-xs text-muted-foreground">
          Archived log: {build.log_path}
        </p>
      ) : null}
    </div>
  );
}

function BuildRow({
  build,
  expanded,
  onToggle,
}: {
  build: BuildView;
  expanded: boolean;
  onToggle: () => void;
}) {
  return (
    <>
      <TableRow data-testid="build-row" data-status={build.status}>
        <TableCell className="whitespace-nowrap">
          <StateBadge state={build.status ?? ""} />
        </TableCell>
        <TableCell className="font-mono text-xs">
          <span title={build.id}>{(build.id ?? "").slice(0, 12)}</span>
        </TableCell>
        <TableCell className="text-xs">{build.service || "—"}</TableCell>
        <TableCell className="text-xs text-muted-foreground">
          {build.driver || "—"}
        </TableCell>
        <TableCell className="whitespace-nowrap font-mono text-xs">
          {formatDuration(build.started_at, build.finished_at)}
        </TableCell>
        <TableCell
          className="whitespace-nowrap text-xs text-muted-foreground"
          title={build.started_at ? formatTime(build.started_at) : undefined}
        >
          {build.started_at ? timeAgo(build.started_at) : "—"}
        </TableCell>
        <TableCell className="max-w-[280px]">
          {build.status === "failed" && build.error_code ? (
            <span className="font-mono text-xs text-red-600 dark:text-red-400">
              {build.error_code}
            </span>
          ) : build.image_ref ? (
            <span
              className="block truncate font-mono text-xs text-muted-foreground"
              title={build.image_ref}
            >
              {build.image_ref}
            </span>
          ) : (
            <span className="text-xs text-muted-foreground">—</span>
          )}
        </TableCell>
        <TableCell>
          <Button variant="ghost" size="sm" aria-expanded={expanded} onClick={onToggle}>
            {expanded ? "Hide details" : "Details"}
          </Button>
        </TableCell>
      </TableRow>
      {expanded ? (
        // 展开行不带 build-row 锚点（行数断言只数构建行本身，与部署页同款纪律）。
        <TableRow className="border-b bg-muted/30 hover:bg-muted/30">
          <TableCell colSpan={8} className="p-4">
            <div className="space-y-3" data-testid="build-detail">
              <div className="flex flex-wrap gap-x-6 gap-y-1 text-xs text-muted-foreground">
                <span>
                  Started:{" "}
                  <span className="text-foreground">
                    {build.started_at ? formatTime(build.started_at) : "—"}
                  </span>
                </span>
                <span>
                  Finished:{" "}
                  <span className="text-foreground">
                    {build.finished_at ? formatTime(build.finished_at) : "—"}
                  </span>
                </span>
                {build.image_digest ? (
                  <span className="break-all font-mono">Digest: {build.image_digest}</span>
                ) : null}
                {build.error_code ? (
                  <span className="font-mono text-red-600 dark:text-red-400">
                    Failure: {build.error_code}
                  </span>
                ) : null}
              </div>
              <BuildLog build={build} />
            </div>
          </TableCell>
        </TableRow>
      ) : null}
    </>
  );
}

export function AppBuildsPage() {
  const { name = "" } = useParams();
  // 单一展开位：一次只看一条构建的详情（再点收起，与部署页同款交互）。
  const [expandedId, setExpandedId] = useState("");

  const buildsQuery = useQuery({
    queryKey: ["builds", name],
    queryFn: () => listBuilds(name, 20),
    refetchInterval: (q) => {
      // 存在非终态构建行时保持轮询（跟踪进行中的构建，复用部署页模式）。
      const rows = q.state.data?.builds ?? [];
      return rows.some((b) => !TERMINAL.has(b.status ?? "")) ? 2000 : 10000;
    },
  });

  if (buildsQuery.isError) {
    const envelope = errorEnvelopeFrom(buildsQuery.error);
    return (
      <EnvelopeAlert
        code={envelope.code}
        message={envelope.message}
        suggestion={envelope.suggestion}
        docs={envelope.docs}
      />
    );
  }

  const builds = buildsQuery.data?.builds ?? [];

  return (
    <Card>
      <CardHeader className="border-b pb-3">
        <CardTitle className="flex items-center gap-2 text-sm font-semibold">
          <Hammer aria-hidden className="h-4 w-4 text-muted-foreground" />
          Builds{" "}
          <span className="font-normal text-muted-foreground">({builds.length})</span>
        </CardTitle>
        <CardDescription>
          Image build ledger for this app (last 20). Builds are queued by
          deploys; to trigger a build outside a deploy, use{" "}
          <code className="font-mono text-xs">fleetly build</code> from the CLI.
        </CardDescription>
      </CardHeader>
      <CardContent className="p-0">
        {builds.length === 0 ? (
          <p className="p-5 text-sm text-muted-foreground">No builds yet.</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>State</TableHead>
                <TableHead>Build</TableHead>
                <TableHead>Service</TableHead>
                <TableHead>Driver</TableHead>
                <TableHead>Duration</TableHead>
                <TableHead>Started</TableHead>
                <TableHead>Result</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {builds.map((b) => (
                <BuildRow
                  key={b.id}
                  build={b}
                  expanded={expandedId !== "" && expandedId === b.id}
                  onToggle={() =>
                    setExpandedId((cur) => (cur === b.id ? "" : b.id ?? ""))
                  }
                />
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}
