// Containers 页：运行实况真相页（2026-09-29 IA 重设计，设计
// docs/design/2026-09-29-console-ia-redesign.md §4.3）——回答「现在到底跑
// 的是什么」：服务并集小结（声明 vs 实况水位成对出现，不单报声明值冒充
// 实况）+ 全量任务表（swarm task 语义：滚动后旧任务仍在列表，
// desired_state=shutdown/remove 标注下线；running 且 desired=running 的
// 任务即「实际运行的容器」）+ Drift 卡（实况 vs 期望的对账域同页）。
// 数据源 RuntimeService.ShowAppRuntime（read scope），5s 轮询与详情头
// getApp 同拍。cron/init 一次性 job 不在本面（任务台账在 Overview 的
// cron 区块）。

import { useQuery } from "@tanstack/react-query";
import { Container, Boxes, TriangleAlert } from "lucide-react";
import { useParams } from "react-router-dom";

import { getAppRuntime } from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import type { ServiceRuntimeView, ServiceTaskView } from "@/api/types";
import { AppDriftCard } from "@/components/app-drift-card";
import { EnvelopeAlert } from "@/components/envelope-alert";
import { EmptyState } from "@/components/empty-state";
import { SectionCard } from "@/components/section-card";
import { StateBadge, TONE_CLASSES } from "@/components/state-badge";
import { timeAgo } from "@/lib/utils";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { cn } from "@/lib/utils";

function TaskStateBadge({ task }: { task: ServiceTaskView }) {
  const state = task.state ?? "";
  return (
    <div className="flex items-center gap-1.5">
      <StateBadge state={state} />
      {task.desired_state && task.desired_state !== "running" ? (
        <span className="text-xs text-muted-foreground">
          desired: {task.desired_state}
        </span>
      ) : null}
    </div>
  );
}

// 服务小结行：名字 + 水位（running x / declared y 成对）+ absent 警示 +
// 更新状态（底座 UpdateStatus 逐字——paused/updating 是诊断入口）。
function ServiceRuntimeCard({ svc }: { svc: ServiceRuntimeView }) {
  const missing = svc.missing === true;
  // uint64 经 proto3 JSON 是字符串——水位比较/渲染前归一。
  const running = Number(svc.actual_replicas ?? 0);
  const declared = Number(svc.declared_replicas ?? 0);
  const watermark = svc.mode === "global" ? "global" : `${running} / ${declared || "—"}`;
  return (
    <div
      data-testid="runtime-service-card"
      className="min-w-0 rounded-lg border p-3"
    >
      <div className="flex items-center gap-2">
        <span
          aria-hidden
          className={cn(
            "inline-block h-2 w-2 shrink-0 rounded-full",
            TONE_CLASSES[missing ? "red" : running > 0 ? "green" : "gray"],
          )}
        />
        <span className="truncate font-mono text-xs font-medium" title={svc.name}>
          {svc.name}
        </span>
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
        <span className="font-medium tabular-nums">
          <span className="text-muted-foreground">running</span> {watermark}
        </span>
        {svc.update_state && svc.update_state !== "completed" ? (
          <span
            className="inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-muted-foreground"
            title={svc.update_message || undefined}
          >
            update: {svc.update_state}
          </span>
        ) : null}
      </div>
      <p className="mt-1 truncate font-mono text-[11px] text-muted-foreground" title={svc.image}>
        {svc.image || "—"}
      </p>
    </div>
  );
}

export function AppContainersPage() {
  const { name = "" } = useParams();
  const app = name;
  const runtimeQuery = useQuery({
    queryKey: ["runtime", app],
    queryFn: () => getAppRuntime(app),
    refetchInterval: 5000,
  });

  if (runtimeQuery.isError) {
    const envelope = errorEnvelopeFrom(runtimeQuery.error);
    return (
      <EnvelopeAlert
        code={envelope.code}
        message={envelope.message}
        suggestion={envelope.suggestion}
        docs={envelope.docs}
      />
    );
  }
  if (!runtimeQuery.isSuccess) {
    return (
      <div className="flex h-40 items-center justify-center text-sm text-muted-foreground">
        Loading runtime…
      </div>
    );
  }

  // gateway EmitUnpopulated=true：空服务集显式输出 []——isSuccess 后即
  // 「已加载」（空集=合法空，与加载中由 isSuccess 区分；`?? []` 为测试桩
  // 历史形态兜底）。
  const services = runtimeQuery.data.services ?? [];
  const tasks: { service: ServiceRuntimeView; task: ServiceTaskView }[] = [];
  for (const svc of services) {
    for (const task of svc.tasks ?? []) {
      tasks.push({ service: svc, task });
    }
  }

  return (
    <div className="space-y-4">
      <SectionCard
        icon={Boxes}
        title="Services"
        description="Declared vs actually running watermark per managed service (expected ∪ actual union)."
        contentClassName="pt-4"
      >
        {services.length === 0 ? (
          <EmptyState
            icon={Container}
            title="No managed services"
            hint="Deploy a compose file to start workloads — see the Deployments tab. Scheduled (cron) services run as one-shot jobs and appear on the Overview cron ledger instead."
          />
        ) : (
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3" data-testid="runtime-services">
            {services.map((svc) => (
              <ServiceRuntimeCard key={svc.name} svc={svc} />
            ))}
          </div>
        )}
      </SectionCard>

      <SectionCard
        icon={Container}
        title="Tasks"
        description="Every swarm task including rollout history — running with desired=running is a live container instance."
        contentClassName="pt-4"
      >
        {tasks.length === 0 ? (
          <EmptyState
            icon={Container}
            title={services.length === 0 ? "No tasks" : "No tasks recorded"}
            hint={
              services.some((s) => s.missing)
                ? "Declared services are absent from the swarm — check the Drift report below or redeploy."
                : undefined
            }
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Service</TableHead>
                <TableHead>Task</TableHead>
                <TableHead>State</TableHead>
                <TableHead>Desired</TableHead>
                <TableHead>Image</TableHead>
                <TableHead>Updated</TableHead>
                <TableHead>Error</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {tasks.map(({ service, task }) => (
                <TableRow key={`${service.name}/${task.id}`} data-testid="runtime-task-row">
                  <TableCell className="max-w-[220px] truncate font-mono text-xs" title={service.name}>
                    {service.name}
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {task.slot ? `#${task.slot} ` : ""}
                    <span title={task.id}>{(task.id ?? "").slice(0, 12)}</span>
                  </TableCell>
                  <TableCell>
                    <TaskStateBadge task={task} />
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {task.desired_state || "—"}
                  </TableCell>
                  <TableCell className="max-w-[220px] truncate font-mono text-xs" title={task.image}>
                    {task.image || "—"}
                  </TableCell>
                  <TableCell
                    className="whitespace-nowrap text-xs text-muted-foreground"
                    title={task.timestamp ?? undefined}
                  >
                    {task.timestamp ? timeAgo(task.timestamp) : "—"}
                  </TableCell>
                  <TableCell className="max-w-[240px]">
                    {task.error ? (
                      <span className="flex items-start gap-1 text-xs text-red-600 dark:text-red-400">
                        <TriangleAlert aria-hidden className="mt-0.5 h-3 w-3 shrink-0" />
                        <span className="break-words">{task.error}</span>
                      </span>
                    ) : (
                      <span className="text-xs text-muted-foreground">—</span>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </SectionCard>

      {/* Drift 卡（实况 vs 期望）从 Overview 迁入：对账语义与任务实况同域
          ——Containers 页成为「跑了什么、对不对」的唯一入口。 */}
      <AppDriftCard app={app} />
    </div>
  );
}
