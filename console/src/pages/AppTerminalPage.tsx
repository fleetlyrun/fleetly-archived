// Web 终端页（E7 W5-S6，web-terminal §2.5）：服务选择 → ticket 签发 →
// WS 接入 @xterm/xterm 渲染。面板常显：平台状态（enabled/relay 部署态/
// 已连接节点/活跃会话——GetTerminalStatus 轮询）+ 会话状态行（目标服务/
// 剩余时限/断线原因——服务端 close 帧的 reason 承载，不发明第二套文案）。
//
// 范围纪律（设计 §2.3）：用户只选 service，不选 task/容器（副本细节对操
// 作员透明）；cron 服务不进选择集（一次性 job 无长驻态可进）。白名单
// shell 探测在 relay 侧（/bin/bash、/bin/sh）——UI 无 shell 选择面。
//
// 锚点（只增）：terminal-panel / terminal-open-button / terminal-status-line
// / terminal-close-button / terminal-session-view / terminal-disabled-note。

import { useQuery } from "@tanstack/react-query";
import { TerminalSquare } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { useParams } from "react-router-dom";

import { createTerminalTicket, getApp, getRevisionSpec, getTerminalStatus, listRevisions } from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import { connectTerminal, type SessionCloseFrame, type TerminalConnection } from "@/api/terminal-ws";
import { EnvelopeAlert } from "@/components/envelope-alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { extractServiceNames } from "@/lib/compose-cron";
import { useTeamCapabilities, useIsPlatformAdmin } from "@/lib/context";
import { AppTerminalView } from "@/terminal/app-terminal-view";

/** 会话面状态（状态行的有限词表——idle/opening/active/closed）。 */
export type TerminalPhase = "idle" | "opening" | "active" | "closed";

/** 硬上限倒计时的基准（设计 §2.4：空闲 10min / 硬上限 30min——连接侧与
 * relay 侧双保险；剩余时限是 30min 硬上限的投影）。 */
const HARD_LIMIT_SECONDS = 30 * 60;

export function AppTerminalPage() {
  const { name = "" } = useParams();
  // 角色门（前端体验门，§3.2）：Web 终端 = developer+（viewer 不给接入口）。
  // 平台管理员资源面恒只读（P0-3 双门）——接入口消失时在禁用说明里如实
  // 说明原因（不与「角色不够」混用一条文案）。
  const { canDeploy } = useTeamCapabilities();
  const isPlatformAdmin = useIsPlatformAdmin();

  // 平台状态（10s 轮询——连接表/会话表是活数据）。
  const status = useQuery({
    queryKey: ["terminal", "status"],
    queryFn: getTerminalStatus,
    refetchInterval: 10000,
  });
  const appQuery = useQuery({ queryKey: ["app", name], queryFn: () => getApp(name) });
  const revisionsQuery = useQuery({ queryKey: ["revisions", name], queryFn: () => listRevisions(name) });
  const active = (revisionsQuery.data?.revisions ?? []).find((r) => r.status === "active");
  const specQuery = useQuery({
    queryKey: ["revision-spec", name, active?.id],
    queryFn: () => getRevisionSpec(name, active!.id ?? ""),
    enabled: active !== undefined,
  });
  // 服务选择集：最近 active revision 的长驻服务（cron 的一次性 job 不进）。
  const services = useMemo(
    () => (extractServiceNames(specQuery.data?.compose) ?? []).filter((s) => !s.isCron),
    [specQuery.data?.compose],
  );
  const [service, setService] = useState("");
  // 默认选中 = 首个长驻服务（渲染期回落，不经 effect——避免级联渲染）。
  const effectiveService = service || services[0]?.name || "";

  const enabled = status.data?.enabled ?? true;
  const err = status.isError
    ? errorEnvelopeFrom(status.error)
    : appQuery.isError
      ? errorEnvelopeFrom(appQuery.error)
      : null;

  // ── 会话面 ──────────────────────────────────────────────────────────────
  const [phase, setPhase] = useState<TerminalPhase>("idle");
  const [closeInfo, setCloseInfo] = useState<SessionCloseFrame | null>(null);
  const [conn, setConn] = useState<TerminalConnection | null>(null);
  const startedAtRef = useRef(0);
  const [remaining, setRemaining] = useState("");
  // 输出汇（AppTerminalView 挂载 xterm 后注册——connectTerminal 的 onData
  // 经此落进终端缓冲）。
  const sinkRef = useRef<(data: Uint8Array) => void>(() => {});

  // 剩余时限（硬上限倒计时投影——会话激活时每秒刷新；首个整秒前状态行
  // 以 "30:00" 兜底）。
  useEffect(() => {
    if (phase !== "active") {
      return;
    }
    const t = setInterval(() => {
      const left = Math.max(0, HARD_LIMIT_SECONDS - Math.floor((Date.now() - startedAtRef.current) / 1000));
      setRemaining(`${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")}`);
    }, 1000);
    return () => clearInterval(t);
  }, [phase]);

  const openSession = () => {
    if (!effectiveService || conn || phase === "opening" || phase === "active") return;
    setPhase("opening");
    setCloseInfo(null);
    createTerminalTicket(name, effectiveService)
      .then((resp) => {
        const path = resp.websocket_path ?? `/v1/terminal?ticket=${resp.ticket ?? ""}`;
        startedAtRef.current = Date.now();
        setPhase("active");
        setConn(
          connectTerminal(path, {
            onData: (data) => sinkRef.current(data),
            onClose: (frame) => {
              setCloseInfo(frame);
              setPhase("closed");
              setConn(null);
            },
            onDisconnect: (reason) => {
              setCloseInfo({ id: "", code: 0, reason });
              setPhase("closed");
              setConn(null);
            },
          }),
        );
      })
      .catch((e) => {
        const envelope = errorEnvelopeFrom(e);
        setCloseInfo({ id: "", code: 0, reason: envelope.message || "ticket request failed" });
        setPhase("closed");
      });
  };

  const closeSession = () => {
    conn?.close();
    setCloseInfo({ id: "", code: 0, reason: "closed by operator" });
    setPhase("closed");
    setConn(null);
  };

  const statusLine = buildStatusLine(phase, closeInfo, effectiveService, remaining);

  return (
    <div className="space-y-4" data-testid="terminal-panel">
      <Card>
        <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
          <TerminalSquare aria-hidden className="h-4 w-4 text-muted-foreground" />
          <CardTitle className="text-sm font-semibold">Terminal</CardTitle>
          <span
            className="ml-auto rounded-md border px-1.5 py-0.5 text-xs text-muted-foreground"
            data-testid="terminal-platform-state"
          >
            {!enabled
              ? "disabled (terminal.enabled=false)"
              : status.data
                ? `relay ${status.data.relay_deployed ? "deployed" : "converging"} · ${status.data.nodes_connected ?? 0} node(s) connected · ${status.data.active_sessions ?? 0} session(s)`
                // 状态未到（查询在途）的加载态占位——不渲染字面 "status…"
                //（2026-09-25 审查 P2-10）。
                : "—"}
          </span>
        </CardHeader>
        <CardContent className="space-y-3 pt-4">
          {err ? <EnvelopeAlert code={err.code} message={err.message} suggestion={err.suggestion} docs={err.docs} /> : null}
          {!enabled || !canDeploy ? (
            <p className="text-sm text-muted-foreground" data-testid="terminal-disabled-note">
              {!enabled
                ? "The web terminal is disabled in the control plane config (terminal.enabled). Enable it and restart fleetlyd — the exec relay manager converges the fleetly-exec service on every node automatically."
                : isPlatformAdmin
                  ? "Platform administrators have read-only access to resources (separation of duties), so the web terminal is not available. Ask a team owner for a member role (developer or higher)."
                  : "The web terminal requires the developer role or higher in this app's project — your account is read-only here."}
            </p>
          ) : (
            <>
              <div className="flex flex-wrap items-center gap-2 text-sm">
                <label className="text-muted-foreground" htmlFor="terminal-service">
                  Service
                </label>
                <select
                  id="terminal-service"
                  className="h-8 rounded-md border bg-background px-2 text-sm"
                  value={effectiveService}
                  onChange={(e) => setService(e.target.value)}
                  disabled={phase === "active" || phase === "opening"}
                  data-testid="terminal-service-select"
                >
                  {services.length === 0 ? <option value="">(no long-running service)</option> : null}
                  {services.map((s) => (
                    <option key={s.name} value={s.name}>
                      {s.name}
                    </option>
                  ))}
                </select>
                {phase === "active" ? (
                  <Button size="sm" variant="outline" onClick={closeSession} data-testid="terminal-close-button">
                    Disconnect
                  </Button>
                ) : (
                  <Button
                    size="sm"
                    onClick={openSession}
                    disabled={!effectiveService || phase === "opening"}
                    data-testid="terminal-open-button"
                  >
                    {phase === "opening" ? "Connecting…" : "Open terminal"}
                  </Button>
                )}
              </div>
              <div
                className="rounded-md border bg-muted/30 px-3 py-2 font-mono text-xs text-muted-foreground"
                data-testid="terminal-status-line"
              >
                {statusLine}
              </div>
              <AppTerminalView
                phase={phase}
                registerSink={(fn) => {
                  sinkRef.current = fn;
                }}
                onInput={(data) => conn?.write(data)}
                onResize={(cols, rows) => conn?.resize(cols, rows)}
              />
            </>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

// buildStatusLine 是状态行的单行投影（英文——对外文本纪律；断线原因取
// 服务端 close.reason 原文）。
function buildStatusLine(
  phase: TerminalPhase,
  closeInfo: SessionCloseFrame | null,
  service: string,
  remaining: string,
): string {
  switch (phase) {
    case "idle":
      return "idle — pick a service and open a terminal (whitelisted shells: /bin/bash, /bin/sh; idle 10m / hard 30m limits)";
    case "opening":
      return `connecting to ${service}…`;
    case "active":
      return `connected to ${service} · time left ${remaining || "30:00"} (hard limit)`;
    case "closed":
      return closeInfo
        ? `closed — code ${closeInfo.code}${closeInfo.reason ? `: ${closeInfo.reason}` : ""}`
        : "closed";
  }
}
