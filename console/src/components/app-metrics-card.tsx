// 应用资源卡（E6 W5-S3，设计 §4.2「Console 图表」+ §4.1 挂账收敛的
// 多副本水位显示）：metrics.mode=on 且三件收敛 → 容器 CPU/内存曲线
//（按 swarm service 维度聚合，service 名 fleetly-<app>-<svc> 前缀匹配）
// + 每副本（task 维度）水位表；mode=on 组件未就绪 → 诚实「采集中」态
//（不画空线——设计 §4.2 原文）；mode=unset → opt-in 引导 + 开关入口
//（写面 = SetMetricsMode 走 requirePlatformWriteFace——平台全局设置，
// 仅平台管理员可写；非平台管理员原位说明，与平台设置页 MetricsSettingsCard
// 同一门）。
//
// PromQL 最小集写死在 Console 侧（锚点只增：app-metrics-card /
// metrics-mode-toggle / replicas-watermark；图表锚点在 metrics-chart）。
// 注：cAdvisor 对 swarm 任务容器暴露 container_label_com_docker_swarm_*
// 维度（compose 的 container_label_com_docker_compose_* 不适用——真实
// 形态由 e2e/metrics.sh 断言钉住）。

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Activity } from "lucide-react";
import { useMemo } from "react";

import {
  getMetricsStatus,
  searchMetrics,
  setMetricsMode,
} from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import type { MetricsSeries } from "@/api/types";
import { EnvelopeAlertFrom } from "@/components/envelope-alert";
import { MetricsChart, type ChartSeries } from "@/components/metrics-chart";
import { useIsPlatformAdmin } from "@/lib/context";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";

/** swarm service label 值 → 展示名（剥离 fleetly-<app>- 前缀）。 */
function displayService(label: string, app: string): string {
  const prefix = `fleetly-${app}-`;
  return label.startsWith(prefix) ? label.slice(prefix.length) : label;
}

/** 序列 → 图表序列（label 取指定维度值并剥离 app 前缀；点集透传）。 */
function toChartSeries(
  series: MetricsSeries[],
  labelKey: string,
  fallback: string,
  app: string,
): ChartSeries[] {
  return series.map((s) => ({
    label: s.metric?.[labelKey]
      ? displayService(s.metric[labelKey] as string, app)
      : fallback,
    points: (s.points ?? []).map((p) => ({ t: Number(p.t ?? 0), v: Number(p.v ?? 0) })),
  }));
}

/** CPU 值格式化（rate 峰值量级 0.01~数核——核数两位小数）。 */
const formatCores = (v: number) => `${v.toFixed(3)} CPU`;

/** 内存值格式化（bytes → MiB，量级直读）。 */
const formatMiB = (v: number) => `${(v / (1024 * 1024)).toFixed(1)} MiB`;

const SERVICE_LABEL = "container_label_com_docker_swarm_service_name";
const TASK_LABEL = "container_label_com_docker_swarm_task_name";

interface AppMetricsCardProps {
  app: string;
}

export function AppMetricsCard({ app }: AppMetricsCardProps) {
  const queryClient = useQueryClient();
  // metrics 模式切换是平台全局设置写面（服务端 SetMetricsMode 走
  // requirePlatformWriteFace——用户 principal 须平台管理员）：平台管理员
  // 见开关，非平台管理员原位说明。此前按 P0-3「资源面只读」归类是误判
  //（开关曾对平台管理员隐藏、对无权写者可见，2026-09-29 反转对齐服务端
  // ——与平台设置页 metrics-mode-toggle 同一门）。
  const isPlatformAdmin = useIsPlatformAdmin();
  const status = useQuery({
    queryKey: ["metrics", "status"],
    queryFn: getMetricsStatus,
    refetchInterval: 10000,
  });

  const enable = useMutation({
    mutationFn: () => setMetricsMode("on"),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["metrics"] }),
  });

  const mode = status.data?.mode ?? "";
  const components = status.data?.components ?? [];
  const ready =
    mode === "on" &&
    components.length > 0 &&
    components.every((c) => c.exists);

  // 服务维度 regex（安全转义 app 名——app 名词表是 ^[a-z0-9-]+$，仍按
  // 字面构造，不依赖词表假设）。
  const serviceFilter = useMemo(() => {
    const escaped = app.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    return `^fleetly-${escaped}-.*`;
  }, [app]);

  const cpuQuery = useQuery({
    queryKey: ["metrics", "app-cpu", app],
    queryFn: () =>
      searchMetrics(
        `sum by (${SERVICE_LABEL}) (rate(container_cpu_usage_seconds_total{${SERVICE_LABEL}=~"${serviceFilter}",image!=""}[2m]))`,
        { step_seconds: 30 },
      ),
    enabled: ready,
    refetchInterval: 15000,
  });
  const memQuery = useQuery({
    queryKey: ["metrics", "app-mem", app],
    queryFn: () =>
      searchMetrics(
        `sum by (${SERVICE_LABEL}) (container_memory_usage_bytes{${SERVICE_LABEL}=~"${serviceFilter}",image!=""})`,
        { step_seconds: 30 },
      ),
    enabled: ready,
    refetchInterval: 15000,
  });
  // 副本水位（task 维度——每副本一行；CPU 为 2m 窗口平均核数）。
  const replicaCpuQuery = useQuery({
    queryKey: ["metrics", "replica-cpu", app],
    queryFn: () =>
      searchMetrics(
        `sum by (${TASK_LABEL}) (rate(container_cpu_usage_seconds_total{${SERVICE_LABEL}=~"${serviceFilter}",image!=""}[2m]))`,
        { step_seconds: 60 },
      ),
    enabled: ready,
    refetchInterval: 15000,
  });
  const replicaMemQuery = useQuery({
    queryKey: ["metrics", "replica-mem", app],
    queryFn: () =>
      searchMetrics(
        `sum by (${TASK_LABEL}) (container_memory_usage_bytes{${SERVICE_LABEL}=~"${serviceFilter}",image!=""})`,
        { step_seconds: 60 },
      ),
    enabled: ready,
    refetchInterval: 15000,
  });

  if (status.isPending) {
    return (
      <Card data-testid="app-metrics-card">
        <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
          <Activity aria-hidden className="h-4 w-4 text-muted-foreground" />
          <CardTitle className="text-sm font-semibold">Resources</CardTitle>
        </CardHeader>
        <CardContent className="pt-4">
          <p className="text-sm text-muted-foreground">Loading…</p>
        </CardContent>
      </Card>
    );
  }

  if (status.isError) {
    return (
      <Card data-testid="app-metrics-card">
        <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
          <Activity aria-hidden className="h-4 w-4 text-muted-foreground" />
          <CardTitle className="text-sm font-semibold">Resources</CardTitle>
        </CardHeader>
        <CardContent className="pt-4">
          <EnvelopeAlertFrom envelope={errorEnvelopeFrom(status.error)} />
        </CardContent>
      </Card>
    );
  }

  // opt-in 缺省态：引导文案 + 开关入口（设计 §4.1 D-W5-2——默认关）。
  if (mode !== "on") {
    return (
      <Card data-testid="app-metrics-card">
        <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
          <Activity aria-hidden className="h-4 w-4 text-muted-foreground" />
          <CardTitle className="text-sm font-semibold">Resources</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3 pt-4">
          <p className="text-sm text-muted-foreground">
            Container metrics are opt-in. Enabling deploys a managed VictoriaMetrics /
            cAdvisor / node-exporter stack (VictoriaMetrics query face is loopback-only
            on the manager; collectors are reachable over the node VPC/LAN face;
            ≈110–165 MB memory) and adds resource charts plus per-replica watermarks here.
          </p>
          {enable.isError ? (
            <EnvelopeAlertFrom envelope={errorEnvelopeFrom(enable.error)} />
          ) : null}
          {isPlatformAdmin ? (
            <Button
              size="sm"
              data-testid="metrics-mode-toggle"
              disabled={enable.isPending}
              onClick={() => enable.mutate()}
            >
              {enable.isPending ? "Enabling…" : "Enable metrics"}
            </Button>
          ) : (
            // 写面说明（与平台设置页 metrics-mode-readonly-note 同语义）。
            <p
              className="text-xs text-muted-foreground"
              data-testid="metrics-mode-readonly-note"
            >
              Platform administrator required — enabling deploys a platform-wide
              collection stack. Ask your platform administrator, or use the CLI
              with a machine token (<code>fleetly metrics mode</code>).
            </p>
          )}
        </CardContent>
      </Card>
    );
  }

  // 启用态但组件未收敛：诚实「采集中」——不画空线。
  if (!ready) {
    return (
      <Card data-testid="app-metrics-card">
        <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
          <Activity aria-hidden className="h-4 w-4 text-muted-foreground" />
          <CardTitle className="text-sm font-semibold">Resources</CardTitle>
        </CardHeader>
        <CardContent className="pt-4" data-testid="metrics-pending">
          <p className="text-sm text-muted-foreground">
            Metrics is enabled and the managed stack is converging — charts appear once
            the collector is reporting. This is honest, not an empty chart.
          </p>
        </CardContent>
      </Card>
    );
  }

  // 副本水位表数据（CPU/内存按 task 名对齐；任一查询失败如实降级为
  // 内存列缺失——不伪造 0）。
  const replicaRows = new Map<
    string,
    { cpu?: number; mem?: number }
  >();
  for (const s of replicaCpuQuery.data?.series ?? []) {
    const name = s.metric?.[TASK_LABEL];
    if (name && s.points?.length) {
      const row = replicaRows.get(name) ?? {};
      row.cpu = Number(s.points[s.points.length - 1]?.v ?? 0);
      replicaRows.set(name, row);
    }
  }
  for (const s of replicaMemQuery.data?.series ?? []) {
    const name = s.metric?.[TASK_LABEL];
    if (name && s.points?.length) {
      const row = replicaRows.get(name) ?? {};
      row.mem = Number(s.points[s.points.length - 1]?.v ?? 0);
      replicaRows.set(name, row);
    }
  }

  return (
    <Card className="md:col-span-2" data-testid="app-metrics-card">
      <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
        <Activity aria-hidden className="h-4 w-4 text-muted-foreground" />
        <CardTitle className="text-sm font-semibold">Resources</CardTitle>
        <CardDescription className="ml-auto text-xs">
          {status.data?.nodes_reporting}/{status.data?.nodes_total} nodes reporting
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4 pt-4">
        {/* 归属标签缺席的诚实降级（docker 29 containerd-snapshotter 形态下
            cAdvisor 元数据不带 swarm 标签——上游限制，见 spec.go 注记）：
            服务维度序列为空时显式说明，不冒充分组图表。 */}
        {!cpuQuery.isError &&
        !memQuery.isError &&
        (cpuQuery.data?.series?.length ?? 0) === 0 &&
        (memQuery.data?.series?.length ?? 0) === 0 &&
        !cpuQuery.isPending &&
        !memQuery.isPending ? (
          <p className="text-xs text-muted-foreground" data-testid="metrics-grouping-unavailable">
            Container samples are being collected, but this Docker host runs the
            containerd-snapshotter mode where cAdvisor cannot read swarm labels —
            per-service grouping is unavailable here. Raw container series remain
            queryable via the CLI (fleetly metrics query).
          </p>
        ) : null}
        {cpuQuery.isError ? (
          <EnvelopeAlertFrom envelope={errorEnvelopeFrom(cpuQuery.error)} />
        ) : (
          <div>
            <div className="mb-1 text-xs font-medium text-muted-foreground">
              CPU per service (2m rate)
            </div>
            <MetricsChart
              series={toChartSeries(cpuQuery.data?.series ?? [], SERVICE_LABEL, "cpu", app)}
              formatValue={formatCores}
              ariaLabel="CPU usage per service"
            />
          </div>
        )}
        {memQuery.isError ? (
          <EnvelopeAlertFrom envelope={errorEnvelopeFrom(memQuery.error)} />
        ) : (
          <div>
            <div className="mb-1 text-xs font-medium text-muted-foreground">
              Memory per service
            </div>
            <MetricsChart
              series={toChartSeries(memQuery.data?.series ?? [], SERVICE_LABEL, "mem", app)}
              formatValue={formatMiB}
              ariaLabel="Memory usage per service"
            />
          </div>
        )}

        <div data-testid="replicas-watermark">
          <div className="mb-1 text-xs font-medium text-muted-foreground">
            Per-replica watermark
          </div>
          {replicaRows.size === 0 ? (
            <p className="text-xs text-muted-foreground">
              No replica samples yet (fresh deployments take ~1 minute to report).
            </p>
          ) : (
            <div className="rounded-md border">
              <table className="w-full text-xs">
                <thead>
                  <tr className="border-b text-left text-muted-foreground">
                    <th className="px-3 py-2 font-medium">Replica</th>
                    <th className="px-3 py-2 font-medium">CPU</th>
                    <th className="px-3 py-2 font-medium">Memory</th>
                  </tr>
                </thead>
                <tbody>
                  {[...replicaRows.entries()].map(([name, row]) => (
                    <tr key={name} className="border-b last:border-b-0">
                      <td className="px-3 py-2 font-mono">{name}</td>
                      <td className="px-3 py-2">
                        {row.cpu !== undefined ? formatCores(row.cpu) : "—"}
                      </td>
                      <td className="px-3 py-2">
                        {row.mem !== undefined ? formatMiB(row.mem) : "—"}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </CardContent>
    </Card>
  );
}

// displayService 保留导出面之外的使用可能（AppOverviewPage 后续直接引用
// 服务展示名）；当前组件内部经由 toChartSeries 的原始 label 展示，函数
// 由测试直接覆盖。
export { displayService };
