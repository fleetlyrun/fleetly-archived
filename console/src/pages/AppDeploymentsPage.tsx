// 部署历史页（2026-09-29 三轮重设计 §4.7，对齐 dokploy Deployments 页签）：
// 部署入口（Deploy 卡）上移 Overview 首页签后，本页收敛为纯历史面——
// 顶部重部署 webhook URL（admin+，dokploy「想在 git provider / CI 里重部署
// 就用这个 URL」同构，接收端按业务名分派，见 app-deploy-card.tsx 头注）+
// 历史表（状态徽章含 blocked_waiting/observing 等中间态；失败行 code+
// verdict+recovery 信封形态；行内 What changed 字段级 diff + Compose 快照
// 对话框 + 行内 Rollback/Cancel；时长列 = 入队→终态〔updated_at 是 state 层
// 自证的终态写入时刻近似，internal/state/deployments.go〕，进行中行显示
// 已流逝时间）+ 批量取消（Cancel queued——服务端取消语义受限：未切流才可
// 取消，曾健康/终态 409，故批量结果如实汇报成败计数，不做全成假象）。
//
// 平台管理员资源面恒只读（P0-3 双门）——只读说明卡原位，行内写操作按
// 能力门（canDeploy）不渲染，不做静默消失。

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { History, Loader2, OctagonX, Undo2 } from "lucide-react";
import { useState } from "react";
import { useParams } from "react-router-dom";

import {
  cancelDeployment,
  listDeployments,
  rollbackDeployment,
  showAppWebhook,
} from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import type { DeploymentView } from "@/api/types";
import {
  CopyValueRow,
  TERMINAL,
  WEBHOOK_NAME_PATTERN,
  webhookReceiverUrl,
} from "@/components/app-deploy-card";
import { ComposeDialog } from "@/components/app-compose-card";
import { DeploymentDiff } from "@/components/deployment-diff";
import { DeploymentFailureAlert, EnvelopeAlert } from "@/components/envelope-alert";
import { SectionCard } from "@/components/section-card";
import { StateBadge } from "@/components/state-badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatDuration, formatTime, timeAgo } from "@/lib/utils";
import { useIsPlatformAdmin, useMeQuery, useTeamCapabilities } from "@/lib/context";

/**
 * 「上一部署」的选取（T0-V2.4）：部署历史（最新在前）中当前行之前最近一条
 * 带 revision 的部署行。中间的失败/进行中行没有固化快照（revision 仅成功
 * 终态写入），跳过它们——「这次部署改了什么」的对照基准是上一个真实固化的
 * 版本，而非一条没有落盘任何变更的失败尝试。
 */
function previousWithRevision(
  deployments: DeploymentView[],
  index: number,
): DeploymentView | undefined {
  for (let i = index + 1; i < deployments.length; i++) {
    const d = deployments[i];
    if (d?.revision_id) return d;
  }
  return undefined;
}

// ── 时长列（dokploy 每行耗时徽章同构）────────────────────────────────────
// 终态行 = created_at → updated_at（updated_at 恒重盖，state 层自注是「终态
// 写入时刻的最近似代理」——入队等待含在内，title 如实说明口径）；进行中行
// = 至当前的已流逝时间（随页面轮询节拍刷新，不另起计时器）。

function deploymentDuration(d: DeploymentView): string {
  if (!d.created_at) return "—";
  const created = Date.parse(d.created_at);
  if (Number.isNaN(created)) return "—";
  if (TERMINAL.has(d.status ?? "")) {
    if (!d.updated_at) return "—";
    const end = Date.parse(d.updated_at);
    if (Number.isNaN(end) || end < created) return "—";
    return formatDuration(end - created);
  }
  return formatDuration(Date.now() - created);
}

// ── 批量取消（dokploy Cancel Queues 同构）────────────────────────────────
// 服务端 CancelDeployment 是受限语义：未切流才能取消，曾健康/终态 409
//（internal/api/deployments.go）。批量逐条调用，结果如实汇报——已切流的行
// 取消失败是服务端事实，不静默吞掉，也不阻塞其余行的取消。

function CancelQueuedDialog({
  app,
  rows,
  onClose,
}: {
  app: string;
  rows: DeploymentView[];
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<{ cancelled: number; refused: number } | null>(null);
  const [error, setError] = useState<ReturnType<typeof errorEnvelopeFrom> | null>(null);

  async function onConfirm() {
    setBusy(true);
    setError(null);
    let cancelled = 0;
    let refused = 0;
    for (const row of rows) {
      try {
        await cancelDeployment(row.id ?? "");
        cancelled += 1;
      } catch {
        refused += 1;
      }
    }
    setBusy(false);
    void queryClient.invalidateQueries({ queryKey: ["deployments", app] });
    if (refused === 0) {
      onClose();
      return;
    }
    setResult({ cancelled, refused });
  }

  return (
    <Dialog open onOpenChange={(v) => (v ? undefined : onClose())}>
      <DialogContent data-testid="deployments-cancel-queued-dialog">
        <DialogHeader>
          <DialogTitle>Cancel {rows.length} in-flight deployment{rows.length === 1 ? "" : "s"}?</DialogTitle>
          <DialogDescription>
            Cancels every deployment that has not reached a terminal state.
            Deployments that already switched traffic refuse cancellation
            (server-side guard) and keep converging to their terminal state —
            they stay listed in the history.
          </DialogDescription>
        </DialogHeader>
        <ul className="space-y-1 font-mono text-xs text-muted-foreground">
          {rows.map((r) => (
            <li key={r.id}>
              <span title={r.id}>{(r.id ?? "").slice(0, 12)}</span>
              {" · "}
              {r.status ?? ""}
              {r.phase === "blocked_waiting" ? " (blocked_waiting)" : ""}
            </li>
          ))}
        </ul>
        {result ? (
          <p className="text-sm text-amber-800 dark:text-amber-300" data-testid="deployments-cancel-queued-result">
            Cancelled {result.cancelled} · {result.refused} refused cancellation
            (already serving or terminal) — they converge on their own.
          </p>
        ) : null}
        {error ? (
          <EnvelopeAlert
            code={error.code}
            message={error.message}
            suggestion={error.suggestion}
            docs={error.docs}
          />
        ) : null}
        <DialogFooter>
          <Button
            variant="outline"
            size="sm"
            data-testid="deployments-cancel-queued-close"
            onClick={onClose}
          >
            Close
          </Button>
          <Button
            size="sm"
            variant="destructive"
            data-testid="deployments-cancel-queued-confirm"
            disabled={busy}
            onClick={() => void onConfirm()}
          >
            {busy ? <Loader2 aria-hidden className="h-4 w-4 animate-spin" /> : null}
            Cancel deployments
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ── 重部署 webhook URL 行（admin+；dokploy Deployments 顶部 URL 同构）────
// 值 = 接收端 URL（业务名寻址，webhookReceiverUrl 单点拼装）；读失败呈
// 中性说明而非错误大-alert（触发面配置的完整编辑在 Overview 的 Deploy 卡，
// 本行只是快捷复制位）。

function HistoryWebhookRow({
  cfg,
  readError,
}: {
  cfg: Awaited<ReturnType<typeof showAppWebhook>> | undefined;
  readError: ReturnType<typeof errorEnvelopeFrom> | null;
}) {
  return (
    <div className="space-y-1 border-b p-4" data-testid="history-webhook-row">
      <CopyValueRow
        label="Redeploy webhook URL"
        value={cfg?.name ? webhookReceiverUrl(cfg.name, "github") : ""}
        testid="history-webhook-url"
        empty={readError ? "Receiver URL unavailable." : "Loading trigger configuration…"}
        hint={
          <>
            Deliveries to this URL re-deploy the app — point your git provider
            or CI at it (Gitea: same path with <code className="font-mono">/gitea</code>).
            The receiver answers 404 until a signing secret is configured;
            trigger settings (git push remote, fetch source, secret) live in
            the Deploy card on the Overview tab.
          </>
        }
      />
      {readError ? (
        <p className="text-xs text-muted-foreground" data-testid="history-webhook-error">
          Trigger configuration unavailable ({readError.code}) — an admin team
          role is required.
        </p>
      ) : null}
      {cfg?.name && !WEBHOOK_NAME_PATTERN.test(cfg.name) ? (
        <p
          className="text-xs text-amber-800 dark:text-amber-300"
          data-testid="history-webhook-name-note"
        >
          This app&apos;s name contains characters outside [a-z0-9-]: the webhook
          receiver path only accepts lowercase letters, digits and dashes, so
          webhook triggers are unavailable for this app (server-side
          limitation).
        </p>
      ) : null}
    </div>
  );
}

function DeploymentRow({
  d,
  app,
  expanded,
  onToggle,
  previous,
}: {
  d: DeploymentView;
  app: string;
  expanded: boolean;
  onToggle: () => void;
  previous: DeploymentView | undefined;
}) {
  const queryClient = useQueryClient();
  const cancelMutation = useMutation({
    mutationFn: () => cancelDeployment(d.id ?? ""),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["deployments", app] });
    },
  });
  // 行内回滚（2026-09-29 二次重设计：在历史里选要回去的那一行；确认框
  // 承载语义：重放该行 revision 的快照，产生一条新部署行走正常发布管线）。
  const [rollbackOpen, setRollbackOpen] = useState(false);
  const rollbackMutation = useMutation({
    mutationFn: () => rollbackDeployment(app, d.revision_id ?? undefined),
    onSuccess: () => {
      setRollbackOpen(false);
      void queryClient.invalidateQueries({ queryKey: ["deployments", app] });
    },
  });
  const rollbackError = rollbackMutation.isError
    ? errorEnvelopeFrom(rollbackMutation.error)
    : null;
  const { canDeploy } = useTeamCapabilities();
  const cancellable = canDeploy && !TERMINAL.has(d.status ?? "");
  const rollbackable = canDeploy && d.revision_id !== undefined && d.revision_id !== "";
  // 行内 Compose 快照对话框（2026-09-29 §4.4：该行 revision 的实际生效快照）。
  const [composeOpen, setComposeOpen] = useState(false);
  const showState =
    d.phase === "blocked_waiting" ? "blocked_waiting" : d.status ?? "";

  return (
    <>
      <TableRow data-testid="deployment-row" data-status={d.status}>
        <TableCell className="whitespace-nowrap">
          <div className="flex items-center gap-2">
            <StateBadge state={showState} />
            {d.kind === "rollback" ? (
              <span className="text-xs text-muted-foreground">(rollback)</span>
            ) : null}
          </div>
        </TableCell>
        <TableCell className="font-mono text-xs">
          <span title={d.id}>{(d.id ?? "").slice(0, 12)}</span>
        </TableCell>
        <TableCell
          className="whitespace-nowrap text-xs text-muted-foreground"
          title={d.created_at ? formatTime(d.created_at) : undefined}
        >
          {timeAgo(d.created_at)}
        </TableCell>
        <TableCell
          className="whitespace-nowrap font-mono text-xs text-muted-foreground"
          data-testid="deployment-duration"
          title="Time from enqueue to terminal state (elapsed time while in flight; includes queue wait)"
        >
          {deploymentDuration(d)}
        </TableCell>
        <TableCell className="text-xs">
          {d.source_git_sha ? `${d.source_git_sha.slice(0, 7)}` : "—"}
        </TableCell>
        <TableCell className="max-w-[360px]">
          {d.status === "failed" && (d.error_code || d.verdict) ? (
            <DeploymentFailureAlert
              errorCode={d.error_code ?? ""}
              verdict={d.verdict ?? ""}
              recovery={d.recovery ?? ""}
              deploymentId={d.id ?? ""}
            />
          ) : (
            <span className="text-xs text-muted-foreground">
              {d.verdict || ""}
              {d.recovery ? ` · ${d.recovery}` : ""}
            </span>
          )}
        </TableCell>
        <TableCell>
          <div className="flex items-center gap-1">
            {/* 字段级 diff 展开（T0-V2.4）：仅带 revision 的行可展开——失败/
                进行中行没有快照，展开也无从对比。 */}
            {d.revision_id ? (
              <>
                <Button
                  variant="ghost"
                  size="sm"
                  aria-expanded={expanded}
                  onClick={onToggle}
                >
                  {expanded ? "Hide changes" : "What changed"}
                </Button>
                {/* 该版实际生效 compose（历史回看，2026-09-29 §4.4）。 */}
                <Button
                  variant="ghost"
                  size="sm"
                  data-testid="deployment-compose"
                  onClick={() => setComposeOpen(true)}
                >
                  Compose
                </Button>
              </>
            ) : null}
            {rollbackable ? (
              <Button
                variant="ghost"
                size="sm"
                data-testid="deployment-rollback"
                onClick={() => setRollbackOpen(true)}
              >
                Rollback
              </Button>
            ) : null}
            {cancellable ? (
              <Button
                variant="ghost"
                size="sm"
                onClick={() => cancelMutation.mutate()}
                disabled={cancelMutation.isPending}
              >
                Cancel
              </Button>
            ) : null}
          </div>
        </TableCell>
      </TableRow>
      {expanded ? (
        // 展开行不带 deployment-row 锚点（行数断言只数部署行本身）。
        <TableRow className="border-b bg-muted/30 hover:bg-muted/30">
          <TableCell colSpan={7} className="p-4">
            <DeploymentDiff app={app} deployment={d} previous={previous} />
          </TableCell>
        </TableRow>
      ) : null}
      <ComposeDialog
        app={app}
        deploymentId={d.id ?? ""}
        revisionId={d.revision_id ?? undefined}
        open={composeOpen}
        onOpenChange={setComposeOpen}
      />
      <Dialog open={rollbackOpen} onOpenChange={setRollbackOpen}>
        <DialogContent data-testid="deployment-rollback-dialog">
          <DialogHeader>
            <DialogTitle>Roll back to this deployment?</DialogTitle>
            <DialogDescription>
              Replays the revision snapshot of deployment{" "}
              <code className="font-mono text-xs">{(d.id ?? "").slice(0, 12)}</code>
              {d.created_at ? ` (${timeAgo(d.created_at)})` : ""} as a new
              deployment — it goes through the normal release pipeline and
              appears in the history. The current state stays serving until the
              rollback converges.
            </DialogDescription>
          </DialogHeader>
          {rollbackError ? (
            <EnvelopeAlert
              code={rollbackError.code}
              message={rollbackError.message}
              suggestion={rollbackError.suggestion}
              docs={rollbackError.docs}
            />
          ) : null}
          <DialogFooter>
            <Button
              variant="outline"
              size="sm"
              data-testid="deployment-rollback-cancel"
              onClick={() => setRollbackOpen(false)}
            >
              Cancel
            </Button>
            <Button
              size="sm"
              data-testid="deployment-rollback-confirm"
              disabled={rollbackMutation.isPending}
              onClick={() => rollbackMutation.mutate()}
            >
              {rollbackMutation.isPending ? (
                <Loader2 aria-hidden className="h-4 w-4 animate-spin" />
              ) : (
                <Undo2 aria-hidden className="h-4 w-4" />
              )}
              Roll back
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}

export function AppDeploymentsPage() {
  const { name = "" } = useParams();
  // 单一展开位：一次只看一条部署的 diff（再点收起）。
  const [expandedId, setExpandedId] = useState("");
  const [cancelQueuedOpen, setCancelQueuedOpen] = useState(false);
  // 角色门（前端体验门，§3.2）：回滚/取消 = developer+；触发面读
  //（ShowAppWebhook）= admin scope（scope.go 登记）→ canAdminResources
  // 同口径。平台管理员资源面恒只读（P0-3 双门，服务端 ownership.go 硬拒）
  // ——只读说明卡原位，不做静默消失。
  const { canDeploy, canAdminResources } = useTeamCapabilities();
  const isPlatformAdmin = useIsPlatformAdmin();

  const historyQuery = useQuery({
    queryKey: ["deployments", name],
    queryFn: () => listDeployments(name, 20),
    refetchInterval: (q) => {
      // 存在非终态部署行时保持 2s 快轮询（跟踪进行中的部署）；稳态 10s。
      const rows = q.state.data?.deployments ?? [];
      return rows.some((d) => !TERMINAL.has(d.status ?? "")) ? 2000 : 10000;
    },
  });
  // 重部署 webhook URL 读面（admin+）：历史页顶部的快捷复制位。enabled
  // 叠加 Me 就绪门（useMeQuery.isSuccess）——能力门 fail-open 窗口内不发
  //（developer/平台管理员的挂载期不发出注定 403 的请求）。
  const { isSuccess: meReady } = useMeQuery();
  const webhookQuery = useQuery({
    queryKey: ["app-webhook", name],
    queryFn: () => showAppWebhook(name),
    enabled: canAdminResources && meReady,
  });
  const webhookReadError = webhookQuery.isError
    ? errorEnvelopeFrom(webhookQuery.error)
    : null;

  if (historyQuery.isError) {
    const envelope = errorEnvelopeFrom(historyQuery.error);
    return (
      <EnvelopeAlert
        code={envelope.code}
        message={envelope.message}
        suggestion={envelope.suggestion}
        docs={envelope.docs}
      />
    );
  }

  const deployments = historyQuery.data?.deployments ?? [];
  const inFlight = deployments.filter((d) => !TERMINAL.has(d.status ?? ""));

  return (
    <div className="space-y-4">
      {canDeploy ? null : isPlatformAdmin ? (
        // P0-3：平台管理员资源面只读（职责分离）——说明卡原位，指明可行动
        // 路径（CLI 机具令牌 / 成员角色）；行内写操作已按能力门隐藏。
        <Card className="border-dashed">
          <CardContent
            className="p-4 text-sm text-muted-foreground"
            data-testid="platform-readonly-note"
          >
            Platform administrators have read-only access to resources
            (separation of duties). Deploy and roll back from the CLI with a
            machine token, or ask a team owner for a member role.
          </CardContent>
        </Card>
      ) : null}
      <SectionCard
        icon={History}
        title={
          <>
            Deployments{" "}
            <span className="font-normal text-muted-foreground">({deployments.length})</span>
          </>
        }
        description="Last 20 deployments — per-row rollback, compose snapshot and field-level diff."
        contentClassName="p-0"
        actions={
          canDeploy && inFlight.length > 0 ? (
            <Button
              variant="outline"
              size="sm"
              data-testid="deployments-cancel-queued"
              onClick={() => setCancelQueuedOpen(true)}
            >
              <OctagonX aria-hidden className="h-4 w-4" />
              Cancel queued ({inFlight.length})
            </Button>
          ) : null
        }
      >
        {/* 行渲染同样叠加 Me 就绪门：fail-open 窗口不出「Loading 触发配置」
            的假行（Me 落地后该行对 developer/平台管理员根本不存在）。 */}
        {canAdminResources && meReady ? (
          <HistoryWebhookRow
            cfg={webhookQuery.data}
            readError={webhookReadError}
          />
        ) : null}
        {deployments.length === 0 ? (
          <p className="p-5 text-sm text-muted-foreground">No deployments yet.</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>State</TableHead>
                <TableHead>Deployment</TableHead>
                <TableHead>Created</TableHead>
                <TableHead>Duration</TableHead>
                <TableHead>Git</TableHead>
                <TableHead>Detail</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {deployments.map((d, i) => (
                <DeploymentRow
                  key={d.id}
                  d={d}
                  app={name}
                  expanded={expandedId !== "" && expandedId === d.id}
                  onToggle={() =>
                    setExpandedId((cur) => (cur === d.id ? "" : d.id ?? ""))
                  }
                  previous={previousWithRevision(deployments, i)}
                />
              ))}
            </TableBody>
          </Table>
        )}
      </SectionCard>
      {cancelQueuedOpen ? (
        <CancelQueuedDialog
          app={name}
          rows={inFlight}
          onClose={() => setCancelQueuedOpen(false)}
        />
      ) : null}
    </div>
  );
}
