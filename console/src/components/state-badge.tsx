// 状态徽章：fleetly 颜色语义统一（票面口径）——
//   running/succeeded   = 绿（健康终态）
//   degraded            = 琥珀（部分可用）
//   blocked/failed/down = 红（不可用/失败）
//   中间态（queued/preparing/building/releasing/observing/blocked_waiting/
//   deleting/cancelled）= 中性蓝/灰（进行中或非失败终态）
// 未知状态按中性灰兜底——不发明词表外的展示语义。

import { cn } from "@/lib/utils";
import { StatusDot } from "@/components/status-dot";

export type StateTone = "green" | "amber" | "red" | "neutral-blue" | "gray";

export const TONE_CLASSES: Record<StateTone, string> = {
  green: "bg-emerald-500",
  amber: "bg-amber-500",
  red: "bg-red-500",
  "neutral-blue": "bg-sky-500",
  gray: "bg-zinc-400",
};

export const INTERMEDIATE = new Set([
  "queued",
  "preparing",
  "building",
  "releasing",
  "observing",
]);

export function stateTone(state: string): StateTone {
  switch (state) {
    case "running":
    case "succeeded":
      return "green";
    case "degraded":
      return "amber";
    // suspended = 用户挂起（app Stop，权威位直投影）：琥珀——非失败、可行动
    //（Start 在标题栏）。DB paused 走灰（无动作语义），app 挂起有直接恢复
    // 动作，色阶上与 degraded 同档「需注意但非故障」。
    case "suspended":
      return "amber";
    case "blocked":
    case "failed":
    case "down":
      return "red";
    case "blocked_waiting":
      return "neutral-blue";
    case "deleting":
    case "cancelled":
      return "gray";
    default:
      if (INTERMEDIATE.has(state)) return "neutral-blue";
      return "gray";
  }
}

export function StateBadge({
  state,
  className,
}: {
  state: string;
  className?: string;
}) {
  return (
    <span
      data-testid="state-badge"
      data-state={state}
      className={cn(
        "inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 text-xs font-semibold",
        className,
      )}
    >
      <StatusDot state={state} />
      {state || "unknown"}
    </span>
  );
}
