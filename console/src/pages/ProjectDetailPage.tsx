// 项目详情页（/projects/:projectId，2026-09-25 用户裁决「项目怎么查看或
// 编辑详情」——此前 Projects 一级页只聚合+创建，项目信息无查看/编辑面）：
//   - Info 卡：slug（不可变）/created/描述；name+description 编辑（服务端
//     UpdateProject = 团队 owner 硬门，前端按成员角色渲染编辑钮——个人队
//     owner 天然可编辑；平台管理员非成员只读视角）；
//   - Applications 卡：项目内应用清单（listApps ?project=team/prj 收窄，
//     行链接 id 寻址进详情——同名应用安全）+ 创建 CTA（Deploy new
//     application 对话框——Deploy upsert 语义随首署建应用，2026-09-25 审查
//     P0-1「全站没有创建应用入口」收口）；
//   - Databases 卡：listDatabases ?project= 收窄（同 id 寻址）+ 创建 CTA
//     （抽出的 CreateDatabaseDialog，目标项目 = 本页项目限定形）；
//   - 成员覆写管理在团队设置 Projects tab（单一管理面）——卡片底部链接。
// 面包屑经 ["project", id] 详情缓存反解项目名。

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Boxes,
  Database,
  Loader2,
  Pencil,
  Plus,
  Rocket,
  Save,
} from "lucide-react";
import { useState, type FormEvent } from "react";
import { Link, useParams } from "react-router-dom";

import { getProject, listApps, listDatabases, updateProject } from "@/api/endpoints";
import { errorEnvelopeFrom, type ErrorEnvelope } from "@/api/errors";
import { CreateAppDialog } from "@/components/create-app-dialog";
import { CreateDatabaseDialog } from "@/components/create-database-dialog";
import { EmptyState } from "@/components/empty-state";
import { EnvelopeAlert } from "@/components/envelope-alert";
import { PageHeader } from "@/components/page-header";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { roleAtLeast, useIsPlatformAdmin, useProjectContext } from "@/lib/context";
import { formatTime, timeAgo } from "@/lib/utils";

export function ProjectDetailPage() {
  const { projectId = "" } = useParams();
  const { teams, projectOverrides } = useProjectContext();
  const isPlatformAdmin = useIsPlatformAdmin();
  const queryClient = useQueryClient();
  const [error, setError] = useState<ErrorEnvelope | null>(null);
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  // 创建 CTA 的对话框开关（Applications / Databases 各一）。
  const [createAppOpen, setCreateAppOpen] = useState(false);
  const [createDbOpen, setCreateDbOpen] = useState(false);

  const projectQuery = useQuery({
    queryKey: ["project", projectId],
    queryFn: () => getProject(projectId),
  });
  const project = projectQuery.data?.project;

  const updateMutation = useMutation({
    mutationFn: () => updateProject(projectId, { name: name.trim(), description }),
    onSuccess: () => {
      setEditing(false);
      setError(null);
      void queryClient.invalidateQueries({ queryKey: ["project", projectId] });
      void queryClient.invalidateQueries({ queryKey: ["projects"] });
    },
    onError: (err) => setError(errorEnvelopeFrom(err)),
  });

  function onSave(e: FormEvent) {
    e.preventDefault();
    if (name.trim()) updateMutation.mutate();
  }

  if (projectQuery.isPending) {
    return <p className="text-sm text-muted-foreground">Loading project…</p>;
  }
  if (projectQuery.isError || !project) {
    const envelope = errorEnvelopeFrom(projectQuery.error);
    return (
      <EnvelopeAlert
        code={envelope.code}
        message={envelope.message}
        suggestion={envelope.suggestion}
        docs={envelope.docs}
      />
    );
  }

  // 我的团队角色（Me 投影）：owner 才见编辑钮（服务端 UpdateProject 硬门
  // 同规）；非成员（平台管理员跨队视角）只读。
  const membership = teams.find((t) => t.team_id === project.team_id);
  const myRole = membership?.role ?? null;
  const canEdit = myRole === "owner";
  // 资源创建门（前端体验门，§3.2 矩阵 + P0-3 双门）：按**本页项目**的归属
  // 团队解析（顶栏选中上下文无关——项目详情页面向任意可见项目）。覆写行
  // 优先（W3-S3 同判定式：team_id + prj_slug 命中即用覆写角色）。平台管理
  // 员资源面恒只读（服务端 ownership.go 硬拒）——CTA 不渲染，改落说明卡。
  const override = projectOverrides.find(
    (o) => o.team_id === project.team_id && o.prj_slug === project.slug,
  );
  const resourceRole = override?.role ?? myRole;
  const canDeploy = !isPlatformAdmin && roleAtLeast(resourceRole, "developer");
  const canAdminResources = !isPlatformAdmin && roleAtLeast(resourceRole, "admin");
  const qualified = `${project.team_slug}/${project.slug}`;

  return (
    <div className="space-y-4" data-testid="project-detail-page">
      <PageHeader
        title={project.name ?? project.slug ?? "Project"}
        description={
          <>
            <span className="font-mono">{qualified}</span>
            {membership ? (
              <>
                {" · "}
                <Link
                  to={`/teams/${encodeURIComponent(membership.team_id ?? "")}?tab=members`}
                  className="hover:underline"
                >
                  team settings
                </Link>
                {" · "}
                {/* 角色覆写管理面在团队设置的 Projects tab（单一管理面）——
                    落点带 tab（2026-09-25 审查 P2-2：两个链接此前同指一个
                    URL，覆写入口永远落在 Members tab）；文案直说落点语义
                    （2026-09-25 走查：'members & role overrides' 含糊）。 */}
                <Link
                  to={`/teams/${encodeURIComponent(membership.team_id ?? "")}?tab=projects`}
                  className="hover:underline"
                >
                  project role overrides
                </Link>
              </>
            ) : (
              " · read-only (no team membership)"
            )}
          </>
        }
      />

      <Card data-testid="project-info-card">
        <CardHeader className="border-b pb-3">
          <CardTitle className="text-sm font-semibold">Project details</CardTitle>
          <CardDescription className="mt-1">
            Slug is immutable and the middle segment of the infrastructure naming formula.
            {canEdit
              ? " Name and description are editable (team owner)."
              : " Only the team owner can edit these fields."}
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-3 pt-4">
          {error ? (
            <EnvelopeAlert code={error.code} message={error.message} suggestion={error.suggestion} />
          ) : null}
          {editing ? (
            <form className="flex flex-wrap items-end gap-2" onSubmit={onSave}>
              <div className="space-y-1.5">
                <Label htmlFor="project-edit-name">Name</Label>
                <Input
                  id="project-edit-name"
                  data-testid="project-name-input"
                  className="w-56"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="project-edit-description">Description</Label>
                <Input
                  id="project-edit-description"
                  data-testid="project-description-input"
                  className="w-72"
                  value={description}
                  onChange={(e) => setDescription(e.target.value)}
                />
              </div>
              <Button
                type="submit"
                size="sm"
                data-testid="project-save"
                disabled={!name.trim() || updateMutation.isPending}
              >
                {updateMutation.isPending ? (
                  <Loader2 aria-hidden className="h-3.5 w-3.5 animate-spin" />
                ) : (
                  <Save aria-hidden className="h-3.5 w-3.5" />
                )}
                Save
              </Button>
              <Button
                type="button"
                variant="outline"
                size="sm"
                data-testid="project-edit-cancel"
                onClick={() => setEditing(false)}
              >
                Cancel
              </Button>
            </form>
          ) : (
            <div className="grid gap-x-8 gap-y-2 text-sm sm:grid-cols-[10rem_1fr]">
              <span className="text-muted-foreground">Slug</span>
              <span className="font-mono text-xs">{project.slug}</span>
              <span className="text-muted-foreground">Name</span>
              <span data-testid="project-detail-name">{project.name}</span>
              <span className="text-muted-foreground">Description</span>
              <span data-testid="project-detail-description" className="text-muted-foreground">
                {project.description || "—"}
              </span>
              <span className="text-muted-foreground">Project network</span>
              {/* 项目网面（IMPL-T15-1/OT-1）：overlay 名 + 参与成员数——对象
                  由平台在首个成员 attach 时创建，成员清空且零端点后回收。 */}
              <span data-testid="project-network-summary" className="text-muted-foreground">
                <code className="text-xs" data-testid="project-network-name">
                  {project.network_name}
                </code>{" "}
                · {project.network_members ?? 0} app
                {(project.network_members ?? 0) === 1 ? "" : "s"} attached
              </span>
              <span className="text-muted-foreground">Created</span>
              <span className="text-xs text-muted-foreground">{formatTime(project.created_at)}</span>
            </div>
          )}
          {canEdit && !editing ? (
            <Button
              variant="outline"
              size="sm"
              data-testid="project-edit-open"
              onClick={() => {
                setName(project.name ?? "");
                setDescription(project.description ?? "");
                setEditing(true);
              }}
            >
              <Pencil aria-hidden className="h-3.5 w-3.5" />
              Edit details
            </Button>
          ) : null}
        </CardContent>
      </Card>

      {/* P0-3 平台管理员说明态：资源创建 CTA 因双门（资源面恒只读）隐藏
          时，以说明卡明示原因与可行动路径，不做静默消失。 */}
      {isPlatformAdmin ? (
        <Card className="border-dashed">
          <CardContent
            className="p-4 text-sm text-muted-foreground"
            data-testid="platform-readonly-note"
          >
            Platform administrators have read-only access to resources
            (separation of duties). Deploy applications and create databases
            from the CLI with a machine token, or ask a team owner for a member
            role.
          </CardContent>
        </Card>
      ) : null}

      <ProjectApps
        teamSlug={project.team_slug ?? ""}
        slug={project.slug ?? ""}
        canDeploy={canDeploy}
        onCreate={() => setCreateAppOpen(true)}
      />
      <ProjectDatabases
        teamSlug={project.team_slug ?? ""}
        slug={project.slug ?? ""}
        canAdminResources={canAdminResources}
        onCreate={() => setCreateDbOpen(true)}
      />

      {canDeploy ? (
        <CreateAppDialog
          open={createAppOpen}
          onOpenChange={setCreateAppOpen}
          projectRef={qualified}
        />
      ) : null}
      {canAdminResources ? (
        <CreateDatabaseDialog
          open={createDbOpen}
          onOpenChange={setCreateDbOpen}
          projectRef={qualified}
        />
      ) : null}
    </div>
  );
}

/** 项目内应用清单（服务端 ?project= 收窄；行 id 寻址进详情）。创建 CTA：
 *  developer+（Deploy new application 对话框——deploy upsert 随首署建行）。 */
function ProjectApps({
  teamSlug,
  slug,
  canDeploy,
  onCreate,
}: {
  teamSlug: string;
  slug: string;
  canDeploy: boolean;
  onCreate: () => void;
}) {
  const query = useQuery({
    queryKey: ["apps", `${teamSlug}/${slug}`],
    queryFn: () => listApps({ project: `${teamSlug}/${slug}` }),
  });
  const apps = query.data?.apps ?? [];

  return (
    <Card data-testid="project-apps-card">
      <CardHeader className="flex-row items-start justify-between space-y-0 border-b pb-3">
        <div className="space-y-1">
          <CardTitle className="flex items-center gap-2 text-sm font-semibold">
            <Boxes aria-hidden className="h-4 w-4 text-muted-foreground" />
            Applications
          </CardTitle>
          <CardDescription className="mt-1">Apps deployed in this project.</CardDescription>
        </div>
        {canDeploy ? (
          <Button size="sm" data-testid="project-app-create-button" onClick={onCreate}>
            <Rocket aria-hidden className="h-3.5 w-3.5" />
            Deploy application
          </Button>
        ) : null}
      </CardHeader>
      <CardContent className="p-0">
        {apps.length === 0 ? (
          <EmptyState
            icon={Boxes}
            title="No applications in this project yet."
            hint="Deploy a compose file to create the first app in this project — the app is created together with its first deployment."
          >
            {canDeploy ? (
              <Button
                size="sm"
                className="mt-2"
                data-testid="project-app-empty-cta"
                onClick={onCreate}
              >
                <Rocket aria-hidden className="h-3.5 w-3.5" />
                Deploy your first application
              </Button>
            ) : null}
          </EmptyState>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Application</TableHead>
                <TableHead>State</TableHead>
                {/* 项目网参与列（IMPL-T15-1/OT-1）：attach/detach 在 app 概览卡
                    （单一操作面）；本列只读呈现参与状态。 */}
                <TableHead>Project network</TableHead>
                <TableHead>Updated</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {apps.map((a) => (
                <TableRow key={a.id} data-testid="project-app-row">
                  <TableCell>
                    <Link
                      to={`/apps/${encodeURIComponent(a.id ?? "")}`}
                      className="font-medium hover:underline"
                    >
                      {a.name}
                    </Link>
                  </TableCell>
                  <TableCell>
                    <Badge variant="secondary">{a.derived_state}</Badge>
                  </TableCell>
                  <TableCell>
                    <Badge
                      variant={a.project_network_attached ? "secondary" : "outline"}
                      data-testid="project-app-network-badge"
                    >
                      {a.project_network_attached ? "attached" : "detached"}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">{timeAgo(a.updated_at)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}

/** 项目内数据库清单（服务端 ?project= 收窄；行 id 寻址——同名库安全）。
 *  创建 CTA：admin+（复用抽出的 CreateDatabaseDialog，目标 = 本页项目）。 */
function ProjectDatabases({
  teamSlug,
  slug,
  canAdminResources,
  onCreate,
}: {
  teamSlug: string;
  slug: string;
  canAdminResources: boolean;
  onCreate: () => void;
}) {
  const query = useQuery({
    queryKey: ["databases", `${teamSlug}/${slug}`],
    queryFn: () => listDatabases({ project: `${teamSlug}/${slug}` }),
  });
  const dbs = query.data?.databases ?? [];

  return (
    <Card data-testid="project-databases-card">
      <CardHeader className="flex-row items-start justify-between space-y-0 border-b pb-3">
        <div className="space-y-1">
          <CardTitle className="flex items-center gap-2 text-sm font-semibold">
            <Database aria-hidden className="h-4 w-4 text-muted-foreground" />
            Databases
          </CardTitle>
          <CardDescription className="mt-1">Managed databases in this project.</CardDescription>
        </div>
        {canAdminResources ? (
          <Button size="sm" data-testid="project-db-create-button" onClick={onCreate}>
            <Plus aria-hidden className="h-3.5 w-3.5" />
            Create database
          </Button>
        ) : null}
      </CardHeader>
      <CardContent className="p-0">
        {dbs.length === 0 ? (
          <EmptyState
            icon={Database}
            title="No databases in this project yet."
            hint="Create a managed instance in this project — referencing apps consume it via the FLEETLY_DB_* variables."
          >
            {canAdminResources ? (
              <Button
                size="sm"
                className="mt-2"
                data-testid="project-db-empty-cta"
                onClick={onCreate}
              >
                <Plus aria-hidden className="h-3.5 w-3.5" />
                Create a database
              </Button>
            ) : null}
          </EmptyState>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Instance</TableHead>
                <TableHead>Engine</TableHead>
                <TableHead>Status</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {dbs.map((d) => (
                <TableRow key={d.id} data-testid="project-db-row">
                  <TableCell>
                    <Link
                      to={`/databases/${encodeURIComponent(d.id ?? "")}`}
                      className="font-medium hover:underline"
                    >
                      {d.name}
                    </Link>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">{d.template}</TableCell>
                  <TableCell>
                    <Badge variant="secondary">{d.status}</Badge>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}
