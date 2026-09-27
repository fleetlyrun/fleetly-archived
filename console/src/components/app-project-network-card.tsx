// 项目网卡（T 线 OT-1 / IMPL-T15-1）：app 归属项目 + 项目网参与状态 +
// attach/detach 控件。
//
// 语义（服务端为真值，文案如实转述）：
//   - 缺省不参加（OT-1：app 私网隔离现状零变化）；attach 后成员服务双挂
//     app 私网 + 项目网（项目网别名 <app>-<service>，短名只在 app 私网）；
//   - attach/detach 通过「参与变更重部署」生效——成员服务在下一次发布中
//     滚动切换网络（Console 诚实标注，不承诺零中断）；
//   - 角色门：admin scope + 项目角色 admin（团队 owner/admin）；平台管理员
//     只读不代写（说明行原位渲染，不静默消失）；viewer/developer 只读。
//
// 数据源 = getApp 投影（project_id / project_network_attached /
// project_network——父组件查询传入，本卡不重复拉取）。

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Network, Unplug } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";

import { attachAppProjectNetwork, detachAppProjectNetwork } from "@/api/endpoints";
import { errorEnvelopeFrom, type ErrorEnvelope } from "@/api/errors";
import type { GetAppResponse } from "@/api/types";
import { EnvelopeAlert } from "@/components/envelope-alert";
import { Badge } from "@/components/ui/badge";
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
import { useIsPlatformAdmin, useTeamCapabilities } from "@/lib/context";

function Field({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-4 text-sm">
      <span className="text-muted-foreground">{label}</span>
      <span className="text-right font-medium">{value}</span>
    </div>
  );
}

export function AppProjectNetworkCard({ app }: { app: GetAppResponse }) {
  const queryClient = useQueryClient();
  const { canAdminResources } = useTeamCapabilities();
  const isPlatformAdmin = useIsPlatformAdmin();
  const [error, setError] = useState<ErrorEnvelope | null>(null);
  const [detachOpen, setDetachOpen] = useState(false);

  const appName = app.name ?? "";
  const appID = app.id ?? "";
  const attached = app.project_network_attached === true;
  const projectRef = `${app.team_slug ?? ""}/${app.project_slug ?? ""}`;

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ["app", appName] });
    void queryClient.invalidateQueries({ queryKey: ["apps"] });
    void queryClient.invalidateQueries({ queryKey: ["project", app.project_id] });
    void queryClient.invalidateQueries({ queryKey: ["projects"] });
  };
  const attach = useMutation({
    mutationFn: () => attachAppProjectNetwork(appID),
    onSuccess: () => {
      setError(null);
      invalidate();
    },
    onError: (err) => setError(errorEnvelopeFrom(err)),
  });
  const detach = useMutation({
    mutationFn: () => detachAppProjectNetwork(appID),
    onSuccess: () => {
      setError(null);
      setDetachOpen(false);
      invalidate();
    },
    onError: (err) => setError(errorEnvelopeFrom(err)),
  });

  return (
    <Card data-testid="app-project-network-card">
      <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
        <Network aria-hidden className="h-4 w-4 text-muted-foreground" />
        <CardTitle className="text-sm font-semibold">Project network</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3 pt-4">
        <Field
          label="Project"
          value={
            app.project_id ? (
              <Link
                to={`/projects/${encodeURIComponent(app.project_id)}`}
                className="font-mono text-xs hover:underline"
                data-testid="project-network-project-link"
              >
                {projectRef}
              </Link>
            ) : (
              <span className="text-xs text-muted-foreground">—</span>
            )
          }
        />
        <Field
          label="Participation"
          value={
            <Badge
              variant={attached ? "secondary" : "outline"}
              data-testid="project-network-badge"
            >
              {attached ? "attached" : "detached"}
            </Badge>
          }
        />
        <Field
          label="Network"
          value={
            attached && app.project_network ? (
              <code className="text-xs" data-testid="project-network-name">
                {app.project_network}
              </code>
            ) : (
              <span className="text-xs text-muted-foreground">—</span>
            )
          }
        />
        <p className="text-xs text-muted-foreground">
          Off by default (apps stay isolated in their own private network). While
          attached, every member service is joined to the project overlay in
          addition to the app network, reachable by its peers as{" "}
          <code>&lt;app&gt;-&lt;service&gt;</code> (short names stay app-private).
          Attaching and detaching roll member services on the following
          redeploy.
        </p>
        {error ? (
          <EnvelopeAlert code={error.code} message={error.message} suggestion={error.suggestion} />
        ) : null}
        {isPlatformAdmin ? (
          <p className="text-xs text-muted-foreground" data-testid="project-network-readonly-note">
            Platform administrators have read-only access to resources
            (separation of duties). Change project-network participation from
            the CLI with a machine token, or ask a team owner for a member role.
          </p>
        ) : canAdminResources ? (
          attached ? (
            <Button
              variant="outline"
              size="sm"
              data-testid="project-network-detach"
              disabled={detach.isPending}
              onClick={() => {
                setError(null);
                setDetachOpen(true);
              }}
            >
              <Unplug aria-hidden className="h-3.5 w-3.5" />
              Detach from project network
            </Button>
          ) : (
            <Button
              size="sm"
              data-testid="project-network-attach"
              disabled={attach.isPending}
              onClick={() => attach.mutate()}
            >
              <Network aria-hidden className="h-3.5 w-3.5" />
              Attach to project network
            </Button>
          )
        ) : (
          <p className="text-xs text-muted-foreground" data-testid="project-network-role-note">
            Changing project-network participation requires the team admin or
            owner role.
          </p>
        )}

        {detachOpen ? (
          <Dialog open onOpenChange={(v) => (v ? undefined : setDetachOpen(false))}>
            <DialogContent data-testid="project-network-detach-dialog">
              <DialogHeader>
                <DialogTitle>Detach {appName} from the project network?</DialogTitle>
                <DialogDescription>
                  Member services leave the shared project overlay on the
                  following redeploy (a rolling update). Peers in this project
                  lose network reachability by {`<app>-<service>`} until you
                  attach again. The app keeps its own private network
                  throughout.
                </DialogDescription>
              </DialogHeader>
              <DialogFooter>
                <Button
                  variant="outline"
                  size="sm"
                  data-testid="project-network-detach-cancel"
                  onClick={() => setDetachOpen(false)}
                >
                  Cancel
                </Button>
                <Button
                  variant="destructive"
                  size="sm"
                  data-testid="project-network-detach-confirm"
                  disabled={detach.isPending}
                  onClick={() => detach.mutate()}
                >
                  Detach
                </Button>
              </DialogFooter>
            </DialogContent>
          </Dialog>
        ) : null}
      </CardContent>
    </Card>
  );
}
