package cmd

// 库实例命令（E4 数据库托管，W4-S2 生命周期面）：`fleetly databases
// <create|get|show|list|delete|suspend|resume|retry|settings>`。全部经 RPC
//（CLI-over-SDK 纪律）；连接信息为服务端脱敏投影——密码明文零出现
//（掩码 URL + 指纹），显式 reveal 不在 CLI 面（S4/S6）。删除在 CLI 侧
// 自动回传 confirm=<名>（REST 面保持显式 confirm 参数）；--delete-volumes
// 是唯一需要显式 flag 的数据破坏性选择。

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/lynx-go/commands"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
)

// databasesCmd 是外层动词 `databases`：分发生命周期子命令。
type databasesCmd struct {
	sub *commands.App
}

func newDatabasesCmd() *databasesCmd {
	sub := commands.New()
	sub.Register(&databaseCreateCmd{}, &databaseGetCmd{}, &databaseShowCmd{}, &databaseListCmd{}, &databaseDeleteCmd{},
		&databaseSuspendCmd{}, &databaseResumeCmd{}, &databaseRetryCmd{}, &databaseSettingsCmd{},
		&databaseRotateCmd{}, &databaseRevealCmd{},
		&databaseBackupCmd{}, &databaseBackupsCmd{}, &databaseRestoreCmd{}, &databaseUpgradeCmd{})
	sub.VerbTitle = "databases subcommands:"
	return &databasesCmd{sub: sub}
}

func (c *databasesCmd) Name() string { return "databases" }
func (c *databasesCmd) Synopsis() string {
	return "create and manage managed database instances (lifecycle, settings; connection secrets are masked)"
}
func (c *databasesCmd) Usage() string {
	return "databases <create|get|show|list|delete|suspend|resume|retry|settings|rotate|reveal|backup|backups|restore|upgrade> [flags] ..."
}

func (c *databasesCmd) SetFlags(_ *flag.FlagSet) {}

func (c *databasesCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (create|get|show|list|delete|suspend|resume|retry|settings|rotate|reveal|backup|backups|restore|upgrade)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// databaseLimitFlags 是 create/settings 共享的限额与备份计划 flag 集（零值 =
// 模板/平台缺省）。
type databaseLimitFlags struct {
	cpu           float64
	memoryBytes   int64
	backupHours   int
	backupKeep    int
	backupHourUTC int
}

func (f *databaseLimitFlags) register(fs *flag.FlagSet) {
	fs.Float64Var(&f.cpu, "cpu", 0, "CPU limit in cores (0 = template default)")
	fs.Int64Var(&f.memoryBytes, "memory-bytes", 0, "memory limit in bytes (0 = template default)")
	fs.IntVar(&f.backupHours, "backup-interval-hours", 0, "backup interval in hours (0 = platform default 24)")
	fs.IntVar(&f.backupKeep, "backup-keep", 0, "backups to keep (0 = platform default 7)")
	fs.IntVar(&f.backupHourUTC, "backup-hour-utc", 0, "daily backup hour in UTC (0 = platform default 3)")
}

func (f *databaseLimitFlags) limits() *serverv1.DatabaseLimits {
	if f.cpu == 0 && f.memoryBytes == 0 {
		return nil
	}
	return &serverv1.DatabaseLimits{CpuSeconds: f.cpu, MemoryBytes: f.memoryBytes}
}

func (f *databaseLimitFlags) backupPlan() *serverv1.DatabaseBackupPlan {
	if f.backupHours == 0 && f.backupKeep == 0 && f.backupHourUTC == 0 {
		return nil
	}
	return &serverv1.DatabaseBackupPlan{
		IntervalHours: int32(f.backupHours),
		Keep:          int32(f.backupKeep),
		HourUtc:       int32(f.backupHourUTC),
	}
}

// databaseCreateCmd 实现 `fleetly databases create <name> <template>`。
type databaseCreateCmd struct {
	template string
	jsonOut  bool
	limits   databaseLimitFlags
	conn     connFlags
}

func (c *databaseCreateCmd) Name() string { return "create" }
func (c *databaseCreateCmd) Synopsis() string {
	return "create a managed database instance (accepted as provisioning; the platform converges it to ready)"
}
func (c *databaseCreateCmd) Usage() string {
	return "databases create [--template postgres-16|postgres-18|percona-postgresql-18|redis-7|mysql-8.4|mongodb-8.0] [--cpu ...] [--memory-bytes ...] [--addr <host:port>] [--token <tok>] [--json] <name>"
}

func (c *databaseCreateCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.StringVar(&c.template, "template", "postgres-16", "engine template id (postgres-16, postgres-18, percona-postgresql-18, redis-7, mysql-8.4 or mongodb-8.0)")
	c.limits.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *databaseCreateCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	// v0.3 W2-S3 归属：上下文解析单点 resolveContext（flag > env > config）。
	rc, err := resolveContext(c.conn.team, c.conn.project)
	if err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Databases().CreateDatabase(ctx, &serverv1.CreateDatabaseRequest{
			Name:       args[0],
			Template:   c.template,
			Limits:     c.limits.limits(),
			BackupPlan: c.limits.backupPlan(),
			Project:    rc.Project, // 裸名或 team/prj；用户缺省个人队 default
		})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeJSON(env.Stdout, toDatabaseJSON(resp.GetDatabase()))
		}
		_, err = fmt.Fprint(env.Stdout, renderDatabase(resp.GetDatabase()))
		return err
	})
}

// databaseGetCmd 实现 `fleetly databases get <name>`。
type databaseGetCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *databaseGetCmd) Name() string { return "get" }
func (c *databaseGetCmd) Synopsis() string {
	return "show one database instance (connection values are masked; use the referencing app env for the live credential)"
}
func (c *databaseGetCmd) Usage() string {
	return "databases get [--addr <host:port>] [--token <tok>] [--json] <name>"
}

func (c *databaseGetCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *databaseGetCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Databases().GetDatabase(ctx, &serverv1.GetDatabaseRequest{Name: c.conn.ref(args[0])})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeJSON(env.Stdout, toDatabaseJSON(resp.GetDatabase()))
		}
		_, err = fmt.Fprint(env.Stdout, renderDatabase(resp.GetDatabase()))
		return err
	})
}

// databaseShowCmd 实现 `fleetly databases show <name>`（managed-databases
// §5.4 动词表的同义动词——与 get 完全同义的别名：共享 get 的 flag 集、
// RPC 与渲染，只换动词名。不建别名机制，最小加法直挂同实现）。
type databaseShowCmd struct {
	databaseGetCmd
}

func (c *databaseShowCmd) Name() string { return "show" }
func (c *databaseShowCmd) Synopsis() string {
	return c.databaseGetCmd.Synopsis()
}
func (c *databaseShowCmd) Usage() string {
	return "databases show [--addr <host:port>] [--token <tok>] [--json] <name>"
}

// Run 先按 show 的 usage 校验参数形状，再直通 get 实现（flag 集与渲染共享；
// get 内层的形状校验此处恒已满足）。
func (c *databaseShowCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.databaseGetCmd.Run(ctx, env, args)
}

// databaseListCmd 实现 `fleetly databases list`。
type databaseListCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *databaseListCmd) Name() string { return "list" }
func (c *databaseListCmd) Synopsis() string {
	return "list database instances (name ascending; deleted tombstones are not listed)"
}
func (c *databaseListCmd) Usage() string {
	return "databases list [--addr <host:port>] [--token <tok>] [--json]"
}

func (c *databaseListCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *databaseListCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Databases().ListDatabases(ctx, &serverv1.ListDatabasesRequest{})
		if err != nil {
			return err
		}
		if c.jsonOut {
			rows := make([]databaseJSON, 0, len(resp.GetDatabases()))
			for _, v := range resp.GetDatabases() {
				rows = append(rows, toDatabaseJSON(v))
			}
			return writeJSON(env.Stdout, struct {
				Databases []databaseJSON `json:"databases"`
			}{rows})
		}
		if len(resp.GetDatabases()) == 0 {
			_, err := fmt.Fprintln(env.Stdout, "no database instances (create one with: fleetly databases create <name> --template <id>)")
			return err
		}
		for _, v := range resp.GetDatabases() {
			line := fmt.Sprintf("%s  %-13s %-12s %s", v.GetName(), v.GetStatus(), v.GetTemplate(), v.GetPlacement())
			if le := v.GetLastError(); le != "" {
				line += "  error=" + le
			}
			if _, err := fmt.Fprintln(env.Stdout, line); err != nil {
				return err
			}
		}
		return nil
	})
}

// databaseDeleteCmd 实现 `fleetly databases delete <name> [--delete-volumes]`。
type databaseDeleteCmd struct {
	deleteVolumes bool
	conn          connFlags
}

func (c *databaseDeleteCmd) Name() string { return "delete" }
func (c *databaseDeleteCmd) Synopsis() string {
	return "delete a database instance (tombstone; data volume is KEPT and orphaned unless --delete-volumes is passed)"
}
func (c *databaseDeleteCmd) Usage() string {
	return "databases delete [--delete-volumes] [--addr <host:port>] [--token <tok>] <name>"
}

func (c *databaseDeleteCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.deleteVolumes, "delete-volumes", false, "irreversibly delete the data volume (default: keep it as orphaned)")
}

func (c *databaseDeleteCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Databases().DeleteDatabase(ctx, &serverv1.DeleteDatabaseRequest{
			Name:          c.conn.ref(args[0]), // 引用面补全限定形；confirm 须实例裸名（下方）
			Confirm:       args[0],
			DeleteVolumes: c.deleteVolumes,
		})
		if err != nil {
			return err
		}
		note := "the data volume is kept as orphaned"
		if c.deleteVolumes {
			note = "the data volume is being deleted irreversibly"
		}
		_, err = fmt.Fprintf(env.Stdout, "database %s accepted for deletion (%s); the platform reaps managed objects in the background; %s\n",
			resp.GetName(), resp.GetStatus(), note)
		return err
	})
}

// databaseSuspendCmd 实现 `fleetly databases suspend <name>`。
type databaseSuspendCmd struct {
	conn connFlags
}

func (c *databaseSuspendCmd) Name() string { return "suspend" }
func (c *databaseSuspendCmd) Synopsis() string {
	return "suspend a database (scale to zero; services and volumes are retained, referencing apps lose connectivity)"
}
func (c *databaseSuspendCmd) Usage() string {
	return "databases suspend [--addr <host:port>] [--token <tok>] <name>"
}

func (c *databaseSuspendCmd) SetFlags(fs *flag.FlagSet) { c.conn.register(fs) }

func (c *databaseSuspendCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Databases().SuspendDatabase(ctx, &serverv1.SuspendDatabaseRequest{Name: c.conn.ref(args[0])})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(env.Stdout, "database %s suspended (status %s)\n", args[0], resp.GetDatabase().GetStatus())
		return err
	})
}

// databaseResumeCmd 实现 `fleetly databases resume <name>`。
type databaseResumeCmd struct {
	conn connFlags
}

func (c *databaseResumeCmd) Name() string { return "resume" }
func (c *databaseResumeCmd) Synopsis() string {
	return "resume a suspended database (reconverges to provisioning, then ready/degraded)"
}
func (c *databaseResumeCmd) Usage() string {
	return "databases resume [--addr <host:port>] [--token <tok>] <name>"
}

func (c *databaseResumeCmd) SetFlags(fs *flag.FlagSet) { c.conn.register(fs) }

func (c *databaseResumeCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Databases().ResumeDatabase(ctx, &serverv1.ResumeDatabaseRequest{Name: c.conn.ref(args[0])})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(env.Stdout, "database %s resumed (status %s); reconvergence in progress\n", args[0], resp.GetDatabase().GetStatus())
		return err
	})
}

// databaseRetryCmd 实现 `fleetly databases retry <name>`。
type databaseRetryCmd struct {
	conn connFlags
}

func (c *databaseRetryCmd) Name() string { return "retry" }
func (c *databaseRetryCmd) Synopsis() string {
	return "retry a failed database (failed -> provisioning reconvergence; the failed scene is preserved until then)"
}
func (c *databaseRetryCmd) Usage() string {
	return "databases retry [--addr <host:port>] [--token <tok>] <name>"
}

func (c *databaseRetryCmd) SetFlags(fs *flag.FlagSet) { c.conn.register(fs) }

func (c *databaseRetryCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Databases().RetryDatabase(ctx, &serverv1.RetryDatabaseRequest{Name: c.conn.ref(args[0])})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(env.Stdout, "database %s retry accepted (status %s); reconvergence in progress\n", args[0], resp.GetDatabase().GetStatus())
		return err
	})
}

// databaseSettingsCmd 实现 `fleetly databases settings <name> [flags]`。
type databaseSettingsCmd struct {
	limits  databaseLimitFlags
	jsonOut bool
	conn    connFlags
}

func (c *databaseSettingsCmd) Name() string { return "settings" }
func (c *databaseSettingsCmd) Synopsis() string {
	return "update limits and backup plan (whole replacement; limits are applied by the next convergence beat)"
}
func (c *databaseSettingsCmd) Usage() string {
	return "databases settings [--cpu ...] [--memory-bytes ...] [--backup-interval-hours ...] [--addr <host:port>] [--token <tok>] [--json] <name>"
}

func (c *databaseSettingsCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	c.limits.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *databaseSettingsCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Databases().UpdateDatabaseSettings(ctx, &serverv1.UpdateDatabaseSettingsRequest{
			Name:       c.conn.ref(args[0]),
			Limits:     c.limits.limits(),
			BackupPlan: c.limits.backupPlan(),
		})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeJSON(env.Stdout, toDatabaseJSON(resp.GetDatabase()))
		}
		_, err = fmt.Fprintf(env.Stdout, "settings updated for %s (cpu=%v memory=%d status=%s)\n",
			args[0], resp.GetDatabase().GetLimits().GetCpuSeconds(),
			resp.GetDatabase().GetLimits().GetMemoryBytes(), resp.GetDatabase().GetStatus())
		return err
	})
}

// databaseRotateCmd 实现 `fleetly databases rotate <name> --confirm <name>`
// （E4 W4-S4，managed-databases §2.5：破坏性两段式——CLI 侧与 API 同面保留
// 显式 confirm，不自动代答；轮换不可逆且引用 app 被自动重部署）。
type databaseRotateCmd struct {
	confirm string
	conn    connFlags
}

func (c *databaseRotateCmd) Name() string { return "rotate" }
func (c *databaseRotateCmd) Synopsis() string {
	return "rotate database credentials (destructive, two-phase confirm; referencing apps are auto-redeployed)"
}
func (c *databaseRotateCmd) Usage() string {
	return "databases rotate --confirm <name> [--addr <host:port>] [--token <tok>] <name>"
}

func (c *databaseRotateCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.StringVar(&c.confirm, "confirm", "", "pass the instance name to confirm rotation (two-phase destructive confirm)")
}

func (c *databaseRotateCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Databases().RotateDatabaseCredentials(ctx, &serverv1.RotateDatabaseCredentialsRequest{
			Name:    c.conn.ref(args[0]),
			Confirm: c.confirm,
		})
		if err != nil {
			return err
		}
		apps := strings.Join(resp.GetRedeployedApps(), ", ")
		if apps == "" {
			apps = "(no referencing apps)"
		}
		_, err = fmt.Fprintf(env.Stdout, "database %s credentials rotated (fingerprint %s); referencing apps requeued for redeploy: %s\n",
			args[0], resp.GetDatabase().GetConnection().GetPasswordFingerprint(), apps)
		return err
	})
}

// databaseRevealCmd 实现 `fleetly databases reveal <name>`（§2.5 显式展开
// 面——输出含密码明文与完整 URL；服务端已落 db.reveal 审计）。
type databaseRevealCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *databaseRevealCmd) Name() string { return "reveal" }
func (c *databaseRevealCmd) Synopsis() string {
	return "reveal database connection credentials in plaintext (admin; the access is audited)"
}
func (c *databaseRevealCmd) Usage() string {
	return "databases reveal [--addr <host:port>] [--token <tok>] [--json] <name>"
}

func (c *databaseRevealCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *databaseRevealCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Databases().RevealDatabaseCredentials(ctx, &serverv1.RevealDatabaseCredentialsRequest{Name: c.conn.ref(args[0])})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeJSON(env.Stdout, resp)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "database %s (template %s)\n", resp.GetName(), resp.GetTemplate())
		fmt.Fprintf(&b, "  host: %s\n  port: %d\n", resp.GetHost(), resp.GetPort())
		if resp.GetUser() != "" {
			fmt.Fprintf(&b, "  user: %s\n  database: %s\n", resp.GetUser(), resp.GetDatabase())
		}
		fmt.Fprintf(&b, "  password: %s\n", resp.GetPassword())
		fmt.Fprintf(&b, "  url: %s\n", resp.GetUrl())
		fmt.Fprintf(&b, "  note: this access has been recorded in the audit log\n")
		_, err = fmt.Fprint(env.Stdout, b.String())
		return err
	})
}

// ── E4 W4-S5：备份/恢复/升级子命令（managed-databases §2.6）────────────────

// databaseBackupCmd 实现 `fleetly databases backup <name>`（手动备份受理
// ——异步：job 分钟级，结论经 `databases backups <name>` 台账与平台事件
// 披露）。
type databaseBackupCmd struct {
	conn connFlags
}

func (c *databaseBackupCmd) Name() string { return "backup" }
func (c *databaseBackupCmd) Synopsis() string {
	return "trigger a manual database backup (async; check progress with 'databases backups <name>' and platform events)"
}
func (c *databaseBackupCmd) Usage() string {
	return "databases backup [--addr <host:port>] [--token <tok>] <name>"
}

func (c *databaseBackupCmd) SetFlags(fs *flag.FlagSet) { c.conn.register(fs) }

func (c *databaseBackupCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Databases().TriggerDatabaseBackup(ctx, &serverv1.TriggerDatabaseBackupRequest{Name: c.conn.ref(args[0])})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(env.Stdout, "database %s backup accepted (kind %s); the ledger row appears when the job finishes — events carry success/failure\n",
			resp.GetName(), resp.GetKind())
		return err
	})
}

// databaseBackupsCmd 实现 `fleetly databases backups <name>`（台账列表——
// kind/snapshot/size/verify_status 全量事实面）。
type databaseBackupsCmd struct {
	limit   int
	jsonOut bool
	conn    connFlags
}

func (c *databaseBackupsCmd) Name() string { return "backups" }
func (c *databaseBackupsCmd) Synopsis() string {
	return "list database backups (newest first; verify_status is the honesty gate — failed rows mean the backup must not be trusted)"
}
func (c *databaseBackupsCmd) Usage() string {
	return "databases backups [--limit n] [--addr <host:port>] [--token <tok>] [--json] <name>"
}

func (c *databaseBackupsCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.IntVar(&c.limit, "limit", 20, "maximum rows to list")
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *databaseBackupsCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Databases().ListDatabaseBackups(ctx, &serverv1.ListDatabaseBackupsRequest{
			Name:  c.conn.ref(args[0]),
			Limit: int32(c.limit),
		})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeJSON(env.Stdout, resp)
		}
		if len(resp.GetBackups()) == 0 {
			_, err := fmt.Fprintln(env.Stdout, "no backups recorded (trigger one with: fleetly databases backup <name>)")
			return err
		}
		for _, b := range resp.GetBackups() {
			line := fmt.Sprintf("%s  %-11s %-12s %d bytes  verify=%s %s",
				b.GetId(), b.GetKind(), shortSnapshot(b.GetSnapshot()), b.GetSizeBytes(),
				b.GetVerifyStatus(), tstampRFC3339(b.GetCreatedAt()))
			if b.GetError() != "" {
				line += "  error=" + b.GetError()
			}
			if _, err := fmt.Fprintln(env.Stdout, line); err != nil {
				return err
			}
		}
		return nil
	})
}

// shortSnapshot 是 snapshot id 的列表展示形态（长 id 截尾——寻址仍以完整
// id；JSON 输出带全量）。
func shortSnapshot(s string) string {
	if len(s) > 16 {
		return s[:16] + "..."
	}
	return s
}

// databaseRestoreCmd 实现 `fleetly databases restore <name> --snapshot <id>
// --confirm <name>`（破坏性两段式——原地重放覆盖数据卷上的现库；confirm
// 与 API 同面显式回传，不自动代答）。
type databaseRestoreCmd struct {
	snapshot string
	confirm  string
	conn     connFlags
}

func (c *databaseRestoreCmd) Name() string { return "restore" }
func (c *databaseRestoreCmd) Synopsis() string {
	return "restore a database in place from a backup snapshot (DESTRUCTIVE: the current data on the volume is overwritten; the instance is stopped and replayed)"
}
func (c *databaseRestoreCmd) Usage() string {
	return "databases restore --snapshot <id> --confirm <name> [--addr <host:port>] [--token <tok>] <name>"
}

func (c *databaseRestoreCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.StringVar(&c.snapshot, "snapshot", "", "restic snapshot id to restore (list with: databases backups <name>)")
	fs.StringVar(&c.confirm, "confirm", "", "pass the instance name to confirm the destructive restore")
}

func (c *databaseRestoreCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	if c.snapshot == "" {
		return fmt.Errorf("--snapshot is required (list candidates with: fleetly databases backups %s)", args[0])
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Databases().RestoreDatabaseBackup(ctx, &serverv1.RestoreDatabaseBackupRequest{
			Name:     c.conn.ref(args[0]),
			Snapshot: c.snapshot,
			Confirm:  c.confirm,
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(env.Stdout, "database %s restore accepted (snapshot %s); the instance is stopped while the snapshot is replayed — completion and failure surface via events\n",
			resp.GetName(), resp.GetSnapshot())
		return err
	})
}

// databaseUpgradeCmd 实现 `fleetly databases upgrade <name> --confirm <name>`
// （受控升级受理——pre_upgrade 备份门 → digest 受控重建 → 健康门；失败 =
// digest 归位 + degraded。confirm 与 API 同面显式回传）。
type databaseUpgradeCmd struct {
	confirm string
	conn    connFlags
}

func (c *databaseUpgradeCmd) Name() string { return "upgrade" }
func (c *databaseUpgradeCmd) Synopsis() string {
	return "upgrade a database to the current template image (pre-upgrade backup gate, controlled rebuild with a downtime window; failure rolls the digest back)"
}
func (c *databaseUpgradeCmd) Usage() string {
	return "databases upgrade --confirm <name> [--addr <host:port>] [--token <tok>] <name>"
}

func (c *databaseUpgradeCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.StringVar(&c.confirm, "confirm", "", "pass the instance name to confirm the controlled rebuild")
}

func (c *databaseUpgradeCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Databases().UpgradeDatabase(ctx, &serverv1.UpgradeDatabaseRequest{
			Name:    c.conn.ref(args[0]),
			Confirm: c.confirm,
		})
		if err != nil {
			return err
		}
		v := resp.GetDatabase()
		_, err = fmt.Fprintf(env.Stdout, "database %s upgrade accepted (pre-upgrade backup gate runs first); %s -> template image, progress via events\n",
			v.GetName(), shortSnapshot(v.GetImageDigest()))
		return err
	})
}

// renderDatabase 是详情的人读形态（连接段恒为掩码口径）。
func renderDatabase(v *serverv1.DatabaseView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "database %s\n", v.GetName())
	fmt.Fprintf(&b, "  id: %s\n  template: %s\n  status: %s\n", v.GetId(), v.GetTemplate(), v.GetStatus())
	fmt.Fprintf(&b, "  image: %s\n", v.GetImageDigest())
	fmt.Fprintf(&b, "  placement: %s\n", v.GetPlacement())
	if vol := v.GetVolume(); vol != nil {
		fmt.Fprintf(&b, "  volume: %s (%s)\n", vol.GetName(), vol.GetStatus())
	} else {
		b.WriteString("  volume: (not registered yet)\n")
	}
	if conn := v.GetConnection(); conn != nil {
		fmt.Fprintf(&b, "  connection: %s\n", conn.GetUrl())
		fmt.Fprintf(&b, "  password: masked (fingerprint %s)\n", conn.GetPasswordFingerprint())
	}
	limits := v.GetLimits()
	fmt.Fprintf(&b, "  limits: cpu=%v memory=%d\n", limits.GetCpuSeconds(), limits.GetMemoryBytes())
	backup := v.GetBackupPlan()
	fmt.Fprintf(&b, "  backup: interval=%dh keep=%d hour_utc=%d\n",
		backup.GetIntervalHours(), backup.GetKeep(), backup.GetHourUtc())
	if v.GetUpgradeAvailable() {
		fmt.Fprintf(&b, "  upgrade: available (run 'fleetly databases upgrade %s --confirm %s')\n", v.GetName(), v.GetName())
	}
	if v.GetLastError() != "" {
		fmt.Fprintf(&b, "  last_error: %s\n", v.GetLastError())
	}
	return b.String()
}

// databaseJSON 是库实例的机器可读投影（时间 RFC3339；连接只带掩码形态）。
type databaseJSON struct {
	ID                  string                           `json:"id"`
	Name                string                           `json:"name"`
	Template            string                           `json:"template"`
	Status              string                           `json:"status"`
	ImageDigest         string                           `json:"image_digest"`
	UpgradeAvailable    bool                             `json:"upgrade_available,omitempty"`
	Placement           string                           `json:"placement,omitempty"`
	Volume              *serverv1.DatabaseVolumeView     `json:"volume,omitempty"`
	Connection          *serverv1.DatabaseConnectionView `json:"connection,omitempty"`
	Limits              *serverv1.DatabaseLimits         `json:"limits,omitempty"`
	BackupPlan          *serverv1.DatabaseBackupPlan     `json:"backup_plan,omitempty"`
	CredentialUpdatedAt string                           `json:"credential_updated_at,omitempty"`
	LastError           string                           `json:"last_error,omitempty"`
	CreatedAt           string                           `json:"created_at,omitempty"`
	UpdatedAt           string                           `json:"updated_at,omitempty"`
}

func toDatabaseJSON(v *serverv1.DatabaseView) databaseJSON {
	out := databaseJSON{
		ID:               v.GetId(),
		Name:             v.GetName(),
		Template:         v.GetTemplate(),
		Status:           v.GetStatus(),
		ImageDigest:      v.GetImageDigest(),
		UpgradeAvailable: v.GetUpgradeAvailable(),
		Placement:        v.GetPlacement(),
		Volume:           v.GetVolume(),
		Connection:       v.GetConnection(),
		Limits:           v.GetLimits(),
		BackupPlan:       v.GetBackupPlan(),
		LastError:        v.GetLastError(),
	}
	if ts := v.GetCredentialUpdatedAt(); ts != nil {
		out.CredentialUpdatedAt = tstampRFC3339(ts)
	}
	if ts := v.GetCreatedAt(); ts != nil {
		out.CreatedAt = tstampRFC3339(ts)
	}
	if ts := v.GetUpdatedAt(); ts != nil {
		out.UpdatedAt = tstampRFC3339(ts)
	}
	return out
}

// 编译期断言：databases 命令实现 commands.Command/Flagged 契约。
var (
	_ commands.Command = &databasesCmd{}
	_ commands.Flagged = &databasesCmd{}
)
