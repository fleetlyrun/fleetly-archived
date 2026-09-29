// 应用详情壳：标题行（名称 + 派生状态 + 生命周期）+ 分段式子导航
// （概览/部署/日志/env/域名）+ 详情数据加载。深链形如
// /ui/apps/<ref>/deployments——<ref> 支持平台 id（列表行导航口径，同名
// app 裸名必歧义，2026-09-25 走查裁决）与裸名（唯一名直连）；daemon 的
// SPA 回退直接可达。

import { useQuery } from "@tanstack/react-query";
import { Boxes, Loader2 } from "lucide-react";
import { Suspense } from "react";
import { Outlet, useLocation, useNavigate, useParams } from "react-router-dom";

import { getApp } from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import { EnvelopeAlert } from "@/components/envelope-alert";
import { PillTabs } from "@/components/pill-tabs";
import { StateBadge } from "@/components/state-badge";
import { timeAgo } from "@/lib/utils";

// 页签序（2026-09-29 IA 重排，设计 docs/design/2026-09-29-console-ia-
// redesign.md §4.1）：Overview → Containers（运行真相第二优先，对齐
// dokploy）→ Deployments/Builds（变更史）→ Logs → Env/Secrets/Configs
//（配置三族相邻）→ Domains → Terminal（工具位殿后）。
const TABS = [
  { key: "", label: "Overview" },
  { key: "containers", label: "Containers" },
  { key: "deployments", label: "Deployments" },
  { key: "builds", label: "Builds" },
  { key: "logs", label: "Logs" },
  { key: "env", label: "Env" },
  { key: "secrets", label: "Secrets" },
  { key: "configs", label: "Configs" },
  { key: "domains", label: "Domains" },
  { key: "terminal", label: "Terminal" },
];

export function AppDetailLayout() {
  const { name = "" } = useParams();
  const navigate = useNavigate();
  const location = useLocation();
  const query = useQuery({
    queryKey: ["app", name],
    queryFn: () => getApp(name),
    refetchInterval: 5000,
  });

  const base = `/apps/${encodeURIComponent(name)}`;
  const current =
    TABS.find((t) => t.key !== "" && location.pathname.startsWith(`${base}/${t.key}`))?.key ?? "";

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

  const app = query.data;
  // 标题显示业务名：ref 为 id 寻址时从详情响应反解（加载中暂显 ref）。
  const displayName = app?.name ?? name;
  const lifecycle = app?.lifecycle ?? "active";

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-3">
        <span className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg border bg-muted/40">
          <Boxes aria-hidden className="h-5 w-5 text-muted-foreground" />
        </span>
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2.5">
            <h1 className="truncate font-mono text-xl font-semibold tracking-tight">{displayName}</h1>
            {app ? <StateBadge state={app.derived_state ?? ""} /> : null}
            {app && lifecycle !== "active" ? (
              <span className="rounded-md border px-1.5 py-0.5 text-xs text-muted-foreground">
                {lifecycle}
              </span>
            ) : null}
          </div>
          {app ? (
            <p className="text-xs text-muted-foreground">
              updated {timeAgo(app.updated_at)} · created {timeAgo(app.created_at)}
            </p>
          ) : null}
        </div>
      </div>

      <PillTabs
        ariaLabel="App sections"
        value={current}
        onValueChange={(key) => navigate(key === "" ? base : `${base}/${key}`)}
        items={TABS}
      />

      {/* 子 tab（概览/日志/终端等）同属路由级 lazy——内层边界让 chunk 取回
          期间详情头/页签保持可见（外层 Layout 边界只兜底详情壳本身）。 */}
      <Suspense
        fallback={
          <div className="flex h-48 items-center justify-center" data-testid="route-splash">
            <Loader2 aria-hidden className="h-5 w-5 animate-spin text-muted-foreground" />
          </div>
        }
      >
        <Outlet />
      </Suspense>
    </div>
  );
}
