// 面包屑：从 location 派生（Home / Applications / <app> / <tab>）。
// 已知路由段映射为业务名；未知段按业务解析：
//   - 团队 id 段 → 团队名（Me 投影）；
//   - 应用 id 段（详情导航 2026-09-25 起 id 寻址）→ 应用名：响应式观察
//     ["apps", ref] 列表缓存与 ["app", id] 详情缓存（后者与详情壳共用同
//     键查询——深链直入也随后到数据补出真名，getQueryData 非响应式读不到
//     异步填充，2026-09-25 复验实爆）。id 段可处于路径任意位置（详情壳
//     子页签 /apps/:id/logs 的 id 非末段——详情壳自身持有同键查询，观察
//     者挂上即可命中缓存）。数据落地前显示骨架占位「—」，绝不裸显 ID、
//     也不再渲染字面 "Application" 占位（2026-09-25 审查 P2-9）。
// 末段为当前页（纯文本），前段可点。

import { useQuery } from "@tanstack/react-query";
import { Fragment } from "react";
import { Link, useLocation } from "react-router-dom";
import { ChevronRight } from "lucide-react";

import { getApp, getProject, listApps, listProjects } from "@/api/endpoints";
import { useProjectContext } from "@/lib/context";

const SEGMENT_LABELS: Record<string, string> = {
  apps: "Applications",
  databases: "Databases",
  system: "System",
  events: "Events",
  audit: "Audit",
  teams: "Teams",
  projects: "Projects",
  deployments: "Deployments",
  builds: "Builds",
  logs: "Logs",
  env: "Env",
  secrets: "Secrets",
  configs: "Configs",
  domains: "Domains",
  terminal: "Terminal",
  "git-keys": "Git push keys",
  // 一级页存量缺失段（2026-09-25 复核）：/pat（PAT 自服务页）、
  // /admin/users（Administration 组入口——/admin/audit 的 "audit" 段已被
  // 上面 audit 键覆盖，"admin" 前缀段保持原文渲染）。
  pat: "API tokens",
  users: "Users",
};

/** 26 字符规范 ULID（平台 ID 段的形态判据）。 */
const ULID_RE = /^[0-9A-HJKMNP-TV-Z]{26}$/;

export function Breadcrumbs() {
  const location = useLocation();
  const { teams, projectRef } = useProjectContext();
  const segments = location.pathname.split("/").filter(Boolean);

  // 应用 id 段至多一个、可处于路径任意位置（详情壳子页签的 id 非末段）；
  // 详情观察者按「路径中存在 app id 段」挂载（enabled=false 只读缓存不发
  // 请求——列表先行场景零开销）。项目 id 段仅末段（详情页本体）。
  const lastSeg = segments[segments.length - 1] ?? "";
  const appSeg =
    segments.find((seg, i) => ULID_RE.test(seg) && segments[i - 1] === "apps") ?? "";
  const lastIsProjectId = ULID_RE.test(lastSeg) && segments[segments.length - 2] === "projects";
  const appsList = useQuery({
    queryKey: ["apps", projectRef],
    queryFn: () => listApps(projectRef ? { project: projectRef } : {}),
    enabled: false,
    staleTime: 5 * 60 * 1000,
  });
  const appDetail = useQuery({
    queryKey: ["app", appSeg],
    queryFn: () => getApp(appSeg),
    enabled: appSeg !== "",
    staleTime: 30 * 1000,
    retry: false,
  });
  // 项目 id 段：["projects","context"] 列表缓存（切换器/一级页先行必命中）
  // + ["project", id] 详情缓存（详情页查询键同源）。
  const projectsList = useQuery({
    queryKey: ["projects", "context"],
    queryFn: () => listProjects(),
    enabled: false,
    staleTime: 60 * 1000,
  });
  const projectDetail = useQuery({
    queryKey: ["project", lastSeg],
    queryFn: () => getProject(lastSeg),
    enabled: lastIsProjectId,
    staleTime: 30 * 1000,
    retry: false,
  });

  const labelFor = (seg: string, prev?: string): string => {
    if (SEGMENT_LABELS[seg]) return SEGMENT_LABELS[seg];
    if (ULID_RE.test(seg)) {
      const team = teams.find((t) => t.team_id === seg);
      if (team) return team.team_name || team.team_slug || "Team";
      if (prev === "apps") {
        // 反解序：列表缓存行名 → 详情缓存名；落地前骨架占位（不裸显
        // ID、不渲染字面 "Application"——2026-09-25 审查 P2-9）。
        const fromList = appsList.data?.apps?.find((a) => a.id === seg);
        if (fromList?.name) return fromList.name;
        if (seg === appSeg) return appDetail.data?.name ?? "—";
        return "—";
      }
      if (prev === "projects") {
        if (seg === lastSeg) {
          const fromList = projectsList.data?.projects?.find((p) => p.id === seg);
          if (fromList?.name) return fromList.name;
          return projectDetail.data?.project?.name ?? "Project";
        }
        return "Project";
      }
      return "Team settings";
    }
    return decodeURIComponent(seg);
  };

  return (
    <nav aria-label="Breadcrumb" className="flex min-w-0 items-center gap-1 text-sm">
      <Link
        to="/"
        className="shrink-0 text-muted-foreground transition-colors hover:text-foreground"
      >
        Home
      </Link>
      {segments.map((seg, i) => {
        const href = `/${segments.slice(0, i + 1).join("/")}`;
        const label = labelFor(seg, segments[i - 1]);
        const last = i === segments.length - 1;
        return (
          <Fragment key={href}>
            <ChevronRight aria-hidden className="h-3.5 w-3.5 shrink-0 text-muted-foreground/60" />
            {last ? (
              <span aria-current="page" className="truncate font-medium">
                {label}
              </span>
            ) : (
              <Link
                to={href}
                className="truncate text-muted-foreground transition-colors hover:text-foreground"
              >
                {label}
              </Link>
            )}
          </Fragment>
        );
      })}
    </nav>
  );
}
