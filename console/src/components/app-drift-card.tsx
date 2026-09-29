// 应用漂移卡（2026-09-25 审查 backlog #9 / §3 P1-5「Drift 全无 UI」）：
// 运行域漂移读面 + Converge now 动作 + 自动收敛 opt-in 开关。一切展示以
// proto 契约（drift.proto）与服务端代码事实为准：
//   - ShowDriftResponse 三态：desired_deployment 空 = 无成功部署（无期望
//     态基准，无从判定）；drifted=false = in sync；drifted=true = drifted
//     （逐服务 missing/extra/字段级 diff——diff 值为投影形态，env 只到
//     key:hash，无明文泄漏面）。与 CLI `fleetly drift show` 渲染同源。
//   - ConvergeDrift 是入队式（归位重放原语，状态经部署面跟踪；响应的
//     deployment_id 是收敛基准部署而非新在途行）。契约无确认参数（CLI
//     --confirm-destructive 属 deploy 动词）——本卡的确认框是前端一次性
//     防误触，提交即生效；在途部署存在时服务端 409 E_STATE_VERSION_CONFLICT。
//     收敛跟踪：2s 轮询 ShowDrift 直至 drifted 清零（预算 90s，超时如实
//     指向 Deployments 页，不冒充成功）。
//   - 自动收敛 opt-in（apps.drift_converge，默认关；回滚失败被守护进程
//     强制关闭）**无读取面**：ShowDrift/AppView 均不带该位（engine
//     GetDriftConverge 零调用方）——开关只做显式置位并回显响应值，初态
//     如实标注 unknown，不臆造当前状态。
// 角色门（服务端 scope 登记：ShowDrift=read、Converge/Set=deploy；平台
// 管理员资源面 levelRead）：读面全角色；Converge now 与 opt-in 开关 =
// canDeploy（2026-09-25 复核对齐：SetDriftConverge 服务端即 ScopeDeploy
// 登记——developer 服务端可写、UI 按 admin 收口不可见且无说明是不一致
// 缺陷，前端门改同口径）；平台管理员按 platform-readonly-note 既有形态
// 原位说明。

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { GitCompare } from "lucide-react";
import { useEffect, useState } from "react";

import {
  convergeDrift,
  setDriftConvergence,
  showDrift,
} from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import type { ServiceDriftView, ShowDriftResponse } from "@/api/types";
import { EnvelopeAlert, EnvelopeAlertFrom } from "@/components/envelope-alert";
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
import { useIsPlatformAdmin, useTeamCapabilities } from "@/lib/context";

/** 收敛跟踪预算（入队部署走 planning→building→releasing→observing 常规
 * 管线；预算内未清零按「仍在跟踪」如实呈现，不判成功也不判失败）。 */
const CONVERGE_TRACK_BUDGET_MS = 90_000;

/** 漂移状态徽章（state-badge 视觉形态；色调按本卡三态语义直配）。 */
function DriftStatusBadge({ tone, label }: { tone: "green" | "red" | "gray"; label: string }) {
  return (
    <span
      data-testid="drift-status-badge"
      data-state={label}
      className="inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 text-xs font-semibold"
    >
      <StatusDot tone={tone} />
      {label}
    </span>
  );
}

/** 单服务漂移行（missing/extra/diff 展示与 CLI drift show 同口径，英文
 * 文案对齐 cmd/fleetly/cmd/rollback.go 的渲染语义）。 */
function ServiceDriftRows({ services }: { services: ServiceDriftView[] }) {
  return (
    <div data-testid="drift-summary" className="space-y-2">
      {services.map((s) => (
        <div key={s.service} className="rounded-md border p-2">
          <div className="flex flex-wrap items-center gap-2 text-xs font-medium">
            <code className="font-mono">{s.service}</code>
            {s.missing ? <span className="text-red-600 dark:text-red-400">missing — in the desired state, absent from runtime</span> : null}
            {s.extra ? <span className="text-red-600 dark:text-red-400">extra — managed service outside the desired set</span> : null}
            {!s.missing && !s.extra && !s.drifted ? (
              <span className="text-muted-foreground">ok</span>
            ) : null}
          </div>
          {(s.diff ?? []).length > 0 ? (
            <table className="mt-1 w-full text-xs">
              <tbody>
                {(s.diff ?? []).map((d) => (
                  <tr key={`${s.service}:${d.field}`} className="border-t">
                    <td className="py-0.5 pr-2 font-mono text-muted-foreground">{d.field}</td>
                    <td className="py-0.5 pr-2 font-mono break-all">{d.expected}</td>
                    <td className="py-0.5 pr-2 text-muted-foreground">→</td>
                    <td className="py-0.5 font-mono break-all">{d.actual}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : null}
        </div>
      ))}
    </div>
  );
}

interface AppDriftCardProps {
  app: string;
}

export function AppDriftCard({ app }: AppDriftCardProps) {
  const queryClient = useQueryClient();
  // 前端体验门与服务端 ScopeDeploy 登记同口径：收敛与 opt-in 开关都是
  // deploy 面（canDeploy）——不再比服务端更严收口到 admin。
  const { canDeploy } = useTeamCapabilities();
  const isPlatformAdmin = useIsPlatformAdmin();

  // 收敛跟踪：不落独立 phase state——由「最近一次收敛入队时刻」+ 预算
  // 到点位 + 漂移面当前数据派生：idle（从未收敛）→ done（drifted 清零）
  // → tracking（预算内）→ timeout（预算耗尽仍在漂移，如实指向 Deployments
  // 页，不冒充成功）。预算到点由 effect 内的 setTimeout 置位（时钟读取
  // 收在 effect/回调里，渲染期保持纯函数）。
  const [convergedAt, setConvergedAt] = useState(0);
  const [budgetExpired, setBudgetExpired] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [convergeError, setConvergeError] = useState<ReturnType<typeof errorEnvelopeFrom> | null>(null);
  const [toggleError, setToggleError] = useState<ReturnType<typeof errorEnvelopeFrom> | null>(null);
  // opt-in 回显（SetDriftConverge 响应 echo；null = 未知——API 无读取面）。
  const [optInEcho, setOptInEcho] = useState<boolean | null>(null);

  // 派生跟踪相位（优先序：done > timeout > tracking——清零即成功，即使
  // 预算定时器随后到点也不再回退）。漂移数据经 queryClient.getQueryData
  // 纯读缓存：派生必须发生在 useQuery 声明之前（v5 的 observer 在渲染期
  // 同步求值 refetchInterval，读后声明的订阅数据会 TDZ）；同一缓存条目，
  // 与下方订阅数据恒同源。
  const cachedDrifted = queryClient.getQueryData<ShowDriftResponse>(["drift", app])?.drifted;
  const trackPhase: "idle" | "tracking" | "done" | "timeout" =
    convergedAt === 0
      ? "idle"
      : cachedDrifted === false
        ? "done"
        : budgetExpired
          ? "timeout"
          : "tracking";

  const driftQuery = useQuery({
    queryKey: ["drift", app],
    queryFn: () => showDrift(app),
    // 跟踪期 2s 加密轮询；常态 15s 低频跟随（检测是读面但非零成本——
    // 逐服务 ServiceInspect + 投影哈希）。
    refetchInterval: trackPhase === "tracking" ? 2000 : 15000,
  });

  // 预算定时器：到点翻转 budgetExpired（回调内 setState——外部系统订阅
  // 形态，非 effect 体内同步置位）。
  useEffect(() => {
    if (convergedAt === 0) return;
    const remaining = CONVERGE_TRACK_BUDGET_MS - (Date.now() - convergedAt);
    const timer = setTimeout(() => setBudgetExpired(true), Math.max(remaining, 0));
    return () => clearTimeout(timer);
  }, [convergedAt]);

  const converge = useMutation({
    mutationFn: () => convergeDrift(app),
    onSuccess: () => {
      setConvergeError(null);
      setBudgetExpired(false);
      setConvergedAt(Date.now());
      // 收敛入队后立即重取漂移面（不等 2s 轮询拍），部署台账随入队变化
      // 一并失效让 Deployments 页跟进。
      void queryClient.invalidateQueries({ queryKey: ["drift", app] });
      void queryClient.invalidateQueries({ queryKey: ["deployments", app] });
    },
    onError: (err) => setConvergeError(errorEnvelopeFrom(err)),
  });

  const setConvergence = useMutation({
    mutationFn: (enabled: boolean) => setDriftConvergence(app, enabled),
    onSuccess: (resp) => {
      setToggleError(null);
      setOptInEcho(resp.enabled ?? null);
    },
    onError: (err) => setToggleError(errorEnvelopeFrom(err)),
  });

  const report = driftQuery.data;

  // 状态三态判定（gateway EmitUnpopulated=true：drifted=false/desired_
  // deployment="" 显式输出——真值判定语义不变："" 与 false 均为假值）。
  let status: { tone: "green" | "red" | "gray"; label: string };
  if (!report?.desired_deployment) {
    status = { tone: "gray", label: "no baseline" };
  } else if (report.drifted === true) {
    status = { tone: "red", label: "drifted" };
  } else {
    status = { tone: "green", label: "in sync" };
  }

  const driftedServices = (report?.services ?? []).filter((s) => s.drifted || s.missing || s.extra);

  return (
    <Card data-testid="app-drift-card" className="md:col-span-2">
      <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
        <GitCompare aria-hidden className="h-4 w-4 text-muted-foreground" />
        <CardTitle className="text-sm font-semibold">Drift</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3 pt-4">
        {driftQuery.isError ? (
          <EnvelopeAlertFrom envelope={errorEnvelopeFrom(driftQuery.error)} />
        ) : driftQuery.isPending ? (
          <p className="text-sm text-muted-foreground">Loading…</p>
        ) : (
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="min-w-0 space-y-1">
              <div className="flex items-center gap-2">
                <DriftStatusBadge tone={status.tone} label={status.label} />
                {status.label === "no baseline" ? (
                  <span className="text-xs text-muted-foreground">
                    No succeeded deployment — there is no desired state to compare against.
                  </span>
                ) : null}
                {status.label === "in sync" ? (
                  <span className="text-xs text-muted-foreground">
                    Runtime matches the desired state.
                  </span>
                ) : null}
                {status.label === "drifted" ? (
                  <span className="text-xs text-muted-foreground">
                    Runtime differs from the desired state (baseline{" "}
                    <code className="font-mono">{report?.desired_deployment}</code>).
                  </span>
                ) : null}
              </div>
            </div>
            {canDeploy ? (
              <Button
                size="sm"
                data-testid="drift-converge-button"
                disabled={converge.isPending || trackPhase === "tracking"}
                onClick={() => {
                  setConvergeError(null);
                  setConfirmOpen(true);
                }}
              >
                Converge now
              </Button>
            ) : isPlatformAdmin ? null : (
              <p className="text-xs text-muted-foreground">
                Converge requires the deploy face (developer role or higher).
              </p>
            )}
          </div>
        )}

        {convergeError ? (
          <EnvelopeAlert
            code={convergeError.code}
            message={convergeError.message}
            suggestion={convergeError.suggestion}
          />
        ) : null}

        {trackPhase === "tracking" ? (
          <p className="text-xs text-muted-foreground" data-testid="drift-converge-tracking">
            Convergence enqueued (baseline deployment{" "}
            <code className="font-mono">{converge.data?.deployment_id}</code>) — waiting
            for drift to clear…
          </p>
        ) : null}
        {trackPhase === "done" ? (
          <p className="text-xs" data-testid="drift-converge-done">
            Convergence applied — runtime matches the desired state.
          </p>
        ) : null}
        {trackPhase === "timeout" ? (
          <p className="text-xs text-muted-foreground" data-testid="drift-converge-timeout">
            The convergence deployment is still tracked on the Deployments page; drift is
            still detected and will clear once it reaches a succeeded state.
          </p>
        ) : null}

        {status.label === "drifted" && driftedServices.length > 0 ? (
          <ServiceDriftRows services={driftedServices} />
        ) : null}

        {/* 自动收敛 opt-in（检测恒开、收敛默认关）：无读取面——开关只做
            显式置位并回显响应；写面（deploy 门）不可用时按钮不渲染，说明
            文案随之换为受控方语义（2026-09-25 走查：只读视角此前指向不
            存在的 "buttons below"）。 */}
        <div className="border-t pt-3">
          <div className="text-sm font-medium">Automatic convergence</div>
          <p className="mt-1 text-xs text-muted-foreground">
            Detection runs continuously; automatic convergence is opt-in per app
            (default off) and is force-disabled by the daemon after a failed
            rollback. The API does not expose the current opt-in state —{" "}
            {canDeploy && !isPlatformAdmin
              ? "the buttons below set it explicitly (the server echoes the applied value). "
              : "convergence is controlled by team admins. "}
            The CLI equivalent is <code>fleetly drift enable|disable</code>.
          </p>
          <p className="mt-1 text-xs" data-testid="drift-convergence-state">
            Opt-in state:{" "}
            {optInEcho === null ? (
              <span className="text-muted-foreground">unknown (not exposed by the API)</span>
            ) : optInEcho ? (
              <span className="font-medium">on</span>
            ) : (
              <span className="font-medium">off</span>
            )}
            {optInEcho !== null ? (
              <span className="text-muted-foreground"> (set from this session)</span>
            ) : null}
          </p>
          {toggleError ? (
            <EnvelopeAlert
              code={toggleError.code}
              message={toggleError.message}
              suggestion={toggleError.suggestion}
            />
          ) : null}
          {canDeploy && !isPlatformAdmin ? (
            <div className="mt-2 flex gap-2" data-testid="drift-convergence-toggle">
              <Button
                size="sm"
                variant="outline"
                data-testid="drift-convergence-enable"
                disabled={setConvergence.isPending}
                onClick={() => setConvergence.mutate(true)}
              >
                Enable
              </Button>
              <Button
                size="sm"
                variant="outline"
                data-testid="drift-convergence-disable"
                disabled={setConvergence.isPending}
                onClick={() => setConvergence.mutate(false)}
              >
                Disable
              </Button>
            </div>
          ) : isPlatformAdmin ? (
            // 平台管理员双门（P0-3 形态）：资源面写 403——原位说明指 CLI。
            <p className="mt-2 text-xs text-muted-foreground" data-testid="platform-readonly-note">
              Platform administrators have read-only access to resources
              (separation of duties). Toggle convergence from the CLI with a
              machine token (<code>fleetly drift enable|disable</code>), or ask
              a team owner for a member role.
            </p>
          ) : null}
        </div>
      </CardContent>

      {confirmOpen ? (
        <Dialog open onOpenChange={(v) => (v ? undefined : setConfirmOpen(false))}>
          <DialogContent data-testid="drift-converge-dialog">
            <DialogHeader>
              <DialogTitle>Converge {app} to its desired state?</DialogTitle>
              <DialogDescription>
                Convergence replays the last succeeded deployment onto the
                runtime: drifted services are recreated (a rolling update that
                restarts their tasks) and managed services outside the desired
                set are removed. The convergence runs as a regular deployment —
                it is refused while another deployment is in flight. This
                confirmation is advisory: the server applies the convergence
                immediately, there is no extra confirmation parameter in the
                request contract.
              </DialogDescription>
            </DialogHeader>
            <DialogFooter>
              <Button
                size="sm"
                data-testid="drift-converge-submit"
                disabled={converge.isPending}
                onClick={() => {
                  setConfirmOpen(false);
                  converge.mutate();
                }}
              >
                {converge.isPending ? "Converging…" : "Converge now"}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      ) : null}
    </Card>
  );
}
