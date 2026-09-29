// 应用详情标题栏动作行（2026-09-29 三轮 §5，app Stop/Start/Redeploy）：
// UI/UX 与 DatabaseDetailPage 标题栏同款——右对齐 outline 小按钮排 + 按状
// 态相宜启停 + title 提示，即发即走（mutation → invalidate，状态投影随 5s
// 轮询刷新——与 DB 标题栏一致，无内联结果行）。
//
// 动作与角色门（前端体验门，§3.2；服务端 scope 硬门不变）：
//   Redeploy = deploy scope（developer+）——重放当前生效版本（空 target
//              rollback = 保留窗最新成功版本，即现行 active revision）；
//   Stop     = admin scope（与 SuspendDatabase 同级——整应用停摆是大爆炸
//              半径动作）；挂起 = 引擎排水副本到 0，路由仍解析但引用方
//              连不上（DB paused 同款诚实暴露）；
//   Start    = admin scope——清位 + active revision 重部署恢复。
// 挂起期 Redeploy 禁用（服务端 E_APP_SUSPENDED 同门）并 title 指路 Start。
// 平台管理员能力门统一关门 → 三钮全隐（P0-3 双门；说明态在 Overview 的
// Deploy settings 卡）。

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Pause, Play, RotateCw } from "lucide-react";
import { useState } from "react";

import { rollbackDeployment, resumeApp, suspendApp } from "@/api/endpoints";
import type { AppView } from "@/api/types";
import { errorEnvelopeFrom } from "@/api/errors";
import { EnvelopeAlert } from "@/components/envelope-alert";
import { Button } from "@/components/ui/button";
import { useTeamCapabilities } from "@/lib/context";

export function AppHeaderActions({ app }: { app: AppView }) {
  const queryClient = useQueryClient();
  const name = app.name ?? "";
  const { canDeploy, canAdminResources } = useTeamCapabilities();
  const [error, setError] = useState<ReturnType<typeof errorEnvelopeFrom> | null>(null);

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ["app", name] });
    void queryClient.invalidateQueries({ queryKey: ["apps"] });
    void queryClient.invalidateQueries({ queryKey: ["runtime", name] });
  };
  const suspendMutation = useMutation({
    mutationFn: () => suspendApp(name),
    onSuccess: () => {
      setError(null);
      invalidate();
    },
    onError: (err) => setError(errorEnvelopeFrom(err)),
  });
  const resumeMutation = useMutation({
    mutationFn: () => resumeApp(name),
    onSuccess: () => {
      setError(null);
      invalidate();
    },
    onError: (err) => setError(errorEnvelopeFrom(err)),
  });
  const redeployMutation = useMutation({
    mutationFn: () => rollbackDeployment(name),
    onSuccess: () => {
      setError(null);
      invalidate();
      void queryClient.invalidateQueries({ queryKey: ["deployments", name] });
    },
    onError: (err) => setError(errorEnvelopeFrom(err)),
  });

  const suspended = app.suspended === true;
  const pending = suspendMutation.isPending || resumeMutation.isPending || redeployMutation.isPending;

  return (
    <div className="ml-auto flex flex-wrap items-center gap-2" data-testid="app-header-actions">
      {canDeploy ? (
        <Button
          variant="outline"
          size="sm"
          data-testid="app-redeploy-button"
          disabled={suspended || pending}
          title={
            suspended
              ? "Start the app first — deploys are refused while suspended"
              : "Replay the currently deployed revision as a new deployment (goes through the normal release pipeline)"
          }
          onClick={() => redeployMutation.mutate()}
        >
          <RotateCw aria-hidden className="h-3.5 w-3.5" />
          Redeploy
        </Button>
      ) : null}
      {canAdminResources && !suspended ? (
        <Button
          variant="outline"
          size="sm"
          data-testid="app-stop-button"
          disabled={pending}
          title="Stop the app: managed services drain to zero replicas (service objects kept; routes keep resolving and fail honestly — same semantics as paused databases)"
          onClick={() => suspendMutation.mutate()}
        >
          <Pause aria-hidden className="h-3.5 w-3.5" />
          Stop
        </Button>
      ) : null}
      {canAdminResources && suspended ? (
        <Button
          variant="outline"
          size="sm"
          data-testid="app-start-button"
          disabled={pending}
          title="Start the app: clears the suspend bit and redeploys the active revision to bring the services back"
          onClick={() => resumeMutation.mutate()}
        >
          <Play aria-hidden className="h-3.5 w-3.5" />
          Start
        </Button>
      ) : null}
      {error ? (
        <EnvelopeAlert
          code={error.code}
          message={error.message}
          suggestion={error.suggestion}
          docs={error.docs}
          className="basis-full"
        />
      ) : null}
    </div>
  );
}
