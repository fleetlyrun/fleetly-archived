import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

// shadcn/ui 标配：类名合并（cn）。
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// 相对时间的操作者可读形态（列表/历史行的辅助信息面）。
export function timeAgo(iso: string | undefined): string {
  if (!iso) return "—";
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "—";
  const seconds = Math.max(0, Math.floor((Date.now() - then) / 1000));
  if (seconds < 5) return "just now";
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  return `${days}d ago`;
}

export function formatTime(iso: string | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleString();
}

// 时段量的操作者可读形态（部署耗时等）。入参毫秒；负数/非有限值 = 无从
// 计算，按「—」呈现而非 0s（不把缺数据渲染成瞬时完成）。
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "—";
  const seconds = Math.round(ms / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${seconds % 60}s`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours}h ${minutes % 60}m`;
  const days = Math.floor(hours / 24);
  return `${days}d ${hours % 24}h`;
}

// 字节量的操作者可读形态（备份台账 size_bytes——proto int64 在 JSON 面是
// 字符串，调用方负责归一；0/缺失 = 未记录，按「—」呈现而非 0 B）。
export function formatBytes(value: string | number | undefined): string {
  const n = typeof value === "string" ? Number(value) : value;
  if (n === undefined || Number.isNaN(n) || n <= 0) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let v = n;
  let u = 0;
  while (v >= 1024 && u < units.length - 1) {
    v /= 1024;
    u += 1;
  }
  return `${v >= 100 || u === 0 ? Math.round(v) : v.toFixed(1)} ${units[u]}`;
}
