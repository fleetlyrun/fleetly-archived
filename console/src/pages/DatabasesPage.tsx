// 库实例列表（E4 managed-databases §4 验收步 9：Console 库实例为一等页面
// ——独立资源面 /ui/databases，与 apps 分立；2026-09-29 §5 与 Applications
// 页统一骨架——PageHeader〔title+desc+Refresh+Create〕→ 统计卡行 → 工具栏
//〔搜索/状态筛选/排序，客户端投影〕→ 表格卡）。创建对话框（抽出的可复用
// 组件 components/create-database-dialog.tsx——项目详情页 Databases 卡同
// 享；目标项目 = 顶栏选中项目限定形，对话框内明示）。备份列逐行轻查询
//（limit=1，无轮询——单操作员平台量级可控）。

import { useQuery } from "@tanstack/react-query";
import { ChevronRight, Database, Plus, Search } from "lucide-react";
import { useMemo, useState } from "react";
import { Link, useNavigate } from "react-router-dom";

import {
  listDatabaseBackups,
  listDatabases,
} from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import type { DatabaseView } from "@/api/types";
import { CreateDatabaseDialog } from "@/components/create-database-dialog";
import { EmptyState } from "@/components/empty-state";
import { EnvelopeAlert } from "@/components/envelope-alert";
import { PageHeader } from "@/components/page-header";
import { StateBadge } from "@/components/state-badge";
import { StatCard } from "@/components/stat-card";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
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
import { formatBytes, timeAgo } from "@/lib/utils";
import { useIsPlatformAdmin, useProjectContext, useTeamCapabilities } from "@/lib/context";
import { useSubjectResolver } from "@/hooks/use-subject-resolver";

type DbStateFilter = "all" | "ready" | "paused" | "attention";
type DbSortKey = "updated" | "name";

const DB_STATE_FILTERS: { key: DbStateFilter; label: string }[] = [
  { key: "all", label: "All states" },
  { key: "ready", label: "Ready" },
  { key: "paused", label: "Paused" },
  { key: "attention", label: "Attention" },
];

function matchDbStateFilter(status: string | undefined, filter: DbStateFilter): boolean {
  switch (filter) {
    case "ready":
      return status === "ready";
    case "paused":
      return status === "paused";
    case "attention":
      return ["provisioning", "degraded", "failed"].includes(status ?? "");
    default:
      return true;
  }
}

/** 行内最近备份（limit=1 只取最新一行；无轮询——创建/触发后随缓存失效刷新）。 */
function LastBackupCell({ name }: { name: string }) {
  const query = useQuery({
    queryKey: ["database-backups", name],
    queryFn: () => listDatabaseBackups(name, 1),
    staleTime: 30_000,
  });
  const row = query.data?.backups?.[0];
  if (!row) {
    return <span className="text-muted-foreground">—</span>;
  }
  const failed = row.verify_status === "failed";
  return (
    <span className={failed ? "text-red-600 dark:text-red-400" : undefined}>
      {row.kind} · {timeAgo(row.created_at)}
      {failed ? " (verify failed)" : ""}
    </span>
  );
}

function DatabaseRow({ db, resolveSubject }: { db: DatabaseView; resolveSubject: (s: string | undefined) => string }) {
  const navigate = useNavigate();
  const name = db.name ?? "";
  return (
    <TableRow
      data-testid="database-row"
      data-state={db.status}
      className="cursor-pointer"
      onClick={() => navigate(`/databases/${encodeURIComponent(name)}`)}
    >
      <TableCell>
        <div className="flex min-w-0 items-center gap-3">
          <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-md border bg-muted/40">
            <Database aria-hidden className="h-4 w-4 text-muted-foreground" />
          </span>
          <span className="min-w-0">
            <Link
              to={`/databases/${encodeURIComponent(name)}`}
              className="block truncate font-medium hover:underline"
              onClick={(e) => e.stopPropagation()}
            >
              {name}
            </Link>
            <span className="block truncate font-mono text-xs text-muted-foreground">
              {db.template}
            </span>
          </span>
        </div>
      </TableCell>
      <TableCell>
        <StateBadge state={db.status ?? ""} />
      </TableCell>
      <TableCell className="font-mono text-xs text-muted-foreground" title={db.placement}>
        {resolveSubject(db.placement) || "—"}
      </TableCell>
      <TableCell className="font-mono text-xs text-muted-foreground">
        {db.volume ? `${db.volume.name} (${db.volume.status})` : "—"}
      </TableCell>
      <TableCell className="text-xs">
        <LastBackupCell name={name} />
      </TableCell>
      <TableCell className="text-xs">
        {db.upgrade_available ? (
          <span data-testid="database-upgrade-available" className="font-medium text-amber-600 dark:text-amber-400">
            upgrade available
          </span>
        ) : (
          <span className="text-muted-foreground">—</span>
        )}
      </TableCell>
      <TableCell className="text-xs text-muted-foreground">{formatBytes(db.limits?.memory_bytes)}</TableCell>
      <TableCell className="w-10 text-right">
        <ChevronRight aria-hidden className="ml-auto h-4 w-4 text-muted-foreground/60" />
      </TableCell>
    </TableRow>
  );
}

export function DatabasesPage() {
  // 项目上下文收窄（W2-S5）+ 创建按钮角色门（库生命周期 = admin+，§3.2
  // 矩阵；前端体验门，服务端硬门不变）。平台管理员资源面恒只读（P0-3
  // 双门）——创建钮消失时以说明卡明示原因，不做静默消失。
  const { projectRef } = useProjectContext();
  const { canAdminResources } = useTeamCapabilities();
  const isPlatformAdmin = useIsPlatformAdmin();
  // placement 节点 ID 可读化（W2-7）。
  const resolveSubject = useSubjectResolver();
  const query = useQuery({
    queryKey: ["databases", projectRef],
    queryFn: () => listDatabases(projectRef ? { project: projectRef } : {}),
    refetchInterval: 5000,
  });
  const [createOpen, setCreateOpen] = useState(false);

  const databases = useMemo(() => query.data?.databases ?? [], [query.data]);
  const counts = useMemo(() => {
    const by: Record<string, number> = {};
    for (const d of databases) {
      by[d.status ?? ""] = (by[d.status ?? ""] ?? 0) + 1;
    }
    return by;
  }, [databases]);

  // 工具栏（§5 与 Applications 页同款：搜索/状态筛选/排序——客户端投影）。
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState<DbStateFilter>("all");
  const [sortKey, setSortKey] = useState<DbSortKey>("updated");
  const visible = useMemo(() => {
    const needle = search.trim().toLowerCase();
    const filteredRows = databases.filter((d) => {
      if (needle && !(d.name ?? "").toLowerCase().includes(needle)) return false;
      if (!matchDbStateFilter(d.status, statusFilter)) return false;
      return true;
    });
    return filteredRows.sort((a, b) => {
      if (sortKey === "name") return (a.name ?? "").localeCompare(b.name ?? "");
      return (b.updated_at ?? "").localeCompare(a.updated_at ?? "");
    });
  }, [databases, search, statusFilter, sortKey]);

  // P0-3 只读说明：Create database 按钮因平台管理员身份隐藏时，落一张
  // 说明卡（三个返回形态共享——pending/error/ready 的页头都在）。
  const platformReadonlyNote = !canAdminResources && isPlatformAdmin ? (
    <Card className="border-dashed">
      <CardContent
        className="p-4 text-sm text-muted-foreground"
        data-testid="platform-readonly-note"
      >
        Platform administrators have read-only access to resources (separation
        of duties). Create and manage database instances from the CLI with a
        machine token, or ask a team owner for a member role.
      </CardContent>
    </Card>
  ) : null;

  const header = (
    <PageHeader
      title="Databases"
      description="Managed database instances: platform-run engines with credentials, backups and lifecycle — a separate resource type from apps."
      actions={
        <>
          <Button
            variant="outline"
            size="sm"
            onClick={() => void query.refetch()}
            aria-label="Refresh databases"
          >
            Refresh
          </Button>
          {canAdminResources ? (
            <Button size="sm" data-testid="database-create-button" onClick={() => setCreateOpen(true)}>
              <Plus aria-hidden className="h-3.5 w-3.5" />
              Create database
            </Button>
          ) : null}
        </>
      }
    />
  );

  if (query.isPending) {
    return (
      <div className="space-y-4" data-testid="databases-page">
        {header}
        {platformReadonlyNote}
        <p className="text-sm text-muted-foreground">Loading databases…</p>
      </div>
    );
  }
  if (query.isError) {
    const envelope = errorEnvelopeFrom(query.error);
    return (
      <div className="space-y-4" data-testid="databases-page">
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
    <div className="space-y-4" data-testid="databases-page">
      {header}
      {platformReadonlyNote}
      <div className="grid grid-cols-2 gap-4 md:grid-cols-4">
        <StatCard label="Instances" value={databases.length} />
        <StatCard label="Ready" value={counts.ready ?? 0} />
        <StatCard label="Converging" value={(counts.provisioning ?? 0) + (counts.paused ?? 0)} sub="provisioning + paused" />
        <StatCard label="Attention" value={(counts.failed ?? 0) + (counts.degraded ?? 0)} sub="failed + degraded" />
      </div>
      <Card>
        <div className="flex flex-wrap items-center gap-2 border-b px-4 py-3">
          <div className="relative">
            <Search
              aria-hidden
              className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground"
            />
            <Input
              aria-label="Filter databases"
              placeholder="Filter databases…"
              className="w-56 pl-8"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
            />
          </div>
          <Select value={statusFilter} onValueChange={(v) => setStatusFilter(v as DbStateFilter)}>
            <SelectTrigger className="w-36" aria-label="Filter by state">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {DB_STATE_FILTERS.map((f) => (
                <SelectItem key={f.key} value={f.key}>
                  {f.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Select value={sortKey} onValueChange={(v) => setSortKey(v as DbSortKey)}>
            <SelectTrigger className="ml-auto w-44" aria-label="Sort databases">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="updated">Recently updated</SelectItem>
              <SelectItem value="name">Name</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <CardContent className="p-0">
          {databases.length === 0 ? (
            <EmptyState
              icon={Database}
              title="No database instances yet."
              hint="Create one with the button above — or 'fleetly databases create <name> --template postgres-16'. Referencing apps declare the instance with the fleetly.databases compose label."
            />
          ) : visible.length === 0 ? (
            <EmptyState
              icon={Search}
              title="No database instances match the current filters."
              hint="Adjust the search or filter selections to widen the view."
            />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Instance</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Placement</TableHead>
                  <TableHead>Volume</TableHead>
                  <TableHead>Last backup</TableHead>
                  <TableHead>Upgrade</TableHead>
                  <TableHead>Memory limit</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {visible.map((db) => (
                  <DatabaseRow key={db.id} db={db} resolveSubject={resolveSubject} />
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
      {/* 抽出的可复用对话框：目标项目 = 顶栏选中项目（对话框内明示）——
          与抽取前行为一致（projectRef 缺省回落服务端缺省）。 */}
      <CreateDatabaseDialog open={createOpen} onOpenChange={setCreateOpen} projectRef={projectRef} />
    </div>
  );
}
