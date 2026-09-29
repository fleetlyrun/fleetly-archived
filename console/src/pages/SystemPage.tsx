// 系统页（只读为主）：统计卡行（控制面/组件/节点/备份健康——BackupHealth
// 字段旧版未呈现，此处一等展示）+ 分段页签四区（Components / Nodes /
// Ingress / Storage——Storage 承载备份台账（含远端上传结论）与 S3 设置卡，
// E3-8）。

import { useQuery } from "@tanstack/react-query";
import {
  Boxes,
  DatabaseBackup,
  GaugeCircle,
  HardDrive,
  RefreshCw,
} from "lucide-react";
import { useSearchParams } from "react-router-dom";

import { getIngressStatus, getSystemStatus, listNodes } from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import { AcmeSettingsCard } from "@/components/acme-settings-card";
import { AlertingSettingsCard } from "@/components/alerting-settings-card";
import { BackupsCard } from "@/components/backups-card";
import {
  EnvelopeAlert,
  EnvelopeAlertFrom,
} from "@/components/envelope-alert";
import { JoinWizard } from "@/components/join-wizard";
import { MetricsSettingsCard } from "@/components/metrics-settings-card";
import { NotificationsSettingsCard, SmtpSettingsCard } from "@/components/notifications-settings-card";
import { PageHeader } from "@/components/page-header";
import { PillTabs } from "@/components/pill-tabs";
import { S3SettingsCard } from "@/components/s3-settings-card";
import { StatCard } from "@/components/stat-card";
import { StatusDot } from "@/components/status-dot";
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

const TABS = [
  { key: "components", label: "Components" },
  { key: "nodes", label: "Nodes" },
  { key: "ingress", label: "Ingress" },
  { key: "metrics", label: "Metrics" },
  { key: "alerts", label: "Alerts" },
  { key: "notifications", label: "Notifications" },
  { key: "storage", label: "Storage" },
];

// HA 边界诚实口径（multi-node §2.9：节点页固定卡片，架构 §2.6 口径的
// UI 化——「2 台 ≠ 全面 HA」必须显式呈现，防止有状态单点被当产品缺陷）。
const HA_GET = [
  "Stateless process-level HA: loss of contact judged in ~13s, reschedule completes in ~19s; the app is briefly unavailable during the rescheduling window.",
  "Nodes can be drained for maintenance with zero failed new connections (connection-level retry; in-flight connections may break once — remove DNS records first).",
  "Control-plane failure does not affect running apps (apps do not depend on the control plane at runtime).",
];
const HA_NOT = [
  "Management-plane HA (1 manager; with quorum=2 losing any node takes management down — 2-manager setups are not offered, 3 managers are required).",
  "Stateful HA (local volumes do not follow rescheduling; a database on a lost node stays unavailable until backup-restore + rebind).",
  "Image-distribution HA (zot is pinned to the manager; new pulls/rollbacks fail while it is down — running apps are unaffected).",
  "Config-channel HA (each node's Traefik freezes its last good config while the manager is unreachable — ingress keeps serving, config stops changing).",
  "Health-driven failover / VIP at the ingress (redundancy = connection-level retry only).",
];

function HealthDot({ ok }: { ok: boolean }) {
  return (
    <span className="inline-flex items-center gap-1.5 text-xs font-medium">
      <StatusDot state={ok ? "running" : "failed"} />
      {ok ? "healthy" : "unhealthy"}
    </span>
  );
}

export function SystemPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const tab = TABS.some((t) => t.key === searchParams.get("tab"))
    ? (searchParams.get("tab") as string)
    : "components";

  const status = useQuery({
    queryKey: ["system", "status"],
    queryFn: getSystemStatus,
    refetchInterval: 10000,
  });
  const nodes = useQuery({
    queryKey: ["system", "nodes"],
    queryFn: listNodes,
    refetchInterval: 15000,
  });
  const ingress = useQuery({
    queryKey: ["system", "ingress"],
    queryFn: getIngressStatus,
    refetchInterval: 15000,
  });

  const components = status.data?.components ?? [];
  const okComponents = components.filter((c) => c.ok).length;
  const allHealthy = components.length > 0 && okComponents === components.length;

  const nodeList = nodes.data?.nodes ?? [];
  const managers = nodeList.filter((n) => n.is_manager).length;
  const stale = nodeList.filter((n) => n.stale).length;

  // 备份健康（BackupHealth）：最近一次备份的类别/时间/回读校验。
  const backup = status.data?.backup;

  return (
    <div className="space-y-4">
      <PageHeader
        title="System"
        description="Control plane, nodes and ingress at a glance."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              void status.refetch();
              void nodes.refetch();
              void ingress.refetch();
            }}
          >
            <RefreshCw aria-hidden className="h-3.5 w-3.5" />
            Refresh
          </Button>
        }
      />

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <StatCard
          label="Control plane"
          value={status.data?.version ? `v${status.data.version}` : "—"}
          sub={
            <span className="flex items-center gap-1.5">
              <StatusDot state={allHealthy ? "running" : "degraded"} />
              {status.data?.service ?? "fleetlyd"}
            </span>
          }
        />
        <StatCard
          label="Components"
          value={status.isPending ? "—" : `${okComponents}/${components.length}`}
          sub={allHealthy ? "all healthy" : "check components"}
        />
        <StatCard
          label="Nodes"
          value={nodes.isPending ? "—" : nodeList.length}
          sub={`${managers} manager${stale > 0 ? ` · ${stale} stale` : ""}`}
        />
        <StatCard
          label="Last backup"
          value={backup?.last_backup_at ? timeAgo(backup.last_backup_at) : "—"}
          sub={
            backup?.last_backup_at ? (
              <span className="flex items-center gap-1.5">
                <StatusDot
                  state={backup.last_verify_status === "verified" ? "running" : "failed"}
                />
                {backup.last_kind} · {backup.last_verify_status}
              </span>
            ) : (
              "no backups observed yet"
            )
          }
        />
      </div>

      <PillTabs
        ariaLabel="System sections"
        value={tab}
        onValueChange={(key) => setSearchParams(key === "components" ? {} : { tab: key })}
        items={TABS}
      />

      {tab === "metrics" ? (
        // metrics 设置卡（E6 W5-S3，D-W5-2 opt-in）：模式开关 + 栈状态 +
        // 诚实「worker 节点需 overlay 数据面」文案。
        <MetricsSettingsCard />
      ) : null}

      {tab === "alerts" ? (
        // 告警设置卡（B 线 W5-S2，D-V3W5-1）：alerts.mode 开关（前置门
        // metrics.mode=on——off 时禁用态提示）+ vmalert 状态 + 规则管理
        //（expr 编辑 + Test 即时求值 + 通道绑定勾选）。
        <AlertingSettingsCard />
      ) : null}

      {tab === "notifications" ? (
        // notifications 设置卡（E6 W5-S4 通知 Webhook；W4-S3 通道扩展）：
        // 端点清单 + 创建（通道类型选择，secret 一次性弹显）+ 投递台账抽屉
        // + 终败红态 + 平台级 SMTP 设置卡（email 端点共用一份）。
        <div className="space-y-4">
          <NotificationsSettingsCard />
          <SmtpSettingsCard />
        </div>
      ) : null}

      {tab === "storage" ? (
        <div className="space-y-4">
          <BackupsCard />
          <S3SettingsCard />
        </div>
      ) : null}

      {tab === "components" ? (
        <div className="space-y-4">
          <Card>
            <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
              <GaugeCircle aria-hidden className="h-4 w-4 text-muted-foreground" />
              <CardTitle className="text-sm font-semibold">Component health</CardTitle>
            </CardHeader>
          <CardContent className="pt-4">
            {status.isError ? (
              <EnvelopeAlertFrom envelope={errorEnvelopeFrom(status.error)} />
            ) : (
              <div className="grid min-w-0 grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
                {components.map((c) => (
                  <div
                    key={c.name}
                    className="flex min-w-0 items-center justify-between gap-2 rounded-md border p-3"
                    data-testid="component-health"
                  >
                    <code className="shrink-0 text-xs">{c.name}</code>
                    <span className="flex min-w-0 items-center gap-2">
                      {c.error ? (
                        <span
                          className="min-w-0 flex-1 truncate text-xs text-red-600 dark:text-red-400"
                          title={c.error}
                        >
                          {c.error}
                        </span>
                      ) : null}
                      <HealthDot ok={c.ok ?? false} />
                    </span>
                  </div>
                ))}
                {status.isPending ? (
                  <p className="text-sm text-muted-foreground">Loading…</p>
                ) : null}
              </div>
            )}
            {backup?.last_error ? (
              <div className="mt-3">
                <EnvelopeAlert
                  code="backup_verify_failed"
                  message={`last backup (${backup.last_backup_id}) failed verification`}
                  suggestion={backup.last_error}
                />
              </div>
            ) : null}
          </CardContent>
          </Card>
        </div>
      ) : null}

      {tab === "nodes" ? (
        <div className="space-y-4">
          <JoinWizard />

          <Card>
            <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
              <HardDrive aria-hidden className="h-4 w-4 text-muted-foreground" />
              <CardTitle className="text-sm font-semibold">Nodes</CardTitle>
            </CardHeader>
            <CardContent className="pt-4">
              {nodes.isError ? (
                <EnvelopeAlert
                  code={errorEnvelopeFrom(nodes.error).code}
                  message={errorEnvelopeFrom(nodes.error).message}
                  suggestion={errorEnvelopeFrom(nodes.error).suggestion}
                />
              ) : nodeList.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  No nodes observed yet (Docker Swarm idle or unreachable).
                </p>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Hostname</TableHead>
                      <TableHead>State</TableHead>
                      <TableHead>Availability</TableHead>
                      <TableHead>Manager</TableHead>
                      <TableHead>Platform ID</TableHead>
                      <TableHead>Pinned apps</TableHead>
                      <TableHead>Observed</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {nodeList.map((n) => (
                      <TableRow key={n.swarm_node_id}>
                        <TableCell>{n.hostname}</TableCell>
                        <TableCell className="text-xs">
                          {n.state}
                          {n.stale ? (
                            <span className="ml-1 text-amber-600 dark:text-amber-400">(stale)</span>
                          ) : null}
                        </TableCell>
                        <TableCell className="text-xs">{n.availability}</TableCell>
                        <TableCell className="text-xs">
                          {n.is_manager ? "yes" : "no"}
                        </TableCell>
                        <TableCell className="font-mono text-xs">
                          {n.platform_id || "—"}
                        </TableCell>
                        <TableCell className="text-xs">
                          {(n.pinned_app_ids?.length ?? 0) > 0
                            ? `${n.pinned_app_ids?.length} app${(n.pinned_app_ids?.length ?? 0) > 1 ? "s" : ""}`
                            : "—"}
                        </TableCell>
                        <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                          {formatTime(n.observed_at)}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
              <CardTitle className="text-sm font-semibold">
                High-availability boundary
              </CardTitle>
              <CardDescription className="ml-auto text-xs">
                What 2 nodes get — and what they do not. 2 nodes ≠ full HA.
              </CardDescription>
            </CardHeader>
            <CardContent className="pt-4" data-testid="ha-boundary">
              <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
                <div>
                  <div className="mb-2 text-xs font-semibold uppercase tracking-wide text-emerald-600 dark:text-emerald-400">
                    You get
                  </div>
                  <ul className="list-disc space-y-2 pl-5 text-xs">
                    {HA_GET.map((line) => (
                      <li key={line}>{line}</li>
                    ))}
                  </ul>
                </div>
                <div>
                  <div className="mb-2 text-xs font-semibold uppercase tracking-wide text-red-600 dark:text-red-400">
                    You do not get
                  </div>
                  <ul className="list-disc space-y-2 pl-5 text-xs">
                    {HA_NOT.map((line) => (
                      <li key={line}>{line}</li>
                    ))}
                  </ul>
                </div>
              </div>
            </CardContent>
          </Card>
        </div>
      ) : null}

      {tab === "ingress" ? (
        <div className="space-y-4">
          {/* ACME DNS-01 设置卡（B 线 W5-S3，D-V3W5-3/D-V3W5-4）：DNS 服务商
              选择 + 凭证 write-only + 探针 + 通配开关 + 当前证书域集展示
              ——与证书台账同域（ingress 页签）。 */}
          <AcmeSettingsCard />

          <Card>
            <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
              <Boxes aria-hidden className="h-4 w-4 text-muted-foreground" />
              <CardTitle className="text-sm font-semibold">Ingress</CardTitle>
            </CardHeader>
            <CardContent className="pt-4">
              {ingress.isError ? (
                <EnvelopeAlert
                  code={errorEnvelopeFrom(ingress.error).code}
                  message={errorEnvelopeFrom(ingress.error).message}
                  suggestion={errorEnvelopeFrom(ingress.error).suggestion}
                />
              ) : !ingress.data ? (
                <p className="text-sm text-muted-foreground">Loading…</p>
              ) : (
                <div className="space-y-3 text-sm">
                  <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
                    <div className="rounded-md border p-3">
                      <div className="text-xs text-muted-foreground">Traefik</div>
                      {ingress.data.traefik?.exists ? (
                        <>
                          <code className="text-xs">{ingress.data.traefik.image}</code>
                          <div className="text-xs text-muted-foreground">
                            {ingress.data.traefik.static_args} static args
                          </div>
                        </>
                      ) : (
                        <span className="text-xs text-red-600 dark:text-red-400">
                          {ingress.data.traefik?.error || "not present"}
                        </span>
                      )}
                    </div>
                    <div className="rounded-md border p-3">
                      <div className="text-xs text-muted-foreground">Config endpoint</div>
                      <code className="text-xs">{ingress.data.config_addr}</code>
                      <div className="text-xs">
                        healthz: {ingress.data.healthz || "—"}
                      </div>
                      <div className="text-xs">auth: {ingress.data.auth || "—"}</div>
                    </div>
                  </div>
                </div>
              )}
            </CardContent>
          </Card>

          {ingress.data && (ingress.data.certificates ?? []).length > 0 ? (
            <Card>
              <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
                <DatabaseBackup aria-hidden className="h-4 w-4 text-muted-foreground" />
                <CardTitle className="text-sm font-semibold">Certificates</CardTitle>
              </CardHeader>
              <CardContent className="pt-4">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>App</TableHead>
                      <TableHead>Domain</TableHead>
                      <TableHead>Expires</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {(ingress.data.certificates ?? []).map((c) => (
                      <TableRow key={`${c.app}:${c.domain}`}>
                        <TableCell>{c.app}</TableCell>
                        <TableCell className="font-mono text-xs">{c.domain}</TableCell>
                        <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                          {formatTime(c.cert_not_after)}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </CardContent>
            </Card>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
