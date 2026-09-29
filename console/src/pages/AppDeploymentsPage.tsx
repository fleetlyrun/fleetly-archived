// 部署页：部署动作（粘贴/上传 compose → POST Deploy → 跟踪到终态）、
// 部署历史（状态徽章含 blocked_waiting/observing 等中间态；失败行展示
// code + verdict + recovery 同信封形态）、回滚（选 revision → Rollback）、
// 字段级 diff（行内 What changed 展开，对比上一部署的归一化快照，T0-V2.4）、
// 每行 View compose（该部署 revision 的实际生效快照对话框，2026-09-29
// IA 重设计 §4.4——历史每一版的 compose 可回看）。

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Check,
  CheckCircle2,
  Copy,
  FileUp,
  GitBranch,
  Loader2,
  Rocket,
  Undo2,
  Webhook,
} from "lucide-react";
import { useRef, useState, type ChangeEvent, type FormEvent, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import { apiBase } from "@/api/client";
import {
  deploy,
  getApp,
  getDeployment,
  listDeployments,
  listRevisions,
  rollbackDeployment,
  cancelDeployment,
  setAppSource,
  setAppWebhookSecret,
  showAppWebhook,
} from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import type { ComposeWarning, DeploymentView } from "@/api/types";
import { ComposeDialog } from "@/components/app-compose-card";
import { DeploymentDiff } from "@/components/deployment-diff";
import { DeploymentFailureAlert, EnvelopeAlert } from "@/components/envelope-alert";
import { StateBadge } from "@/components/state-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
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
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { formatTime, timeAgo } from "@/lib/utils";
import { useIsPlatformAdmin, useProjectContext, useTeamCapabilities } from "@/lib/context";

/** 终态集：之外的状态轮询跟踪。 */
const TERMINAL = new Set(["succeeded", "failed", "cancelled"]);

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

function DeployCard({ app }: { app: string }) {
  const queryClient = useQueryClient();
  // 项目归属上下文（2026-09-25 走查 + 2026-09-26 W2-4 二修）：优先用应用
  // 自身归属（AppView.team_slug/project_slug——详情壳 ["app", name] 查询
  // 共享缓存，本卡不再额外发请求；v0.3 归属模型下该字段恒在），顶栏项目
  // 上下文仅作详情数据未达时的缺省回退——此前只看顶栏：All projects 下重
  // 部署既有应用被禁用（多团队用户），而应用归属明明已知（隐式全局状态
  // 依赖是缺口本身）。未解析出归属时保持旧门：多团队用户禁用+指路。
  const { teams, projectRef } = useProjectContext();
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
    <Card>
      <CardHeader className="border-b pb-3">
        <CardTitle className="flex items-center gap-2 text-sm font-semibold">
          <Rocket aria-hidden className="h-4 w-4 text-muted-foreground" />
          Deploy compose
        </CardTitle>
        <CardDescription>
          Paste compose YAML or upload the file. The app is created on first
          deploy; changes deploy with zero downtime.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
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
      </CardContent>
    </Card>
  );
}

function RollbackCard({ app }: { app: string }) {
  const queryClient = useQueryClient();
  const [revisionId, setRevisionId] = useState("");
  const [error, setError] = useState<ReturnType<typeof errorEnvelopeFrom> | null>(null);
  const [queuedId, setQueuedId] = useState("");

  const revisionsQuery = useQuery({
    queryKey: ["revisions", app],
    queryFn: () => listRevisions(app),
  });
  const rollbackMutation = useMutation({
    mutationFn: () =>
      rollbackDeployment(app, revisionId === "latest" ? undefined : revisionId),
    onSuccess: (resp) => {
      setQueuedId(resp.deployment_id ?? "");
      setError(null);
      void queryClient.invalidateQueries({ queryKey: ["deployments", app] });
    },
    onError: (err) => setError(errorEnvelopeFrom(err)),
  });

  const revisions = (revisionsQuery.data?.revisions ?? []).filter(
    (r) => r.status === "active",
  );

  return (
    <Card>
      <CardHeader className="border-b pb-3">
        <CardTitle className="flex items-center gap-2 text-sm font-semibold">
          <Undo2 aria-hidden className="h-4 w-4 text-muted-foreground" />
          Rollback
        </CardTitle>
        <CardDescription>
          Revision replay (last 5 verified revisions). Empty target = rollback
          to the previous revision.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="flex items-end gap-2">
          <div className="flex-1 space-y-2">
            <Label htmlFor="rollback-revision">Target revision</Label>
            <Select value={revisionId} onValueChange={setRevisionId}>
              <SelectTrigger id="rollback-revision">
                <SelectValue placeholder="Latest verified (previous revision)" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="latest">Latest successful</SelectItem>
                {revisions.map((r) => (
                  <SelectItem key={r.id} value={r.id ?? ""}>
                    #{r.seq} · {(r.id ?? "").slice(0, 12)} · {timeAgo(r.created_at)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <Button
            variant="outline"
            onClick={() => rollbackMutation.mutate()}
            disabled={rollbackMutation.isPending}
          >
            {rollbackMutation.isPending ? (
              <Loader2 aria-hidden className="h-4 w-4 animate-spin" />
            ) : (
              <Undo2 aria-hidden className="h-4 w-4" />
            )}
            Rollback
          </Button>
        </div>
        {error ? (
          <EnvelopeAlert code={error.code} message={error.message} suggestion={error.suggestion} docs={error.docs} />
        ) : null}
        {queuedId ? (
          <p className="text-sm text-muted-foreground">
            Rollback queued: <code className="font-mono text-xs">{queuedId}</code>
          </p>
        ) : null}
      </CardContent>
    </Card>
  );
}

// ── Deploy triggers 卡（P1-8「Git push 通道不可发现」，2026-09-25 审查
// backlog #11；proto apps.proto ShowAppWebhook/SetAppWebhookSecret/SetAppSource，
// scope.go 三 RPC 均 admin）───────────────────────────────────────────────
// 展示全部取自响应字段：git_remote_hint（push 远端，git SSH 面未启用时为
// 空串）、source_url/branch/auth_kind、secret_configured 位（值永不回读，
// 契约无指纹）。webhook 接收端 URL 不在响应内——按 gateway 既有路由
//（internal/runtime/gateway.go：POST /v1/apps/{app}/webhooks/github|gitea）
// 与 console apiBase 拼装，是既有服务端事实的展示，不是新契约面。
// 路径段必须用响应解析出的**业务名**（name 字段）：服务端分派正则
//（internal/gitserver/webhook.go WebhookPathPattern）只收
// [a-z0-9][a-z0-9-]{0,62} 且命中后按业务名查行——本页路由参数是平台 id
//（26 字符 ULID 含大写，永不匹配分派正则），拿它拼接收端 URL 是恒 404
// 死链（2026-09-25 复核修复）。业务名含下划线等出律字符的形态服务端
// 同样不收——卡内如实注一行说明。

/** 拉源认证形态词表（proto SetAppSourceRequest.source_auth_kind in 约束）。 */
type SourceAuthKind = "none" | "https_token" | "ssh_key";

/** webhook 接收端绝对 URL（apiBase 缺省相对 /v1——以当前 origin 补全供
 * 复制）。app 参数必须是**业务名**：服务端接收端按业务名分派（见文件头
 * 注）——调用方传路由参数（平台 id）会拼出死链。 */
function webhookReceiverUrl(app: string, forge: "github" | "gitea"): string {
  const base = new URL(apiBase(), window.location.origin).href.replace(/\/+$/, "");
  return `${base}/apps/${encodeURIComponent(app)}/webhooks/${forge}`;
}

/** 服务端接收端分派对 app 名的词形约束（WebhookPathPattern 第一捕获组）。 */
const WEBHOOK_NAME_PATTERN = /^[a-z0-9][a-z0-9-]{0,62}$/;

/** 读态行：code 值 + 复制（System 页指纹卡同款形态）；空值诚实说明。 */
function CopyValueRow({
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

function DeployTriggersCard({ app }: { app: string }) {
  const queryClient = useQueryClient();
  const webhookQuery = useQuery({
    queryKey: ["app-webhook", app],
    queryFn: () => showAppWebhook(app),
  });
  const cfg = webhookQuery.data;

  // secret 轮换（write-only：值只出现在请求体，成功后不回显明文——旧值即
  // 时失效：验签按库存值单点判定，state.SetAppWebhookSecret 直接覆盖）。
  const [secret, setSecret] = useState("");
  const [secretDialogOpen, setSecretDialogOpen] = useState(false);
  const [secretStored, setSecretStored] = useState(false);

  // 拉源表单（整体替换语义——读态到达时一次性水合，避免空表单提交静默清掉
  // 既有 source；渲染期派生模式，不经 effect）。
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

  const secretReady = secret.length >= 16;
  const sourceReady =
    sourceBranch.trim() !== "" && (authKind === "none" || authSecret.length >= 16);
  const configured = cfg?.secret_configured === true;
  // 读面失败（403 信封等）一等渲染——不静默吞掉。
  const readError = webhookQuery.isError ? errorEnvelopeFrom(webhookQuery.error) : null;

  function onSourceSubmit(e: FormEvent) {
    e.preventDefault();
    if (sourceReady) sourceMutation.mutate();
  }

  return (
    <Card data-testid="deploy-triggers-card">
      <CardHeader className="border-b pb-3">
        <CardTitle className="flex items-center gap-2 text-sm font-semibold">
          <Webhook aria-hidden className="h-4 w-4 text-muted-foreground" />
          Deploy triggers
        </CardTitle>
        <CardDescription>
          Push to deploy over git, or trigger from GitHub/Gitea webhooks. The
          app&apos;s configured branch is the trigger branch for both channels.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-5 pt-4">
        {readError ? (
          <EnvelopeAlert
            code={readError.code}
            message={readError.message}
            suggestion={readError.suggestion}
            docs={readError.docs}
          />
        ) : null}

        {/* git push 通道：远端 + 触发分支 + deploy key 入口。 */}
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

        {/* webhook 通道：接收端 URL（github/gitea 双变体）+ 签名密钥态。 */}
        <div className="space-y-3 border-t pt-4">
          <div className="flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            <Webhook aria-hidden className="h-3.5 w-3.5" />
            Webhook
          </div>
          <CopyValueRow
            label="Receiver URL (GitHub)"
            value={cfg?.name ? webhookReceiverUrl(cfg.name, "github") : ""}
            testid="triggers-webhook-url"
            empty="Available once the trigger configuration loads."
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
        </div>

        {/* 拉源配置（webhook 收到投递后拉取代码的 remote；整体替换语义）。 */}
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
      </CardContent>

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
    </Card>
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
  // View compose 对话框（2026-09-29）：该部署 revision 的实际生效快照。
  const [composeOpen, setComposeOpen] = useState(false);
  const { canDeploy } = useTeamCapabilities();
  const cancellable = canDeploy && !TERMINAL.has(d.status ?? "");
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
          <TableCell colSpan={6} className="p-4">
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
    </>
  );
}

export function AppDeploymentsPage() {
  const { name = "" } = useParams();
  // 单一展开位：一次只看一条部署的 diff（再点收起）。
  const [expandedId, setExpandedId] = useState("");
  // 角色门（前端体验门，§3.2）：部署/回滚/取消 = developer+；触发面读/写
  //（ShowAppWebhook 三 RPC）= admin scope（scope.go 登记）→ canAdminResources
  // 同口径。平台管理员资源面恒只读（P0-3 双门，服务端 ownership.go 硬拒）
  // ——写卡换成诚实说明，不做静默消失。
  const { canDeploy, canAdminResources } = useTeamCapabilities();
  const isPlatformAdmin = useIsPlatformAdmin();

  const historyQuery = useQuery({
    queryKey: ["deployments", name],
    queryFn: () => listDeployments(name, 20),
    refetchInterval: (q) => {
      // 存在非终态部署行时保持轮询（跟踪进行中的部署）。
      const rows = q.state.data?.deployments ?? [];
      return rows.some((d) => !TERMINAL.has(d.status ?? "")) ? 2000 : 10000;
    },
  });

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

  return (
    <div className="space-y-4">
      <div className="grid grid-cols-1 items-start gap-4 lg:grid-cols-2">
        {canDeploy ? (
          <>
            <DeployCard app={name} />
            <RollbackCard app={name} />
          </>
        ) : isPlatformAdmin ? (
          // P0-3：平台管理员资源面只读（职责分离）——说明卡落在 Deploy/
          // Rollback 卡原位，指明可行动路径（CLI 机具令牌 / 成员角色）。
          <Card className="border-dashed lg:col-span-2">
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
      </div>
      {/* Deploy triggers 卡（P1-8）：读面与写面同门（admin scope）——admin+
          见卡，其余角色见说明态（平台管理员走职责分离口径）。 */}
      {canAdminResources ? (
        <DeployTriggersCard app={name} />
      ) : (
        <Card className="border-dashed">
          <CardContent
            className="p-4 text-sm text-muted-foreground"
            data-testid="triggers-admin-note"
          >
            {isPlatformAdmin
              ? "Platform administrators have read-only access to resources (separation of duties). Deploy trigger settings are managed by team admins, or from the CLI with a machine token."
              : "Deploy trigger settings (git push remote, webhooks, fetch source) are visible to team admins only (admin scope on this app's project). Ask a team admin for access."}
          </CardContent>
        </Card>
      )}
      <Card>
        <CardHeader className="flex-row items-center justify-between space-y-0 border-b pb-3">
          <CardTitle className="text-sm font-semibold">
            Deployment history{" "}
            <span className="font-normal text-muted-foreground">({deployments.length})</span>
          </CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          {deployments.length === 0 ? (
            <p className="p-5 text-sm text-muted-foreground">No deployments yet.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>State</TableHead>
                  <TableHead>Deployment</TableHead>
                  <TableHead>Created</TableHead>
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
        </CardContent>
      </Card>
    </div>
  );
}
