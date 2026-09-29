// 应用列表（dokploy Services 表式；2026-09-29 §5 与 Databases 页统一骨架
// ——PageHeader〔title+desc+Refresh+Create〕→ 统计卡行 → 工具栏 → 表格卡）：
// 卡片内表格 + 搜索/状态/生命周期筛选 + 排序 + 行点击进详情。derived_state
// 徽章一等展示（degraded/blocked/suspended 语义色与 StateBadge 同源）。
// 筛选与排序均为客户端投影——列表读面无服务端分页参数，全量数据量级（单
// 操作员平台）客户端处理即可。创建 = 带归属声明的首次 deploy 入队（deploy
// 面，developer+；平台管理员 P0-3 说明卡原位——与 Databases 页同款）。

import { useQuery } from "@tanstack/react-query";
import { Boxes, ChevronRight, Plus, Search } from "lucide-react";
import { useMemo, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";

import { listApps } from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import type { AppView } from "@/api/types";
import { CreateAppDialog } from "@/components/create-app-dialog";
import { DegradedExplanationCard } from "@/components/degraded-explanation-card";
import { EnvelopeAlert } from "@/components/envelope-alert";
import { EmptyState } from "@/components/empty-state";
import { PageHeader } from "@/components/page-header";
import { StateBadge } from "@/components/state-badge";
import { StatCard } from "@/components/stat-card";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardFooter,
} from "@/components/ui/card";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Input } from "@/components/ui/input";
import { useIsPlatformAdmin, useProjectContext, useTeamCapabilities } from "@/lib/context";
import { timeAgo } from "@/lib/utils";

type StateFilter = "all" | "healthy" | "degraded" | "suspended" | "unavailable";
type LifecycleFilter = "all" | "active" | "deleting";
type SortKey = "updated" | "created" | "name";

function matchStateFilter(state: string | undefined, filter: StateFilter): boolean {
  switch (filter) {
    case "healthy":
      return state === "running";
    case "degraded":
      return state === "degraded";
    case "suspended":
      return state === "suspended";
    case "unavailable":
      return ["blocked", "down", "failed"].includes(state ?? "");
    default:
      return true;
  }
}

const STATE_FILTERS: { key: StateFilter; label: string }[] = [
  { key: "all", label: "All states" },
  { key: "healthy", label: "Running" },
  { key: "degraded", label: "Degraded" },
  { key: "suspended", label: "Suspended" },
  { key: "unavailable", label: "Unavailable" },
];

function AppRow({ app }: { app: AppView }) {
  const navigate = useNavigate();
  const name = app.name ?? "";
  // 详情导航以平台 id 寻址（2026-09-25 走查裁决）：同名 app 的裸名解析必
  // 歧义（E_APP_AMBIGUOUS——点行进错误页的断层），REST 单段路由也承载不了
  // team/prj/app 三段；id 全库唯一且服务端 resolveApp 支持 id 短路。
  const href = `/apps/${encodeURIComponent(app.id ?? "")}`;
  // 副标题 = 归属限定形 team/prj（S3 投影）；旧服务端缺投影时回退 id。
  const ownership =
    app.team_slug && app.project_slug ? `${app.team_slug}/${app.project_slug}` : app.id;
  return (
    <TableRow
      data-testid="app-row"
      data-state={app.derived_state}
      className="cursor-pointer"
      onClick={() => navigate(href)}
    >
      <TableCell>
        <div className="flex min-w-0 items-center gap-3">
          <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-md border bg-muted/40">
            <Boxes aria-hidden className="h-4 w-4 text-muted-foreground" />
          </span>
          <span className="min-w-0">
            <Link
              to={href}
              className="block truncate font-medium hover:underline"
              onClick={(e) => e.stopPropagation()}
            >
              {name}
            </Link>
            <span className="block truncate font-mono text-xs text-muted-foreground">
              {ownership}
            </span>
            {/* degraded 一等 UI（W5-S2）：行内常驻解释（compact 卡）——
                不再是只有 badge 的二等态；点击链接进事件流（行点击语义
                不受影响——链接 stopPropagation）。 */}
            {app.derived_state === "degraded" ? (
              <div onClick={(e) => e.stopPropagation()}>
                <DegradedExplanationCard app={name} compact />
              </div>
            ) : null}
          </span>
        </div>
      </TableCell>
      <TableCell>
        <StateBadge state={app.derived_state ?? ""} />
      </TableCell>
      <TableCell className="text-xs text-muted-foreground">
        {app.lifecycle}
      </TableCell>
      <TableCell
        className="whitespace-nowrap text-xs text-muted-foreground"
        title={app.updated_at}
      >
        {timeAgo(app.updated_at)}
      </TableCell>
      <TableCell
        className="whitespace-nowrap text-xs text-muted-foreground"
        title={app.created_at}
      >
        {timeAgo(app.created_at)}
      </TableCell>
      <TableCell className="w-10 text-right">
        <ChevronRight aria-hidden className="ml-auto h-4 w-4 text-muted-foreground/60" />
      </TableCell>
    </TableRow>
  );
}

export function AppsPage() {
  // 项目上下文收窄（W2-S5 顶栏切换器）：选中项目时请求带 ?project=team/prj
  // （服务端过滤）；只选团队时服务端无 team 参数——按 AppView 归属投影
  // （team_slug）客户端收窄（2026-09-25 走查实爆：选团队后列表仍全量）。
  // queryKey 随 ref 变化——切换即重查。
  const { selectedTeamSlug, selectedProjectSlug, projectRef } = useProjectContext();
  // 创建钮角色门（§5 与 Databases 页统一；创建 = 首署 deploy，developer+
  // 体验门）。平台管理员资源面恒只读（P0-3 双门）——创建钮消失时以说明卡
  // 明示原因，不做静默消失。
  const { canDeploy } = useTeamCapabilities();
  const isPlatformAdmin = useIsPlatformAdmin();
  const query = useQuery({
    queryKey: ["apps", projectRef],
    queryFn: () => listApps(projectRef ? { project: projectRef } : {}),
    refetchInterval: 5000,
  });
  const [createOpen, setCreateOpen] = useState(false);

  // 创建应用对话框的落地说明（P0-1）：DeployResponse 不带应用平台 id，
  // 对话框入队成功后导航到本页并携带 location.state——在此渲染一次性
  // queued 通知（导航离开即消失，不持久化）。
  const location = useLocation();
  const queuedNotice = (
    location.state as { deployQueued?: { app?: string; deploymentId?: string } } | null
  )?.deployQueued;

  const [search, setSearch] = useState("");
  const [stateFilter, setStateFilter] = useState<StateFilter>("all");
  const [lifecycleFilter, setLifecycleFilter] = useState<LifecycleFilter>("all");
  const [sortKey, setSortKey] = useState<SortKey>("updated");

  // 生成类型口径：gateway EmitUnpopulated=true 下空 repeated 显式输出 []，
  // `?? []` 保留为历史响应/测试桩的形态兜底。useMemo 包一层：引用稳定，
  // 下游筛选 memo 的依赖才不会每渲染刷新。
  const apps = useMemo(() => query.data?.apps ?? [], [query.data]);

  const visible = useMemo(() => {
    const needle = search.trim().toLowerCase();
    const filtered = apps.filter((a) => {
      if (selectedTeamSlug && !selectedProjectSlug && a.team_slug !== selectedTeamSlug) {
        return false;
      }
      if (needle && !(a.name ?? "").toLowerCase().includes(needle) && !(a.id ?? "").toLowerCase().includes(needle)) {
        return false;
      }
      if (!matchStateFilter(a.derived_state, stateFilter)) return false;
      if (lifecycleFilter !== "all" && (a.lifecycle ?? "active") !== lifecycleFilter) {
        return false;
      }
      return true;
    });
    return filtered.sort((a, b) => {
      switch (sortKey) {
        case "name":
          return (a.name ?? "").localeCompare(b.name ?? "");
        case "created":
          return (b.created_at ?? "").localeCompare(a.created_at ?? "");
        default:
          return (b.updated_at ?? "").localeCompare(a.updated_at ?? "");
      }
    });
  }, [apps, search, stateFilter, lifecycleFilter, sortKey, selectedTeamSlug, selectedProjectSlug]);

  const filtered = visible.length !== apps.length;

  // 统计卡行（与 Databases 页同骨架）：总数/运行/挂起/需注意——派生态计数
  // 客户端投影（挂起是权威位直投影态，一等计数）。
  const counts = useMemo(() => {
    const by: Record<string, number> = {};
    for (const a of apps) {
      by[a.derived_state ?? ""] = (by[a.derived_state ?? ""] ?? 0) + 1;
    }
    return by;
  }, [apps]);

  // P0-3 只读说明：Create application 按钮因平台管理员身份隐藏时，落一张
  // 说明卡（与 Databases 页同款；三个返回形态共享——pending/error/ready
  // 的页头都在）。
  const platformReadonlyNote = !canDeploy && isPlatformAdmin ? (
    <Card className="border-dashed">
      <CardContent
        className="p-4 text-sm text-muted-foreground"
        data-testid="platform-readonly-note"
      >
        Platform administrators have read-only access to resources (separation
        of duties). Create and deploy applications from the CLI with a machine
        token, or ask a team owner for a member role.
      </CardContent>
    </Card>
  ) : null;

  const header = (
    <PageHeader
      title="Applications"
      description="Compose-deployed applications and their derived health."
      actions={
        <>
          <Button
            variant="outline"
            size="sm"
            onClick={() => void query.refetch()}
            aria-label="Refresh apps"
          >
            Refresh
          </Button>
          {canDeploy ? (
            <Button size="sm" data-testid="app-create-button" onClick={() => setCreateOpen(true)}>
              <Plus aria-hidden className="h-3.5 w-3.5" />
              Create application
            </Button>
          ) : null}
        </>
      }
    />
  );

  if (query.isPending) {
    return (
      <div className="space-y-4" data-testid="apps-page">
        {header}
        {platformReadonlyNote}
        <p className="text-sm text-muted-foreground">Loading apps…</p>
      </div>
    );
  }
  if (query.isError) {
    const envelope = errorEnvelopeFrom(query.error);
    return (
      <div className="space-y-4" data-testid="apps-page">
        {header}
        {platformReadonlyNote}
        <EnvelopeAlert
          code={envelope.code}
          message={envelope.message}
          suggestion={envelope.suggestion}
          docs={envelope.docs}
        />
      </div>
    );
  }

  return (
    <div className="space-y-4" data-testid="apps-page">
      {header}
      {platformReadonlyNote}
      <div className="grid grid-cols-2 gap-4 md:grid-cols-4">
        <StatCard label="Applications" value={apps.length} />
        <StatCard label="Running" value={counts.running ?? 0} />
        <StatCard label="Suspended" value={counts.suspended ?? 0} sub="stopped by request" />
        <StatCard
          label="Attention"
          value={(counts.degraded ?? 0) + (counts.blocked ?? 0) + (counts.down ?? 0)}
          sub="degraded + blocked + down"
        />
      </div>

      {queuedNotice ? (
        <div
          data-testid="deploy-queued-notice"
          role="status"
          className="rounded-md border border-emerald-500/30 bg-emerald-500/5 p-3 text-sm text-emerald-800 dark:text-emerald-300"
        >
          Application “{queuedNotice.app ?? ""}” queued for deployment
          {queuedNotice.deploymentId ? (
            <>
              {" "}(<code className="font-mono text-xs">{queuedNotice.deploymentId}</code>)
            </>
          ) : null}
          . It appears below once created — open its Deployments tab to track
          progress.
        </div>
      ) : null}

      <Card>
        <div className="flex flex-wrap items-center gap-2 border-b px-4 py-3">
          <div className="relative">
            <Search
              aria-hidden
              className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground"
            />
            <Input
              aria-label="Filter applications"
              placeholder="Filter applications…"
              className="w-56 pl-8"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
            />
          </div>
          <Select value={stateFilter} onValueChange={(v) => setStateFilter(v as StateFilter)}>
            <SelectTrigger className="w-36" aria-label="Filter by state">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {STATE_FILTERS.map((f) => (
                <SelectItem key={f.key} value={f.key}>
                  {f.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Select
            value={lifecycleFilter}
            onValueChange={(v) => setLifecycleFilter(v as LifecycleFilter)}
          >
            <SelectTrigger className="w-32" aria-label="Filter by lifecycle">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">All lifecycles</SelectItem>
              <SelectItem value="active">active</SelectItem>
              <SelectItem value="deleting">deleting</SelectItem>
            </SelectContent>
          </Select>
          <Select value={sortKey} onValueChange={(v) => setSortKey(v as SortKey)}>
            <SelectTrigger className="ml-auto w-44" aria-label="Sort applications">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="updated">Recently updated</SelectItem>
              <SelectItem value="created">Newest first</SelectItem>
              <SelectItem value="name">Name</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <CardContent className="p-0">
          {apps.length === 0 ? (
            <EmptyState
              icon={Search}
              title="No applications yet."
              hint="Create one with the button above (a compose deploy with an explicit project target), or 'fleetly deploy compose.yaml' from the CLI."
            />
          ) : visible.length === 0 ? (
            <EmptyState
              icon={Search}
              title="No applications match the current filters."
              hint="Adjust the search or filter selections to widen the view."
            />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Application</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Lifecycle</TableHead>
                  <TableHead>Updated</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {visible.map((app) => (
                  <AppRow key={app.id} app={app} />
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
        <CardFooter className="justify-between border-t py-3 text-xs text-muted-foreground">
          <span>
            {filtered
              ? `${visible.length} of ${apps.length} applications`
              : `${apps.length} ${apps.length === 1 ? "application" : "applications"} total`}
          </span>
          <span className="hidden sm:inline">click a row to open</span>
        </CardFooter>
      </Card>
      {/* 创建应用对话框（与 Databases 页同款复用形态；目标项目 = 顶栏选中
          项目限定形，对话框内明示）。 */}
      <CreateAppDialog open={createOpen} onOpenChange={setCreateOpen} projectRef={projectRef} />
    </div>
  );
}
