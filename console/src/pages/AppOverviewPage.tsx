// 概览页：基本信息 + 服务清单（cron 服务标注 scheduled——compose 声明但
// 非长驻，不冒充长驻态）+ 放置/卷概览（服务拓扑明细在 revision spec）+
// cron 区块（运行台账 + 手动触发）+ Danger Zone（应用删除）。卡片分区：
// Application / Placement / Services / Volumes / Scheduled jobs / Danger Zone。

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, Boxes, Layers, MapPin, PackageOpen } from "lucide-react";
import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";

import {
  deleteApp,
  getApp,
  getPlacement,
  getRevisionSpec,
  listRevisions,
} from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import { timeAgo } from "@/lib/utils";
import { AppDriftCard } from "@/components/app-drift-card";
import { AppMetricsCard } from "@/components/app-metrics-card";
import { AppProjectNetworkCard } from "@/components/app-project-network-card";
import { AppScalingCard } from "@/components/app-scaling-card";
import { CronSection } from "@/components/cron-section";
import { DegradedExplanationCardLive } from "@/components/degraded-explanation-card";
import { EnvelopeAlert } from "@/components/envelope-alert";
import { StatusDot } from "@/components/status-dot";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { StateBadge } from "@/components/state-badge";
import { useIsPlatformAdmin, useTeamCapabilities } from "@/lib/context";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { extractServiceNames } from "@/lib/compose-cron";

function Field({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-4 text-sm">
      <span className="text-muted-foreground">{label}</span>
      <span className="text-right font-medium">{value}</span>
    </div>
  );
}

// Danger Zone 卡（backlog #4-①，2026-09-25 审查 §7/§4.11）：应用删除入口。
// 删除语义取自服务端代码事实（internal/api/apps.go DeleteApp +
// internal/engine/appdelete.go），如实写进确认文案：
//   - 第一拍（API 同步）：active → deleting 墓碑 + 路由即时撤销（域名停摆）；
//   - 第二拍（引擎收敛 duty，约 10s 一拍）：在途部署等终态 → 受管服务逐个
//     移除 → Swarm secret 扫尾 → deleted；失败保持 deleting 下拍重试；
//   - 数据卷不随删除清理（引擎无卷处理路径）——节点上孤儿保留、可手工恢复；
//   - app 行不物理删除且名字唯一约束仍在——名字永久占用、不可复用。
// 角色门（资源面写 = admin+，前端体验门）：平台管理员双门只读（P0-3），
// 按 platform-readonly-note 既有形态原位说明；viewer/developer 整卡不渲染。
function DangerZoneCard({ name, displayName }: { name: string; displayName: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { canAdminResources } = useTeamCapabilities();
  const isPlatformAdmin = useIsPlatformAdmin();
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState<ReturnType<typeof errorEnvelopeFrom> | null>(null);

  const del = useMutation({
    mutationFn: () => deleteApp(name),
    onSuccess: () => {
      setError(null);
      // 删除即离开详情语境：失效详情/列表/放置缓存后回应用列表（详情查询
      // 对 deleting 墓碑仍可读，但本会话已完成删除意图，不再驻留）。
      void queryClient.invalidateQueries({ queryKey: ["app", name] });
      void queryClient.invalidateQueries({ queryKey: ["apps"] });
      void queryClient.invalidateQueries({ queryKey: ["placement", name] });
      navigate("/apps");
    },
    onError: (err) => setError(errorEnvelopeFrom(err)),
  });

  if (!canAdminResources && !isPlatformAdmin) return null;

  return (
    <Card data-testid="danger-zone" className="border-red-200 dark:border-red-900/40">
      <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
        <AlertTriangle aria-hidden className="h-4 w-4 text-red-600 dark:text-red-400" />
        <CardTitle className="text-sm font-semibold">Danger Zone</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3 pt-4">
        {canAdminResources ? (
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="min-w-0">
              <div className="text-sm font-medium">Delete application</div>
              <p className="text-xs text-muted-foreground">
                Tombstones the app and withdraws its routes immediately; managed
                services are reaped in the background. Data volumes are kept and
                the name stays reserved. This cannot be undone.
              </p>
            </div>
            <Button
              variant="destructive"
              size="sm"
              data-testid="app-delete-button"
              onClick={() => {
                setConfirm("");
                setError(null);
                setConfirmOpen(true);
              }}
            >
              Delete application
            </Button>
          </div>
        ) : (
          // P0-3 双门：平台管理员资源面只读——说明行原位渲染（CLI 有应用
          // 删除命令，文案如实指路），不做静默消失。
          <p className="text-xs text-muted-foreground" data-testid="platform-readonly-note">
            Platform administrators have read-only access to resources
            (separation of duties). Delete applications from the CLI with a
            machine token, or ask a team owner for a member role.
          </p>
        )}

        {confirmOpen ? (
          <Dialog open onOpenChange={(v) => (v ? undefined : setConfirmOpen(false))}>
            <DialogContent data-testid="app-delete-dialog">
              <DialogHeader>
                <DialogTitle>Delete application {displayName}</DialogTitle>
                <DialogDescription>
                  Deletion is a two-beat tombstone: routes are withdrawn
                  immediately and the app enters deleting; managed services are
                  removed shortly after by background convergence (in-flight
                  deployments finish first). Data volumes are kept on the node
                  as orphans (recoverable by hand) and deployment history is
                  retained. The name stays reserved — it cannot be reused for a
                  new application.
                </DialogDescription>
              </DialogHeader>
              <div className="space-y-1.5">
                <Label htmlFor="app-delete-confirm">
                  Type the application name{" "}
                  <span className="font-mono">{displayName}</span> to confirm
                </Label>
                <Input
                  id="app-delete-confirm"
                  data-testid="app-delete-confirm-input"
                  className="font-mono text-xs"
                  value={confirm}
                  onChange={(e) => setConfirm(e.target.value)}
                />
              </div>
              {error ? (
                <EnvelopeAlert
                  code={error.code}
                  message={error.message}
                  suggestion={error.suggestion}
                />
              ) : null}
              <DialogFooter>
                <Button
                  variant="destructive"
                  data-testid="app-delete-submit"
                  disabled={confirm !== displayName || del.isPending}
                  onClick={() => del.mutate()}
                >
                  {del.isPending ? "Deleting…" : "Delete application"}
                </Button>
              </DialogFooter>
            </DialogContent>
          </Dialog>
        ) : null}
      </CardContent>
    </Card>
  );
}

export function AppOverviewPage() {
  const { name = "" } = useParams();
  const appQuery = useQuery({
    queryKey: ["app", name],
    queryFn: () => getApp(name),
    refetchInterval: 5000,
  });
  const placementQuery = useQuery({
    queryKey: ["placement", name],
    queryFn: () => getPlacement(name),
  });

  // 服务清单：最近 active revision 的归一化快照（与 cron 区块同源同查询）。
  const revisionsQuery = useQuery({
    queryKey: ["revisions", name],
    queryFn: () => listRevisions(name),
  });
  const active = (revisionsQuery.data?.revisions ?? []).find(
    (r) => r.status === "active",
  );
  const specQuery = useQuery({
    queryKey: ["revision-spec", name, active?.id],
    queryFn: () => getRevisionSpec(name, active!.id ?? ""),
    enabled: active !== undefined,
  });
  const services = extractServiceNames(specQuery.data?.compose);
  const hasCron = services !== null && services.some((s) => s.isCron);

  const app = appQuery.data;
  const placement = placementQuery.data?.placement;
  const volumes = placementQuery.data?.volumes ?? [];

  return (
    <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
      {/* degraded 一等 UI（W5-S2）：派生状态 degraded 时常驻解释卡（事件
          订阅只在 degraded 态挂载——ready 零额外流）。 */}
      {app?.derived_state === "degraded" ? (
        <div className="md:col-span-2">
          <DegradedExplanationCardLive app={name} />
        </div>
      ) : null}
      <Card>
        <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
          <Layers aria-hidden className="h-4 w-4 text-muted-foreground" />
          <CardTitle className="text-sm font-semibold">Application</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3 pt-4">
          {app ? (
            <>
              <Field label="Derived state" value={<StateBadge state={app.derived_state ?? ""} />} />
              <Field label="Lifecycle" value={<code>{app.lifecycle}</code>} />
              {/* Created/Updated 统一相对时间（2026-09-25 审查 P2-3：同一张
                  卡绝对/相对并存），绝对值留 title tooltip——与全站列表一致。 */}
              <Field label="Created" value={<span title={app.created_at}>{timeAgo(app.created_at)}</span>} />
              <Field label="Updated" value={<span title={app.updated_at}>{timeAgo(app.updated_at)}</span>} />
              <Field label="ID" value={<code className="text-xs">{app.id}</code>} />
            </>
          ) : (
            <p className="text-sm text-muted-foreground">Loading…</p>
          )}
        </CardContent>
      </Card>

      {/* 项目网卡（IMPL-T15-1/OT-1）：归属项目 + 参与状态 + attach/detach
          （admin 角色；平台管理员只读说明）——紧随 Application 卡。 */}
      {app ? <AppProjectNetworkCard app={app} /> : null}

      <Card>
        <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
          <MapPin aria-hidden className="h-4 w-4 text-muted-foreground" />
          <CardTitle className="text-sm font-semibold">Placement</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3 pt-4">
          {placement ? (
            <>
              <Field
                label="Binding state"
                value={<StateBadge state={placement.state ?? ""} />}
              />
              <Field
                label="Node"
                value={
                  <code className="text-xs">
                    {placement.label_ref || placement.platform_node_id}
                  </code>
                }
              />
              {placement.reason ? (
                <Field label="Reason" value={placement.reason} />
              ) : null}
            </>
          ) : (
            <p className="text-sm text-muted-foreground">
              No placement binding (stateful placement not declared).
            </p>
          )}
        </CardContent>
      </Card>

      {/* Drift 卡（backlog #9，2026-09-25 审查 §3 P1-5）：运行域漂移读面
          （全角色）+ Converge now（deploy 面）+ 自动收敛 opt-in 开关
          （admin 面）——紧随 Placement 卡。 */}
      <div className="md:col-span-2">
        <AppDriftCard app={name} />
      </div>

      <Card className="md:col-span-2">
        <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
          <Boxes aria-hidden className="h-4 w-4 text-muted-foreground" />
          <CardTitle className="text-sm font-semibold">Services</CardTitle>
        </CardHeader>
        <CardContent className="pt-4" data-testid="services-list">
          {services === null ? (
            <p className="text-xs text-muted-foreground">
              Revision spec is unreadable — services cannot be listed.
            </p>
          ) : services.length === 0 ? (
            <p className="text-xs text-muted-foreground">
              No revision deployed yet.
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Service</TableHead>
                  <TableHead>Mode</TableHead>
                  <TableHead>Replicas</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {services.map((s) => (
                  <TableRow key={s.name}>
                    <TableCell className="font-mono text-xs">{s.name}</TableCell>
                    <TableCell>
                      {s.isCron ? (
                        // cron 服务 = 按点触发的一次性 job：如实标注 scheduled，
                        // 不冒充长驻 running 态（架构 §4.3）。
                        <span
                          data-testid="service-scheduled-badge"
                          className="inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 text-xs font-semibold"
                        >
                          <StatusDot tone="neutral-blue" />
                          scheduled
                        </span>
                      ) : (
                        <span className="text-xs text-muted-foreground">long-running</span>
                      )}
                    </TableCell>
                    {/* 声明副本数常驻显示（W5-S3 挂账收敛；compose 缺省 1，
                        global = 每节点一任务。实际每副本水位在 Resources 卡）。 */}
                    <TableCell>
                      {s.isCron ? (
                        <span className="text-xs text-muted-foreground">—</span>
                      ) : (
                        <span className="text-xs" data-testid="service-replicas">
                          {s.replicas === "global" ? "global (per node)" : s.replicas}
                        </span>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {volumes.length > 0 ? (
        <Card className="md:col-span-2">
          <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
            <PackageOpen aria-hidden className="h-4 w-4 text-muted-foreground" />
            <CardTitle className="text-sm font-semibold">Volumes</CardTitle>
          </CardHeader>
          <CardContent className="pt-4">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Key</TableHead>
                  <TableHead>Kind</TableHead>
                  <TableHead>Mount</TableHead>
                  <TableHead>Status</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {volumes.map((v) => (
                  <TableRow key={v.key}>
                    <TableCell className="font-mono text-xs">{v.key}</TableCell>
                    <TableCell>{v.kind}</TableCell>
                    <TableCell className="font-mono text-xs">{v.mount_path}</TableCell>
                    <TableCell>{v.status}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      ) : null}

      {hasCron ? <CronSection app={name} /> : null}

      {/* 资源卡（E6 W5-S3）：mode=on → 容器曲线 + 每副本水位；unset →
          opt-in 引导 + 开关。 */}
      <div className="md:col-span-2">
        <AppMetricsCard app={name} />
      </div>

      {/* 自动扩缩策略卡（W5-S1，D-V3W5-2）：只列长驻服务（cron 无副本
          语义）——策略展示/编辑/删除（admin+ 可写）；metrics off 休眠态
          提示。 */}
      {services !== null ? (
        <div className="md:col-span-2">
          <AppScalingCard
            app={name}
            services={services.filter((s) => !s.isCron).map((s) => s.name)}
          />
        </div>
      ) : null}

      {/* Danger Zone（backlog #4-①）：应用删除（admin+ 可见；平台管理员
          只读说明）。displayName 为 id 寻址时的反解业务名——确认输入对齐
          用户看到的标题名。 */}
      <div className="md:col-span-2">
        <DangerZoneCard name={name} displayName={app?.name ?? name} />
      </div>
    </div>
  );
}
