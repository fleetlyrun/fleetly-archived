// Deploy 卡（2026-09-29 三轮重设计 §4.7，对齐 dokploy General 页签的
// Provider 形态）：单一卡 + 方式切换 pills（Compose/Git/Webhook——每应用在
// localStorage 记住当前方式，一次只呈现一种）。原挂在 Deployments 页，
// 部署入口上移到 Overview 首页签后随迁为独立组件；部署历史页只留 history
// 与重部署 webhook URL 行。
//
// 方式与角色门（前端体验门，§3.2）：
//   Compose  = deploy scope（developer+）——一次性手动部署；
//   Git      = admin scope（admin+）——push 远端 + 拉源配置（整体替换）；
//   Webhook  = admin scope（admin+）——接收端 URL + 签名密钥。
// 平台管理员资源面恒只读（P0-3 双门）——能力门在 lib/context 统一关门，
// methods 投影为空 → 整卡不渲染；只读说明由宿主页（Deploy settings 卡）承载。

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Check,
  CheckCircle2,
  Copy,
  FileUp,
  GitBranch,
  Loader2,
  Rocket,
} from "lucide-react";
import { useRef, useState, type ChangeEvent, type FormEvent, type ReactNode } from "react";
import { Link } from "react-router-dom";

import { apiBase } from "@/api/client";
import {
  deploy,
  getApp,
  getDeployment,
  setAppSource,
  setAppWebhookSecret,
  showAppWebhook,
} from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import type { ComposeWarning } from "@/api/types";
import { EnvelopeAlert } from "@/components/envelope-alert";
import { PillTabs } from "@/components/pill-tabs";
import { SectionCard } from "@/components/section-card";
import { StateBadge } from "@/components/state-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { useProjectContext, useTeamCapabilities } from "@/lib/context";

/** 终态集：之外的状态轮询跟踪。部署域两处消费（本卡跟踪 + 历史页轮询/
    批量取消/时长判定），单点导出防词表漂移。 */
export const TERMINAL = new Set(["succeeded", "failed", "cancelled"]);

// ── 部署方式选择（每应用记忆；一次只呈现一种方式）────────────────────────

type DeployMethod = "compose" | "git" | "webhook";

const METHOD_KEY_PREFIX = "fleetly.console.deploy-method.";

function loadStoredMethod(app: string): DeployMethod | null {
  try {
    const raw = window.localStorage.getItem(METHOD_KEY_PREFIX + app);
    return raw === "compose" || raw === "git" || raw === "webhook" ? raw : null;
  } catch {
    return null;
  }
}

function storeMethod(app: string, method: DeployMethod) {
  try {
    window.localStorage.setItem(METHOD_KEY_PREFIX + app, method);
  } catch {
    // 存储不可用（隐私模式等）——方式选择退化为会话内 state，不阻断。
  }
}

const METHOD_LABELS: Record<DeployMethod, string> = {
  compose: "Compose",
  git: "Git",
  webhook: "Webhook",
};

export function DeployCard({ app }: { app: string }) {
  // 项目归属上下文（2026-09-25 走查 + 2026-09-26 W2-4 二修）：优先用应用
  // 自身归属（AppView.team_slug/project_slug——详情壳 ["app", name] 查询
  // 共享缓存，本卡不再额外发请求；v0.3 归属模型下该字段恒在），顶栏项目
  // 上下文仅作详情数据未达时的缺省回退——此前只看顶栏：All projects 下重
  // 部署既有应用被禁用（多团队用户），而应用归属明明已知（隐式全局状态
  // 依赖是缺口本身）。未解析出归属时保持旧门：多团队用户禁用+指路。
  const { teams, projectRef } = useProjectContext();
  const { canDeploy, canAdminResources } = useTeamCapabilities();
  const appQuery = useQuery({
    queryKey: ["app", app],
    queryFn: () => getApp(app),
    staleTime: 5_000,
    retry: false,
  });
  const ownProjectRef =
    appQuery.data?.team_slug && appQuery.data?.project_slug
      ? `${appQuery.data.team_slug}/${appQuery.data.project_slug}`
      : "";
  const effectiveProjectRef = ownProjectRef || projectRef;
  const needsProjectPick = teams.length > 1 && !effectiveProjectRef;

  // 可用方式 = 角色门投影：compose=deploy（developer+）；git/webhook=admin
  //（ShowAppWebhook/SetAppSource/SetAppWebhookSecret 三 RPC 均 admin scope）。
  const methods: DeployMethod[] = [];
  if (canDeploy) methods.push("compose");
  if (canAdminResources) methods.push("git", "webhook");
  const [method, setMethod] = useState<DeployMethod>(() => {
    const stored = loadStoredMethod(app);
    return stored && methods.includes(stored) ? stored : (methods[0] ?? "compose");
  });

  function switchMethod(next: string) {
    const m = next as DeployMethod;
    setMethod(m);
    storeMethod(app, m);
  }

  // 触发面读数据（git/webhook 两 pane 共用）：只在 admin+ 且当前方式需要
  // 它时拉取（developer 挂载本卡时不发注定 403 的请求）。
  const webhookQuery = useQuery({
    queryKey: ["app-webhook", app],
    queryFn: () => showAppWebhook(app),
    enabled: canAdminResources && method !== "compose",
  });
  const readError = webhookQuery.isError
    ? errorEnvelopeFrom(webhookQuery.error)
    : null;

  const descriptions: Record<DeployMethod, ReactNode> = {
    compose:
      "Paste or upload a compose file and deploy it now — one-shot from the Console.",
    git: "Push the configured branch to the remote; the app redeploys with zero downtime.",
    webhook: "Trigger deploys from GitHub/Gitea webhooks or CI jobs.",
  };

  if (methods.length === 0) return null;

  return (
    <SectionCard
      icon={Rocket}
      title="Deploy"
      description={descriptions[method]}
      contentClassName="space-y-4 pt-4"
      testId="deploy-card"
      actions={
        <PillTabs
          ariaLabel="Deploy method"
          value={method}
          onValueChange={switchMethod}
          items={methods.map((m) => ({ key: m, label: METHOD_LABELS[m] }))}
        />
      }
    >
      {readError && method !== "compose" ? (
        <EnvelopeAlert
          code={readError.code}
          message={readError.message}
          suggestion={readError.suggestion}
          docs={readError.docs}
        />
      ) : null}

      {method === "compose" ? (
        <ComposePane
          app={app}
          effectiveProjectRef={effectiveProjectRef}
          needsProjectPick={needsProjectPick}
        />
      ) : null}
      {method === "git" ? <GitPane app={app} cfg={webhookQuery.data} /> : null}
      {method === "webhook" ? (
        <WebhookPane app={app} cfg={webhookQuery.data} cfgReady={webhookQuery.isSuccess} />
      ) : null}

      {!canAdminResources ? (
        <p
          className="text-xs text-muted-foreground"
          data-testid="triggers-admin-note"
        >
          Deploy trigger settings (git push, webhooks) are visible to team
          admins only. Ask a team admin for access.
        </p>
      ) : null}
    </SectionCard>
  );
}

// ── Compose pane（一次性手动部署）────────────────────────────────────────

function ComposePane({
  app,
  effectiveProjectRef,
  needsProjectPick,
}: {
  app: string;
  effectiveProjectRef: string;
  needsProjectPick: boolean;
}) {
  const queryClient = useQueryClient();
  const [composeText, setComposeText] = useState("");
  const [trackedId, setTrackedId] = useState("");
  // ComposeWarning 形状跟随生成类型（D4-②：手写 {field,warning} 与 proto
  // {kind,code,service,message} 漂移，已修）。
  const [warnings, setWarnings] = useState<ComposeWarning[]>([]);
  const fileRef = useRef<HTMLInputElement>(null);

  const deployMutation = useMutation({
    mutationFn: () =>
      deploy(app, composeText, effectiveProjectRef ? { project: effectiveProjectRef } : {}),
    onSuccess: (resp) => {
      setTrackedId(resp.deployment_id ?? "");
      setWarnings(resp.warnings ?? []);
      setComposeText("");
      void queryClient.invalidateQueries({ queryKey: ["deployments", app] });
      void queryClient.invalidateQueries({ queryKey: ["apps"] });
      void queryClient.invalidateQueries({ queryKey: ["app", app] });
    },
  });

  // 部署跟踪：入队后轮询该部署直至终态（与 CLI wait 同语义，2s 周期）。
  const tracked = useQuery({
    queryKey: ["deployment", trackedId],
    queryFn: () => getDeployment(trackedId),
    enabled: trackedId !== "",
    // 空数据形态（查询失败/未返回）下 data 可能缺 deployment 成员——
    // 可选链守卫避免 refetchInterval 回调内 TypeError（M9-7）。
    refetchInterval: (q) =>
      TERMINAL.has(q.state.data?.deployment?.status ?? "") ? false : 2000,
  });
  const trackedDeployment = tracked.data?.deployment;
  // 跟踪轮询失败的展示（M9-5）：持续失败不再永远转圈——错误信封一等渲染。
  const trackedError = tracked.isError
    ? errorEnvelopeFrom(tracked.error)
    : deployMutation.isError
      ? errorEnvelopeFrom(deployMutation.error)
      : null;

  function onFile(e: ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0];
    if (!file) return;
    void file.text().then(setComposeText);
    e.target.value = "";
  }

  function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (!composeText.trim()) return;
    deployMutation.mutate();
  }

  return (
    <div className="space-y-3">
      <form className="space-y-3" onSubmit={onSubmit}>
        <Textarea
          aria-label="Compose YAML"
          className="min-h-[180px] font-mono text-xs"
          placeholder={"services:\n  web:\n    image: nginx:1.27-alpine\n    ports:\n      - 8080:80"}
          value={composeText}
          onChange={(e) => setComposeText(e.target.value)}
        />
        <div className="flex items-center gap-2">
          <Button
            type="submit"
            disabled={
              !composeText.trim() || deployMutation.isPending || needsProjectPick
            }
          >
            {deployMutation.isPending ? (
              <Loader2 aria-hidden className="h-4 w-4 animate-spin" />
            ) : null}
            Deploy
          </Button>
          <Button
            type="button"
            variant="outline"
            onClick={() => fileRef.current?.click()}
          >
            <FileUp aria-hidden className="h-4 w-4" />
            Upload file
          </Button>
          <input
            ref={fileRef}
            type="file"
            accept=".yaml,.yml,.json,application/yaml,text/yaml"
            className="hidden"
            onChange={onFile}
          />
        </div>
        {needsProjectPick ? (
          <p
            className="text-xs text-muted-foreground"
            data-testid="deploy-project-context-hint"
          >
            Select a project above to deploy (the app&apos;s own project is
            used automatically once resolved).
          </p>
        ) : null}
      </form>

      {trackedError ? (
        <EnvelopeAlert
          code={trackedError.code}
          message={trackedError.message}
          suggestion={trackedError.suggestion}
          docs={trackedError.docs}
        />
      ) : null}

      {warnings.length > 0 ? (
        <div className="rounded-md border border-amber-500/30 bg-amber-500/5 p-3 text-sm text-amber-800 dark:text-amber-300">
          <div className="font-semibold">Compose warnings</div>
          <ul className="mt-1 list-disc pl-4">
            {warnings.map((w, i) => (
              <li key={i}>
                <code className="text-xs">{w.code || w.kind}</code>
                {w.service ? ` (${w.service})` : ""}: {w.message}
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      {trackedId ? (
        <div className="rounded-md border p-3 text-sm" data-testid="deployment-tracker">
          <div className="flex items-center gap-2">
            <span className="text-muted-foreground">Deployment</span>
            <code className="font-mono text-xs">{trackedId}</code>
            {trackedDeployment ? (
              <>
                <StateBadge state={
                  trackedDeployment.phase === "blocked_waiting"
                    ? "blocked_waiting"
                    : trackedDeployment.status ?? ""
                } />
                {trackedDeployment.error_code ? (
                  <span className="font-mono text-xs text-red-600 dark:text-red-400">
                    {trackedDeployment.error_code}
                  </span>
                ) : null}
              </>
            ) : tracked.isError ? null : (
              <Loader2 aria-hidden className="h-4 w-4 animate-spin" />
            )}
          </div>
          {trackedDeployment?.verdict ? (
            <p className="mt-1 text-muted-foreground">{trackedDeployment.verdict}</p>
          ) : null}
          {trackedDeployment && TERMINAL.has(trackedDeployment.status ?? "") && trackedDeployment.status === "succeeded" ? (
            <p className="mt-1 flex items-center gap-1 text-emerald-600 dark:text-emerald-400">
              <CheckCircle2 aria-hidden className="h-4 w-4" /> Deployed
            </p>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

// ── Git pane（push 远端 + 拉源配置）──────────────────────────────────────
// 展示全部取自 ShowAppWebhook 响应字段：git_remote_hint（push 远端，git SSH
// 面未启用时为空串）、source_url/branch/auth_kind。源配置是整体替换语义
//（读态到达时一次性水合，避免空表单提交静默清掉既有 source）。

/** 拉源认证形态词表（proto SetAppSourceRequest.source_auth_kind in 约束）。 */
type SourceAuthKind = "none" | "https_token" | "ssh_key";

function GitPane({
  app,
  cfg,
}: {
  app: string;
  cfg: Awaited<ReturnType<typeof showAppWebhook>> | undefined;
}) {
  const queryClient = useQueryClient();
  // 拉源表单（整体替换语义——读态到达时一次性水合；渲染期派生模式，不经
  // effect）。
  const [hydratedApp, setHydratedApp] = useState("");
  const [sourceUrl, setSourceUrl] = useState("");
  const [sourceBranch, setSourceBranch] = useState("main");
  const [authKind, setAuthKind] = useState<SourceAuthKind>("none");
  const [authSecret, setAuthSecret] = useState("");
  const [sourceStored, setSourceStored] = useState(false);
  const [error, setError] = useState<ReturnType<typeof errorEnvelopeFrom> | null>(null);

  if (cfg && hydratedApp !== app) {
    setHydratedApp(app);
    setSourceUrl(cfg.source_url ?? "");
    setSourceBranch(cfg.source_branch || "main");
    setAuthKind((cfg.source_auth_kind || "none") as SourceAuthKind);
  }

  const sourceMutation = useMutation({
    mutationFn: () =>
      setAppSource(app, {
        source_url: sourceUrl.trim(),
        source_branch: sourceBranch.trim() || "main",
        source_auth_kind: authKind,
        ...(authKind === "none" ? {} : { source_auth_secret: authSecret }),
      }),
    onSuccess: () => {
      setSourceStored(true);
      setAuthSecret("");
      setError(null);
      void queryClient.invalidateQueries({ queryKey: ["app-webhook", app] });
    },
    onError: (err) => setError(errorEnvelopeFrom(err)),
  });

  const sourceReady =
    sourceBranch.trim() !== "" && (authKind === "none" || authSecret.length >= 16);

  function onSourceSubmit(e: FormEvent) {
    e.preventDefault();
    if (sourceReady) sourceMutation.mutate();
  }

  return (
    <div className="space-y-5" data-testid="deploy-git-pane">
      {/* push 通道：远端 + 触发分支 + deploy key 入口。 */}
      <div className="space-y-3">
        <div className="flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
          <GitBranch aria-hidden className="h-3.5 w-3.5" />
          Git push
        </div>
        <CopyValueRow
          label="Push remote"
          value={cfg?.git_remote_hint ?? ""}
          testid="triggers-git-remote"
          empty="Unavailable — the git SSH face is not enabled on this server."
          hint="Pushing to this remote deploys the configured branch with zero downtime."
        />
        <div className="text-xs text-muted-foreground" data-testid="triggers-branch">
          Trigger branch: <code className="font-mono">{cfg?.source_branch || "main"}</code>
          {" · "}register a deploy key on the{" "}
          <Link to="/git-keys" className="font-medium underline underline-offset-2">
            Git push keys
          </Link>{" "}
          page.
        </div>
      </div>

      {/* 拉源配置（触发通道取代码的 remote；整体替换语义）。 */}
      <div className="space-y-3 border-t pt-4">
        <div className="flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
          <Rocket aria-hidden className="h-3.5 w-3.5" />
          Fetch source
        </div>
        <div className="text-xs text-muted-foreground" data-testid="triggers-source-state">
          {cfg === undefined ? null : cfg.source_url ? (
            <>
              Current: <code className="font-mono">{cfg.source_url}</code> (auth:{" "}
              {cfg.source_auth_kind || "none"})
            </>
          ) : (
            "No fetch source set — webhook deliveries deploy the pushed ref without a fetch."
          )}
        </div>
        <form className="space-y-3" onSubmit={onSourceSubmit}>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1.5 sm:col-span-2">
              <Label htmlFor="source-url-input">Source URL</Label>
              <Input
                id="source-url-input"
                data-testid="source-url-input"
                className="font-mono text-xs"
                placeholder="https://git.example.com/team/app.git (or file://)"
                value={sourceUrl}
                onChange={(e) => setSourceUrl(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="source-branch-input">Branch</Label>
              <Input
                id="source-branch-input"
                data-testid="source-branch-input"
                value={sourceBranch}
                onChange={(e) => setSourceBranch(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <Label>Auth</Label>
              <Select value={authKind} onValueChange={(v) => setAuthKind(v as SourceAuthKind)}>
                <SelectTrigger data-testid="source-auth-kind">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">none (public source)</SelectItem>
                  <SelectItem value="https_token">https_token</SelectItem>
                  <SelectItem value="ssh_key">ssh_key</SelectItem>
                </SelectContent>
              </Select>
            </div>
            {authKind !== "none" ? (
              <div className="space-y-1.5 sm:col-span-2">
                <Label htmlFor="source-auth-secret-input">
                  Auth material ({authKind === "https_token" ? "token" : "PEM private key"})
                </Label>
                <Input
                  id="source-auth-secret-input"
                  type="password"
                  data-testid="source-auth-secret-input"
                  className="font-mono text-xs"
                  placeholder="At least 16 characters"
                  value={authSecret}
                  onChange={(e) => setAuthSecret(e.target.value)}
                />
                <p className="text-xs text-muted-foreground">
                  Encrypted at rest, never read back. Whole-replacement
                  semantics: every save writes URL, branch and auth together,
                  so the material must be re-entered on each change.
                  {authKind === "https_token" ? " https_token requires an https:// source URL." : ""}
                </p>
              </div>
            ) : null}
          </div>
          <div className="flex items-center gap-3">
            <Button
              type="submit"
              variant="outline"
              data-testid="source-submit"
              disabled={!sourceReady || sourceMutation.isPending}
            >
              {sourceMutation.isPending ? (
                <Loader2 aria-hidden className="h-4 w-4 animate-spin" />
              ) : null}
              Save source
            </Button>
            <span className="text-xs text-muted-foreground">
              Save with an empty URL and auth &quot;none&quot; to clear the fetch source.
            </span>
            {sourceStored ? (
              <span className="text-sm text-emerald-600 dark:text-emerald-400" data-testid="source-stored">
                Source saved.
              </span>
            ) : null}
          </div>
        </form>
      </div>

      {error ? (
        <EnvelopeAlert code={error.code} message={error.message} suggestion={error.suggestion} docs={error.docs} />
      ) : null}
    </div>
  );
}

// ── Webhook pane（接收端 + 签名密钥）─────────────────────────────────────
// webhook 接收端 URL 不在响应内——按 gateway 既有路由
//（internal/runtime/gateway.go：POST /v1/apps/{app}/webhooks/github|gitea）
// 与 console apiBase 拼装，是既有服务端事实的展示，不是新契约面。
// 路径段必须用响应解析出的**业务名**（name 字段）：服务端分派正则
//（internal/gitserver/webhook.go WebhookPathPattern）只收
// [a-z0-9][a-z0-9-]{0,62} 且命中后按业务名查行——本页路由参数是平台 id
//（26 字符 ULID 含大写，永不匹配分派正则），拿它拼接收端 URL 是恒 404
// 死链（2026-09-25 复核修复）。业务名含下划线等出律字符的形态服务端
// 同样不收——pane 内如实注一行说明。

/** webhook 接收端绝对 URL（apiBase 缺省相对 /v1——以当前 origin 补全供
 * 复制）。app 参数必须是**业务名**：服务端接收端按业务名分派（见文件头
 * 注）——调用方传路由参数（平台 id）会拼出死链。部署历史页的重部署 URL
 * 行（dokploy Deployments 同构）同样消费本函数。 */
export function webhookReceiverUrl(app: string, forge: "github" | "gitea"): string {
  const base = new URL(apiBase(), window.location.origin).href.replace(/\/+$/, "");
  return `${base}/apps/${encodeURIComponent(app)}/webhooks/${forge}`;
}

/** 服务端接收端分派对 app 名的词形约束（WebhookPathPattern 第一捕获组）。 */
export const WEBHOOK_NAME_PATTERN = /^[a-z0-9][a-z0-9-]{0,62}$/;

function WebhookPane({
  app,
  cfg,
  cfgReady,
}: {
  app: string;
  cfg: Awaited<ReturnType<typeof showAppWebhook>> | undefined;
  cfgReady: boolean;
}) {
  const queryClient = useQueryClient();
  // secret 轮换（write-only：值只出现在请求体，成功后不回显明文——旧值即
  // 时失效：验签按库存值单点判定，state.SetAppWebhookSecret 直接覆盖）。
  const [secret, setSecret] = useState("");
  const [secretDialogOpen, setSecretDialogOpen] = useState(false);
  const [secretStored, setSecretStored] = useState(false);
  const [error, setError] = useState<ReturnType<typeof errorEnvelopeFrom> | null>(null);

  const secretMutation = useMutation({
    mutationFn: () => setAppWebhookSecret(app, secret),
    onSuccess: () => {
      setSecretDialogOpen(false);
      setSecret("");
      setSecretStored(true);
      setError(null);
      void queryClient.invalidateQueries({ queryKey: ["app-webhook", app] });
    },
    onError: (err) => setError(errorEnvelopeFrom(err)),
  });

  const secretReady = secret.length >= 16;
  const configured = cfg?.secret_configured === true;

  return (
    <div className="space-y-3" data-testid="deploy-webhook-pane">
      <CopyValueRow
        label="Receiver URL (GitHub)"
        value={cfg?.name ? webhookReceiverUrl(cfg.name, "github") : ""}
        testid="triggers-webhook-url"
        empty={cfgReady ? "Receiver URL unavailable." : "Loading trigger configuration…"}
        hint={
          <>
            Gitea variant: same path with <code className="font-mono">/gitea</code>. The
            path addresses the app by its name (not the id in the address bar). The
            receiver answers 404 until a signing secret is configured.
          </>
        }
      />
      {/* 服务端接收端只收 [a-z0-9-] 词形的应用名：出律名字（如下划线）
          的应用收不到 push/webhook 触发——如实披露，不静默给死链。 */}
      {cfg?.name && !WEBHOOK_NAME_PATTERN.test(cfg.name) ? (
        <p
          className="text-xs text-amber-800 dark:text-amber-300"
          data-testid="triggers-webhook-name-note"
        >
          This app&apos;s name contains characters outside [a-z0-9-]. The git push
          and webhook receiver paths only accept lowercase letters, digits and
          dashes, so push triggers are unavailable for this app (server-side
          limitation).
        </p>
      ) : null}
      <div className="flex flex-wrap items-center gap-2 text-sm" data-testid="triggers-secret-state">
        <span className="text-xs font-medium text-muted-foreground">Signing secret</span>
        {cfg === undefined ? null : configured ? (
          <Badge variant="secondary" data-testid="triggers-secret-configured">
            Configured (never displayed)
          </Badge>
        ) : (
          <span className="text-xs text-muted-foreground" data-testid="triggers-secret-missing">
            Not configured — the webhook endpoint is disabled.
          </span>
        )}
      </div>
      <div className="flex flex-wrap items-end gap-2">
        <div className="space-y-1.5">
          <Label htmlFor="webhook-secret-input">New secret</Label>
          <Input
            id="webhook-secret-input"
            type="password"
            data-testid="triggers-secret-input"
            className="w-72 font-mono text-xs"
            placeholder="At least 16 characters"
            value={secret}
            onChange={(e) => setSecret(e.target.value)}
          />
        </div>
        <Button
          type="button"
          variant="outline"
          data-testid="triggers-secret-open"
          disabled={secret.length < 16}
          onClick={() => setSecretDialogOpen(true)}
        >
          Set secret
        </Button>
      </div>
      {secretStored ? (
        <p className="text-sm text-emerald-600 dark:text-emerald-400" data-testid="triggers-secret-stored">
          Webhook secret stored (encrypted at rest; it is never displayed again).
        </p>
      ) : null}
      {error ? (
        <EnvelopeAlert code={error.code} message={error.message} suggestion={error.suggestion} docs={error.docs} />
      ) : null}

      {/* 轮换两步确认：旧 secret 即时失效（验签按库存值单点判定——投递方
          同步换新，否则投递 401/拒绝）；值永不回显。 */}
      <Dialog open={secretDialogOpen} onOpenChange={setSecretDialogOpen}>
        <DialogContent data-testid="triggers-secret-dialog">
          <DialogHeader>
            <DialogTitle>Set webhook signing secret</DialogTitle>
            <DialogDescription>
              The new secret takes effect immediately: deliveries signed with
              the old secret fail verification — signature checking is the
              webhook endpoint&apos;s only authentication. Update your forge
              with the same value. The secret is stored encrypted and never
              displayed again.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              variant="outline"
              size="sm"
              data-testid="triggers-secret-cancel"
              onClick={() => setSecretDialogOpen(false)}
            >
              Cancel
            </Button>
            <Button
              size="sm"
              data-testid="triggers-secret-submit"
              disabled={!secretReady || secretMutation.isPending}
              onClick={() => secretMutation.mutate()}
            >
              {secretMutation.isPending ? "Setting…" : "Set secret"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

/** 读态行：code 值 + 复制（System 页指纹卡同款形态）；空值诚实说明。
    部署历史页的重部署 URL 行同样复用（dokploy Deployments 同构）。 */
export function CopyValueRow({
  label,
  value,
  empty,
  hint,
  testid,
}: {
  label: string;
  value: string;
  empty: string;
  hint?: ReactNode;
  testid: string;
}) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="space-y-1" data-testid={testid}>
      <div className="text-xs font-medium text-muted-foreground">{label}</div>
      {value ? (
        <div className="flex flex-wrap items-center gap-2">
          <code className="break-all font-mono text-xs">{value}</code>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="h-7"
            data-testid={`${testid}-copy`}
            onClick={() => {
              void navigator.clipboard?.writeText(value);
              setCopied(true);
            }}
          >
            {copied ? (
              <Check aria-hidden className="h-3.5 w-3.5" />
            ) : (
              <Copy aria-hidden className="h-3.5 w-3.5" />
            )}
            {copied ? "Copied" : "Copy"}
          </Button>
        </div>
      ) : (
        <div className="text-xs text-muted-foreground">{empty}</div>
      )}
      {hint ? <div className="text-xs text-muted-foreground">{hint}</div> : null}
    </div>
  );
}
