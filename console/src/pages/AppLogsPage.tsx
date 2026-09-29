// 日志页：实时跟随（NDJSON 流）+ 历史检索（时间窗/limit/source）+ 日志库
// 统一检索（W5-S2：SearchLogs——关键词/时间窗快捷项/服务/来源 chips/游标
// 加载更多；两态切换，默认直播行为不变）。
//
// 断线续读口径：FollowLogs 契约无游标参数（proto/logs.proto），重连策略
// = 记录最后一条日志的时间戳 → 先以 since=<last_ts> 回放历史补缺口 →
// 再重开跟随流。游标语义在事件流（seq）实现，见 EventsPage。

import { useQuery } from "@tanstack/react-query";
import { Pause, Play, RefreshCw, ScrollText, Search, Terminal } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useParams } from "react-router-dom";

import {
  getRevisionSpec,
  listHistoryLogs,
  listRevisions,
  searchLogs,
} from "@/api/endpoints";
import { ReconnectBackoff } from "@/api/backoff";
import { errorEnvelopeFrom } from "@/api/errors";
import { followLogs, StreamError } from "@/api/streams";
import type { LogEntryView, SearchLogRow, SearchSource } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { formatTime } from "@/lib/utils";
import { extractServiceNames } from "@/lib/compose-cron";
import { useProjectContext } from "@/lib/context";

const LIVE_CAP = 2000;
const ALL_SERVICES = "__all__";

// 检索时间窗快捷项（W5-S2 设计 §3.3）：ms 值给相对窗计算。
const SEARCH_WINDOWS: { key: string; label: string; ms: number }[] = [
  { key: "15m", label: "15m", ms: 15 * 60_000 },
  { key: "1h", label: "1h", ms: 60 * 60_000 },
  { key: "6h", label: "6h", ms: 6 * 60 * 60_000 },
  { key: "24h", label: "24h", ms: 24 * 60 * 60_000 },
  { key: "7d", label: "7d", ms: 7 * 24 * 60 * 60_000 },
];

const SEARCH_PAGE_LIMIT = 200;

// 微批 flush 周期（M9-3）：实时行先入缓冲，~100ms 合并一次 appendEntries
//（一次 setState + 一次渲染），高频流不再逐行 setState。
const FLUSH_INTERVAL_MS = 100;

// 帧内序号（M9-4）：本地单调计数器在入存储时给每行盖章——同一时间戳 +
// 同内容的合法重复行不再被去重键吞掉（渲染 key 与去重键随之唯一）。
let entrySeq = 0;
type StampedLogEntry = LogEntryView & { __seq?: number };

function stampSeq(e: LogEntryView): StampedLogEntry {
  const stamped = e as StampedLogEntry;
  if (stamped.__seq === undefined) stamped.__seq = ++entrySeq;
  return stamped;
}

function entryKey(e: StampedLogEntry): string {
  return `${e.at ?? ""}|${e.service}|${e.source}|${e.line}|${e.__seq ?? ""}`;
}

/** 服务名清单：从最近 active revision 的 canonical JSON compose 提取。
 *  快照同构 compose.Spec（services 是数组、name 在元素上），解析复用
 *  compose-cron 单点——此前本地 Object.keys 按 map 解析，拿到的是数组
 *  下标「0/1」（2026-09-29 用户报告）。 */
function useServiceNames(app: string) {
  const revisionsQuery = useQuery({
    queryKey: ["revisions", app],
    queryFn: () => listRevisions(app),
  });
  const active = (revisionsQuery.data?.revisions ?? []).find(
    (r) => r.status === "active",
  );
  const specQuery = useQuery({
    queryKey: ["revision-spec", app, active?.id],
    queryFn: () => getRevisionSpec(app, active!.id ?? ""),
    enabled: active !== undefined,
  });
  return useMemo(() => {
    const services = extractServiceNames(specQuery.data?.compose);
    return services ? services.map((s) => s.name) : [];
  }, [specQuery.data]);
}

export function AppLogsPage() {
  const { name = "" } = useParams();
  const [service, setService] = useState<string>(ALL_SERVICES);
  const [source, setSource] = useState<"container" | "build" | "all">("all");
  const [live, setLive] = useState(true);
  const [entries, setEntries] = useState<LogEntryView[]>([]);
  const [streamError, setStreamError] = useState<string>("");
  const [autoScroll, setAutoScroll] = useState(true);

  // 两态切换（W5-S2 设计 §3.3）：live = 现有直播视图（行为不变）；search =
  // 日志库统一检索（SearchLogs——VL 后端）。
  const [mode, setMode] = useState<"live" | "search">("live");

  // 历史检索窗（可选；空 = 最近 limit 条）。
  const [since, setSince] = useState("");
  const [until, setUntil] = useState("");

  // 检索态输入与结果（SearchLogs 面状态）。
  const [keyword, setKeyword] = useState("");
  const [windowKey, setWindowKey] = useState<string>("1h");
  const [searchService, setSearchService] = useState<string>(ALL_SERVICES);
  const [searchSources, setSearchSources] = useState<SearchSource[]>([]);
  const [results, setResults] = useState<SearchLogRow[]>([]);
  const [nextCursor, setNextCursor] = useState<string>("");
  const [searchError, setSearchError] = useState<string>("");
  const [searching, setSearching] = useState(false);
  const [hasSearched, setHasSearched] = useState(false);

  const serviceNames = useServiceNames(name);

  // 重连续读的续读点：最新一条已累积日志（appendEntries 内更新，见下）。
  const lastSeenRef = useRef<LogEntryView | null>(null);

  // 去重键的会话级存储（M9-3）：键集合 + 与 entries 窗口同序的键环。键随
  // 窗口滑动同步淘汰——语义与"每次从窗口重建"等价但 O(1) 摊销（修复前：
  // 每次 appendEntries 重建 2000 键的 Set，高频流下每行 O(cap)）。
  const seenKeysRef = useRef<Set<string>>(new Set());
  const keyRingRef = useRef<string[]>([]);

  /** 全量替换 entries（换源/检索）时同步重建去重窗口。 */
  const resetSeenKeys = useCallback((windowed: StampedLogEntry[]) => {
    const seen = new Set<string>();
    const ring: string[] = [];
    for (const e of windowed) {
      const k = entryKey(e);
      if (seen.has(k)) continue;
      seen.add(k);
      ring.push(k);
    }
    seenKeysRef.current = seen;
    keyRingRef.current = ring;
  }, []);

  // 只负责合并去重：source 过滤是视图语义，归 visible（存储层不丢弃，
  // 否则实时流闭包里的旧 source 会永久吞掉新 source 的行，见 M8-3）。
  // 去重/盖章在 setState updater 外做（StrictMode 下 updater 可能双调，
  // ref 变异不得放进 updater）。
  const appendEntries = useCallback((incoming: LogEntryView[]) => {
    if (incoming.length === 0) return;
    const seen = seenKeysRef.current;
    const ring = keyRingRef.current;
    const fresh: StampedLogEntry[] = [];
    for (const e of incoming) {
      const stamped = stampSeq(e);
      const k = entryKey(stamped);
      if (seen.has(k)) continue;
      seen.add(k);
      ring.push(k);
      fresh.push(stamped);
    }
    if (fresh.length === 0) return;
    lastSeenRef.current = fresh[fresh.length - 1];
    // 键环随 entries 窗口同步淘汰最老的键（Set 与窗口严格一致）。
    const overflow = ring.length - LIVE_CAP;
    if (overflow > 0) {
      for (const k of ring.splice(0, overflow)) seen.delete(k);
    }
    setEntries((prev) => [...prev, ...fresh].slice(-LIVE_CAP));
  }, []);

  // 微批缓冲（M9-3）：缓冲与计时器是组件级 ref（跨流重连存活）。换源
  // （name/service）路径同步丢弃，卸载/重开流时先 flush。
  const pendingRef = useRef<LogEntryView[]>([]);
  const flushTimerRef = useRef<number | null>(null);

  const flushPending = useCallback(() => {
    if (flushTimerRef.current !== null) {
      window.clearTimeout(flushTimerRef.current);
      flushTimerRef.current = null;
    }
    const batch = pendingRef.current;
    pendingRef.current = [];
    appendEntries(batch);
  }, [appendEntries]);

  const bufferEntry = useCallback(
    (e: LogEntryView) => {
      pendingRef.current.push(e);
      if (flushTimerRef.current === null) {
        flushTimerRef.current = window.setTimeout(() => {
          flushTimerRef.current = null;
          flushPending();
        }, FLUSH_INTERVAL_MS);
      }
    },
    [flushPending],
  );

  // 跨应用导航重置（H5）：路由 /apps/:name/logs 在参数变化时复用同一组件
  // 实例，name 变化若不清空本地流状态，上一个应用的日志会混入当前应用
  // 视图（去重键 entryKey 不含 app 名，无法靠合并去重挡住）。声明在回填
  // effects 之前，保证 name 变化时先清空再回填新应用的历史。
  // （props 变化重置本地 state 是 React 认可的 effect 例外场景，见
  // react.dev/learn/you-might-not-need-an-effect#resetting-all-state-when-a-prop-changes。）
  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setEntries([]);
    lastSeenRef.current = null;
    setStreamError("");
    // 换应用：去重键窗口与未 flush 的缓冲一并丢弃（旧应用的行不得混入）。
    resetSeenKeys([]);
    pendingRef.current = [];
    if (flushTimerRef.current !== null) {
      window.clearTimeout(flushTimerRef.current);
      flushTimerRef.current = null;
    }
  }, [name, resetSeenKeys]);

  // 卸载时落盘缓冲（M9-3）：流重开/换源的 flush 在流 effect 清理里，这里
  // 兜住 paused 状态下卸载的形态。
  useEffect(() => () => flushPending(), [flushPending]);

  // 实时跟随：跟随流断开后回放历史补缺口，再重开跟随。重连延迟走共享
  // 指数退避（D4-③：1.5s 起 ×2、上限 30s、±20% 抖动；服务端连续立即正常
  // 关流时降频并提示）。
  useEffect(() => {
    if (!live) return;
    let conn: { close(): void } | null = null;
    let timer: number | undefined;
    let disposed = false;
    const backoff = new ReconnectBackoff();

    const open = () => {
      followLogs(name, service === ALL_SERVICES ? undefined : service, {
        onEntry: (entry) => {
          setStreamError("");
          bufferEntry(entry);
        },
        onEnd: (err) => {
          if (disposed) return;
          if (err instanceof StreamError && err.status === 401) {
            // 鉴权失效：全局登出已由流式层统一触发（stream.ts 的 401
            // 处置），此处仅展示并不再重连。
            setStreamError("stream rejected (401)");
            return;
          }
          const delay = backoff.nextDelayMs(err === undefined);
          setStreamError(
            `${err instanceof Error ? `${err.message} — reconnecting` : "stream ended — reconnecting"}${
              backoff.throttled ? " (server keeps closing the stream; retries slowed)" : ""
            }…`,
          );
          timer = window.setTimeout(() => {
            if (disposed) return;
            const last = lastSeenRef.current;
            const backfill = last?.at
              ? listHistoryLogs(name, {
                  service: service === ALL_SERVICES ? undefined : service,
                  since: last.at,
                })
                  // 切 app/卸载竞态下不回填（与初始回填的 cancelled 守卫
                  // 对齐），避免旧应用日志进新应用视图（M8-8）。
                  .then((r) => {
                    if (!disposed) appendEntries(r.entries ?? []);
                  })
                  .catch(() => undefined)
              : Promise.resolve();
            void backfill.then(() => {
              if (!disposed) open();
            });
          }, delay);
        },
      })
        .then((c) => {
          if (disposed) c.close();
          else {
            conn = c;
            backoff.markOpen();
          }
        });
      // openNdjsonStream 恒 resolve（连接级错误一律经 onEnd 回调上抛），
      // 重连的唯一路径是上面的 onEnd 分支——无需 .catch 兜底（M9-13）。
    };
    open();

    return () => {
      disposed = true;
      if (timer !== undefined) window.clearTimeout(timer);
      conn?.close();
      // 卸载/重开流前先落盘缓冲中的行（换 name 时随后的重置 effect 会
      // 清空；真卸载时 React 丢弃 setEntries，无副作用）。
      flushPending();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [live, name, service, bufferEntry, flushPending]);

  // 初始历史回填（最近 200 条），给跟随流一个上下文头部。
  useEffect(() => {
    let cancelled = false;
    listHistoryLogs(name, {
      service: service === ALL_SERVICES ? undefined : service,
      limit: 200,
    })
      .then((r) => {
        if (!cancelled) appendEntries(r.entries ?? []);
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [name, service]);

  // source 过滤的唯一位置（视图层）：存储保留全量，这里按当前 source 投影。
  const visible = useMemo(() => {
    return entries.filter((e) => {
      if (source === "container" && e.source === "build") return false;
      if (source === "build" && e.source === "container") return false;
      return true;
    });
  }, [entries, source]);

  // 自动滚动到底部。
  const scrollRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (autoScroll && scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
    }
  }, [visible, autoScroll]);

  const loadHistory = useCallback(() => {
    listHistoryLogs(name, {
      service: service === ALL_SERVICES ? undefined : service,
      since: since ? new Date(since).toISOString() : undefined,
      until: until ? new Date(until).toISOString() : undefined,
      limit: 500,
      source: source === "all" ? "" : source,
    })
      .then((r) => {
        // 检索结果是全量替换（非合并）：entries 与去重键窗口同步重建。
        const windowed = (r.entries ?? []).slice(-LIVE_CAP).map(stampSeq);
        resetSeenKeys(windowed);
        setEntries(windowed);
      })
      .catch((err) =>
        setStreamError(err instanceof Error ? err.message : String(err)),
      );
  }, [name, service, since, until, source, resetSeenKeys]);

  // ── 检索态（SearchLogs，W5-S2）─────────────────────────────────────────

  // SearchLogs 的 app 流选择器（W2-S4 起：非平台管理员的用户凭据必须三段
  // 限定形 team/prj/app——服务端硬校验）。Console 按当前团队/项目上下文
  // 构造限定形（顶栏切换器）；无上下文时透传裸名，服务端 400 带指引。
  const { qualifyApp } = useProjectContext();
  const runSearch = useCallback(
    (cursor?: string) => {
      setSearching(true);
      setHasSearched(true);
      const win = SEARCH_WINDOWS.find((w) => w.key === windowKey) ?? {
        key: "1h",
        label: "1h",
        ms: 60 * 60_000,
      };
      searchLogs(qualifyApp(name), {
        keyword: keyword.trim() || undefined,
        services: searchService === ALL_SERVICES ? undefined : [searchService],
        sources: searchSources.length > 0 ? searchSources : undefined,
        since: new Date(Date.now() - win.ms).toISOString(),
        limit: SEARCH_PAGE_LIMIT,
        cursor: cursor || undefined,
      })
        .then((r) => {
          setResults((prev) => (cursor ? [...prev, ...(r.rows ?? [])] : r.rows ?? []));
          setNextCursor(r.next_cursor ?? "");
          setSearchError("");
        })
        .catch((err) => {
          const env = errorEnvelopeFrom(err);
          // 诚实错误态：E_LOGS_BACKEND_UNAVAILABLE（jsonl 模式 / VL 不可达）
          // 引导 `fleetly logs backend`——不冒充空结果。
          setSearchError(
            [
              env.code ? `${env.code}: ${env.message ?? ""}` : env.message || "search failed",
              env.suggestion,
              "Inspect the log backend with `fleetly logs backend show`; the log-store search requires logs.backend=victorialogs (live tail is unaffected).",
            ]
              .filter(Boolean)
              .join(" — "),
          );
        })
        .finally(() => setSearching(false));
    },
    [name, keyword, windowKey, searchService, searchSources, qualifyApp],
  );

  const loadMore = useCallback(() => {
    if (!nextCursor) return;
    runSearch(nextCursor);
  }, [nextCursor, runSearch]);

  const toggleSource = useCallback((s: SearchSource) => {
    setSearchSources((prev) =>
      prev.includes(s) ? prev.filter((x) => x !== s) : [...prev, s],
    );
  }, []);

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader className="flex-row items-center justify-between space-y-0 border-b pb-3">
          <CardTitle className="flex items-center gap-2 text-sm font-semibold">
            <Terminal aria-hidden className="h-4 w-4 text-muted-foreground" />
            Logs
          </CardTitle>
          <div className="flex items-center gap-3">
            {/* 两态切换（W5-S2）：默认直播；Search 进入日志库统一检索。 */}
            <div className="flex overflow-hidden rounded-md border" role="group" aria-label="Logs view mode">
              <Button
                variant={mode === "live" ? "secondary" : "ghost"}
                size="sm"
                className="rounded-none border-0"
                onClick={() => setMode("live")}
              >
                Live
              </Button>
              <Button
                data-testid="logs-search-toggle"
                variant={mode === "search" ? "secondary" : "ghost"}
                size="sm"
                className="rounded-none border-0"
                onClick={() => setMode("search")}
              >
                <Search aria-hidden className="h-3.5 w-3.5" />
                Search
              </Button>
            </div>
            {mode === "live" ? (
              streamError ? (
                <span className="text-xs text-amber-600 dark:text-amber-400">{streamError}</span>
              ) : (
                <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
                  <span
                    aria-hidden
                    className={`h-2 w-2 rounded-full ${
                      live ? "bg-emerald-500 animate-pulse" : "bg-zinc-400"
                    }`}
                  />
                  {live ? "following" : "paused"}
                </span>
              )
            ) : null}
          </div>
        </CardHeader>
        {mode === "live" ? (
          <CardContent className="space-y-3 pt-4">
          <div className="flex flex-wrap items-end gap-3">
            <div className="w-48 space-y-1.5">
              <Label htmlFor="log-service">Service</Label>
              <Select
                value={service}
                onValueChange={(v) => {
                  setService(v);
                  setEntries([]);
                  // 换服务：去重键窗口与未 flush 的缓冲同步丢弃
                  //（旧服务的行不得混入新服务视图）。
                  resetSeenKeys([]);
                  pendingRef.current = [];
                }}
              >
                <SelectTrigger id="log-service">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={ALL_SERVICES}>All services</SelectItem>
                  {serviceNames.map((s) => (
                    <SelectItem key={s} value={s}>
                      {s}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="w-40 space-y-1.5">
              <Label htmlFor="log-source">Source</Label>
              <Select
                value={source}
                onValueChange={(v) => setSource(v as typeof source)}
              >
                <SelectTrigger id="log-source">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="all">All</SelectItem>
                  <SelectItem value="container">container</SelectItem>
                  <SelectItem value="build">build</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <Button variant="outline" size="sm" onClick={() => setLive((v) => !v)}>
              {live ? (
                <>
                  <Pause aria-hidden className="h-3.5 w-3.5" /> Pause follow
                </>
              ) : (
                <>
                  <Play aria-hidden className="h-3.5 w-3.5" /> Resume follow
                </>
              )}
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setAutoScroll((v) => !v)}
            >
              <ScrollText aria-hidden className="h-3.5 w-3.5" />
              {autoScroll ? "Auto-scroll on" : "Auto-scroll off"}
            </Button>
          </div>

          <div
            ref={scrollRef}
            data-testid="log-stream"
            className="h-[440px] overflow-auto rounded-lg border border-zinc-800 bg-zinc-950 p-3 font-mono text-xs leading-5 text-zinc-100"
          >
            {visible.length === 0 ? (
              <p className="text-zinc-500">
                No entries yet. Waiting for logs…
              </p>
            ) : (
              visible.map((e) => (
                <div key={entryKey(e)} className="whitespace-pre-wrap break-all">
                  <span className="text-zinc-500">
                    {e.at ? formatTime(e.at) : "—"}{" "}
                  </span>
                  <span className="text-sky-400">{e.service}</span>
                  {e.source === "build" ? (
                    <span className="text-amber-400"> [build] </span>
                  ) : (
                    " "
                  )}
                  {e.stderr ? (
                    <span className="text-red-400">{e.line}</span>
                  ) : (
                    e.line
                  )}
                </div>
              ))
            )}
          </div>
        </CardContent>
        ) : (
        <CardContent className="space-y-3 pt-4">
          {/* 检索态（W5-S2 设计 §3.3）：关键词 + 时间窗快捷项 + 服务 +
              来源 chips → SearchLogs；结果时间倒序（VL 原生序）+ 游标加载
              更多；VL 不可达/jsonl 模式 = 诚实错误态。 */}
          <div className="flex flex-wrap items-end gap-3">
            <div className="min-w-64 flex-1 space-y-1.5">
              <Label htmlFor="logs-search-input">Keyword</Label>
              <Input
                id="logs-search-input"
                data-testid="logs-search-input"
                placeholder="Search as a literal phrase…"
                value={keyword}
                onChange={(e) => setKeyword(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && !searching) runSearch();
                }}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="logs-search-window">Window</Label>
              <div className="flex overflow-hidden rounded-md border" role="group" aria-label="Search time window">
                {SEARCH_WINDOWS.map((w) => (
                  <Button
                    key={w.key}
                    data-testid="logs-search-window"
                    data-window={w.key}
                    variant={windowKey === w.key ? "secondary" : "ghost"}
                    size="sm"
                    className="rounded-none border-0"
                    onClick={() => setWindowKey(w.key)}
                  >
                    {w.label}
                  </Button>
                ))}
              </div>
            </div>
            <div className="w-48 space-y-1.5">
              <Label htmlFor="logs-search-service">Service</Label>
              <Select
                value={searchService}
                onValueChange={setSearchService}
              >
                <SelectTrigger id="logs-search-service">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={ALL_SERVICES}>All services</SelectItem>
                  {serviceNames.map((s) => (
                    <SelectItem key={s} value={s}>
                      {s}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label>Source</Label>
              <div className="flex items-center gap-1.5">
                {(["container", "build", "access"] as SearchSource[]).map((s) => (
                  <button
                    key={s}
                    type="button"
                    data-testid={`logs-search-source-${s}`}
                    aria-pressed={searchSources.includes(s)}
                    onClick={() => toggleSource(s)}
                    className={`rounded-full border px-2.5 py-1 text-xs font-medium transition-colors ${
                      searchSources.includes(s)
                        ? "border-sky-500 bg-sky-500/10 text-sky-600 dark:text-sky-400"
                        : "text-muted-foreground hover:bg-muted/60"
                    }`}
                  >
                    {s}
                  </button>
                ))}
              </div>
            </div>
            <Button
              data-testid="logs-search-submit"
              size="sm"
              disabled={searching}
              onClick={() => runSearch()}
            >
              <Search aria-hidden className="h-3.5 w-3.5" />
              {searching ? "Searching…" : "Search"}
            </Button>
          </div>

          {searchError ? (
            <div
              data-testid="logs-search-error"
              className="rounded-md border border-amber-500/50 bg-amber-500/10 px-3 py-2 text-xs text-amber-700 dark:text-amber-400"
            >
              {searchError}
            </div>
          ) : null}

          <div
            data-testid="logs-search-results"
            className="h-[440px] overflow-auto rounded-lg border border-zinc-800 bg-zinc-950 p-3 font-mono text-xs leading-5 text-zinc-100"
          >
            {results.length === 0 && !searchError ? (
              <p className="text-zinc-500">
                {hasSearched
                  ? "No matches in the searched window."
                  : "No matches yet. Run a search over the log store…"}
              </p>
            ) : (
              results.map((r, i) => (
                <div
                  key={`${r.at ?? ""}|${r.service}|${r.source}|${r.msg}|${i}`}
                  className="whitespace-pre-wrap break-all"
                >
                  <span className="text-amber-300">
                    {r.at ? formatTime(r.at) : "—"}{" "}
                  </span>
                  <span className="text-sky-400">{r.service}</span>
                  {r.source ? (
                    <span className="text-violet-400"> [{r.source}] </span>
                  ) : (
                    " "
                  )}
                  {r.msg}
                  {r.source === "access" && r.fields ? (
                    <span className="text-zinc-500">
                      {" "}
                      {"{"}
                      {Object.entries(r.fields)
                        .map(([k, v]) => `${k}=${v}`)
                        .join(" ")}
                      {"}"}
                    </span>
                  ) : null}
                </div>
              ))
            )}
          </div>
          {nextCursor ? (
            <Button
              data-testid="logs-search-load-more"
              variant="outline"
              size="sm"
              disabled={searching}
              onClick={loadMore}
            >
              Load more
            </Button>
          ) : null}
        </CardContent>
        )}
      </Card>

      {mode === "live" ? (
      <Card>
        <CardHeader className="border-b pb-3">
          <CardTitle className="text-sm font-semibold">History search</CardTitle>
        </CardHeader>
        <CardContent className="pt-4">
          <div className="flex flex-wrap items-end gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="log-since">Since</Label>
              <Input
                id="log-since"
                type="datetime-local"
                value={since}
                onChange={(e) => setSince(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="log-until">Until</Label>
              <Input
                id="log-until"
                type="datetime-local"
                value={until}
                onChange={(e) => setUntil(e.target.value)}
              />
            </div>
            <Button variant="outline" size="sm" onClick={loadHistory}>
              <RefreshCw aria-hidden className="h-3.5 w-3.5" />
              Search history
            </Button>
          </div>
        </CardContent>
      </Card>
      ) : null}
    </div>
  );
}
