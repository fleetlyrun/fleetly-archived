//go:build manual

package database

// IMPL-DB-0/DB-1 真机探针（默认不跑）：dbtools 的工具面（vanilla 16/18 +
// percona 发行版面）在真实 PG 实例上跑通 dump → verify（pg_restore --list）
// → restore 原地重放全链——**读 DB-1 注册表真值**（postgres-16 / postgres-18 /
// percona-postgresql-18；DB-0 期的合成模板已退役）。percona 腿额外覆盖
// pgvector（CREATE EXTENSION vector + vector(3) 数据写入与恢复后回读——
// DT-9「含 pgvector」的真机事实）。
//
//	FLEETLY_MANUAL_DBTOOLS=1 go test -tags manual ./internal/database -run TestManualDbtoolsToolFaces -v
//
// 镜像：缺省 = DefaultDatabaseToolsImage（需本机已 pull 或可匿名拉取）；本地
// 验证新工具面（发布挂账期）时指向本地构建：
//
//	docker buildx build --load -t fleetly-dbtools:local -f deploy/Dockerfile.dbtools deploy
//	FLEETLY_MANUAL_DBTOOLS=1 FLEETLY_MANUAL_DBTOOLS_IMAGE=fleetly-dbtools:local \
//	  go test -tags manual ./internal/database -run TestManualDbtoolsToolFaces -v
//
// 执行形态：docker run 直接承载 adapter 拼装的 job 脚本（strings.Join(script,
// "\n") 逐字——原语级等价命令，见票面「adapter 原语级 job 脚本或等价命令」），
// restic repo = 本地卷（RESTIC_REPOSITORY=/repo，免 S3 依赖）。staging/多节点
// 腿不在此探针范围。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
)

// manualProbeLeg 是一条模板腿的探针参数（工具面路径由 pgToolDir 按
// 发行版 × major 选择，探针不重复写路径）。
type manualProbeLeg struct {
	name        string
	engineImage string
	template    dbtemplate.Template
	// vector 为真时额外覆盖 pgvector（CREATE EXTENSION vector + vector(3)
	// 数据写入与恢复后回读；percona 发行版面）。
	vector bool
}

func TestManualDbtoolsToolFaces(t *testing.T) {
	if os.Getenv("FLEETLY_MANUAL_DBTOOLS") != "1" {
		t.Skip("set FLEETLY_MANUAL_DBTOOLS=1 to run the dbtools tool-face probe")
	}
	toolsImage := os.Getenv("FLEETLY_MANUAL_DBTOOLS_IMAGE")
	if toolsImage == "" {
		toolsImage = DefaultDatabaseToolsImage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	run(t, ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if _, err := tryRun(ctx, "docker", "image", "inspect", toolsImage, "--format", "{{.Id}}"); err != nil {
		// 本机无该引用 → 尝试拉取（私有包需已登录；失败即诚实报错）。
		if _, err := tryRun(ctx, "docker", "pull", toolsImage); err != nil {
			t.Fatalf("dbtools image %s is not available locally and cannot be pulled: %v", toolsImage, err)
		}
	}

	// 工具面自证：三个面（vanilla 16、vanilla 18、percona 18）二进制并存且
	// 版本号正确。
	for _, face := range []struct{ dir, want string }{
		{"/usr/lib/postgresql/16/bin", "16."},
		{"/usr/lib/postgresql/18/bin", "18."},
		{"/usr/pgsql-18/bin", "18."},
	} {
		out := run(t, ctx, "docker", "run", "--rm", toolsImage, face.dir+"/pg_dump", "--version")
		if !strings.Contains(out, face.want) {
			t.Fatalf("tools image %s pg_dump reported %q, want %s", face.dir, out, face.want)
		}
		t.Logf("tools face %s pg_dump: %s", face.dir, strings.TrimSpace(out))
	}

	legs := []manualProbeLeg{
		{name: "vanilla16", engineImage: dbtemplate.DefaultPostgresImage, template: mustTemplate(t, dbtemplate.TemplatePostgres16)},
		{name: "vanilla18", engineImage: dbtemplate.DefaultPostgres18Image, template: mustTemplate(t, dbtemplate.TemplatePostgres18)},
		{name: "percona18", engineImage: dbtemplate.DefaultPerconaPostgresql18Image, template: mustTemplate(t, dbtemplate.TemplatePerconaPostgresql18), vector: true},
	}
	for _, leg := range legs {
		leg := leg
		t.Run(leg.name, func(t *testing.T) {
			probeToolFaceLeg(t, ctx, toolsImage, leg)
		})
	}
}

// probeToolFaceLeg 跑通一条模板腿的 dump → verify → restore 全链（含重放
// 断言：备份后新增的行在恢复后消失、备份时的行在场；vector 腿另断言
// vector 数据与扩展版本）。
func probeToolFaceLeg(t *testing.T, ctx context.Context, toolsImage string, leg manualProbeLeg) {
	t.Helper()
	suffix := "db1p-" + leg.name
	network := "fleetly-" + suffix + "-net"
	serverName := "fleetly-" + suffix + "-server"
	dataVolume := "fleetly-" + suffix + "-data"
	repoVolume := "fleetly-" + suffix + "-repo"
	instance := strings.ReplaceAll(suffix, "-", "")
	mountPath := leg.template.VolumeMountPath
	password := "probe-pass-0123456789"

	// 清场 + 收尾（探针可复跑）。
	cleanup := func() {
		_, _ = tryRun(context.Background(), "docker", "rm", "-f", serverName)
		_, _ = tryRun(context.Background(), "docker", "volume", "rm", "-f", dataVolume)
		_, _ = tryRun(context.Background(), "docker", "volume", "rm", "-f", repoVolume)
		_, _ = tryRun(context.Background(), "docker", "network", "rm", network)
	}
	cleanup()
	t.Cleanup(cleanup)

	run(t, ctx, "docker", "network", "create", network)
	run(t, ctx, "docker", "volume", "create", dataVolume)
	run(t, ctx, "docker", "volume", "create", repoVolume)
	// restic repo 初始化（生产 = 平台 restic 基础设施已 init；探针用本地卷）。
	run(t, ctx, "docker", "run", "--rm", "-v", repoVolume+":/repo",
		"-e", "RESTIC_PASSWORD=probe-repo-pass", "-e", "RESTIC_REPOSITORY=/repo", "-e", "HOME=/tmp",
		toolsImage, "restic", "init")

	startServer := func() {
		run(t, ctx, "docker", "run", "-d", "--name", serverName, "--network", network, "--network-alias", instance,
			"-e", "POSTGRES_PASSWORD="+password, "-e", "POSTGRES_USER=fleetly", "-e", "POSTGRES_DB="+dbtemplate.DatabaseName(instance),
			"-e", "PGDATA="+mountPath+"/pgdata",
			"-v", dataVolume+":"+mountPath,
			leg.engineImage)
	}
	waitReady := func() {
		deadline := time.Now().Add(3 * time.Minute)
		for {
			if _, err := tryRun(ctx, "docker", "exec", serverName, "psql", "-U", "fleetly", "-d", dbtemplate.DatabaseName(instance), "-tAc", "select 1"); err == nil {
				return
			}
			if time.Now().After(deadline) {
				logs, _ := tryRun(context.Background(), "docker", "logs", "--tail", "30", serverName)
				t.Fatalf("%s server not ready; logs:\n%s", leg.name, logs)
			}
			time.Sleep(2 * time.Second)
		}
	}
	sql := func(statement string) string {
		return run(t, ctx, "docker", "exec", serverName, "psql", "-U", "fleetly", "-d", dbtemplate.DatabaseName(instance), "-tAc", statement)
	}

	startServer()
	waitReady()
	version := strings.TrimSpace(sql("select version()"))
	t.Logf("leg %s server: %s", leg.name, version)
	if !strings.Contains(version, fmt.Sprintf(" %d.", leg.template.Major)) {
		t.Fatalf("server version %q does not match leg major %d", version, leg.template.Major)
	}
	run(t, ctx, "docker", "exec", serverName, "psql", "-U", "fleetly", "-d", dbtemplate.DatabaseName(instance), "-c",
		"create table probe_t(id int primary key, v text); insert into probe_t values (1, 'at-backup-time');")
	if leg.vector {
		run(t, ctx, "docker", "exec", serverName, "psql", "-U", "fleetly", "-d", dbtemplate.DatabaseName(instance), "-v", "ON_ERROR_STOP=1", "-c",
			"create extension vector; create table vec_t(id int primary key, v vector(3)); insert into vec_t values (1, '[1,2,3]');")
		ext := strings.TrimSpace(sql("select extversion from pg_extension where extname='vector'"))
		if ext == "" {
			t.Fatal("vector extension missing on the percona leg instance")
		}
		t.Logf("leg %s vector extension: %s", leg.name, ext)
	}

	// ── adapter 原语脚本（逐字承载）──
	backupScript, err := backupJobScript(leg.template, dbtemplate.BackupInput{Instance: instance, TemplateID: leg.template.ID})
	if err != nil {
		t.Fatalf("backupJobScript: %v", err)
	}
	backupOut := runJobScript(t, ctx, toolsImage, network, backupScript, repoVolume,
		"PGPASSWORD="+password, "RESTIC_PASSWORD=probe-repo-pass", "RESTIC_REPOSITORY=/repo", "HOME=/tmp")
	snap, size := parseResticSummary(backupOut)
	if snap == "" {
		t.Fatalf("backup produced no restic snapshot id; raw output:\n%s", backupOut)
	}
	t.Logf("leg %s backup snapshot=%s size=%d", leg.name, snap, size)

	outcome := dbtemplate.BackupOutcome{Instance: instance, TemplateID: leg.template.ID, SnapshotID: snap}
	verifyScript, err := verifyJobScript(leg.template, outcome)
	if err != nil {
		t.Fatalf("verifyJobScript: %v", err)
	}
	verifyOut := runJobScript(t, ctx, toolsImage, network, verifyScript, repoVolume,
		"RESTIC_PASSWORD=probe-repo-pass", "RESTIC_REPOSITORY=/repo", "HOME=/tmp")
	t.Logf("leg %s verify ok: %s", leg.name, strings.TrimSpace(verifyOut))

	// 备份后新增行（重放后必须消失——证明恢复真重放而非空转）。
	run(t, ctx, "docker", "exec", serverName, "psql", "-U", "fleetly", "-d", dbtemplate.DatabaseName(instance), "-c",
		"insert into probe_t values (2, 'after-backup');")
	if leg.vector {
		run(t, ctx, "docker", "exec", serverName, "psql", "-U", "fleetly", "-d", dbtemplate.DatabaseName(instance), "-c",
			"insert into vec_t values (2, '[4,5,6]');")
	}

	// ── 恢复：停实例 → 挂数据卷重放 → 重启断言 ──
	run(t, ctx, "docker", "stop", serverName)
	restoreScript, err := restorePostgresJobScript(leg.template, dbtemplate.RestoreInput{
		Instance: instance, TemplateID: leg.template.ID, VolumeTarget: mountPath, SnapshotID: snap,
	})
	if err != nil {
		t.Fatalf("restorePostgresJobScript: %v", err)
	}
	restoreOut := runJobScriptWithMount(t, ctx, toolsImage, network, restoreScript, repoVolume, dataVolume, mountPath,
		"RESTIC_PASSWORD=probe-repo-pass", "RESTIC_REPOSITORY=/repo", "HOME=/tmp")
	t.Logf("leg %s restore ok: %s", leg.name, strings.TrimSpace(restoreOut))

	run(t, ctx, "docker", "rm", "-f", serverName)
	startServer()
	waitReady()
	rows := strings.TrimSpace(sql("select id || ':' || v from probe_t order by id"))
	if rows != "1:at-backup-time" {
		t.Fatalf("replayed data = %q, want exactly the backup-time row (post-backup row must be gone)", rows)
	}
	t.Logf("leg %s replayed rows: %s", leg.name, rows)
	if leg.vector {
		vecRows := strings.TrimSpace(sql("select count(*) from vec_t"))
		vec := strings.TrimSpace(sql("select v::text from vec_t where id=1"))
		ext := strings.TrimSpace(sql("select extversion from pg_extension where extname='vector'"))
		if vecRows != "1" || vec != "[1,2,3]" {
			t.Fatalf("replayed vector data = %s rows, %q (want 1 row and [1,2,3])", vecRows, vec)
		}
		t.Logf("leg %s replayed vector: rows=%s v=%s extversion=%s", leg.name, vecRows, vec, ext)
	}
}

// runJobScript 以 docker run 承载 job 脚本（repo 卷挂载；实例网络内按名可达）。
// 脚本切片 = adapter 的 Cmd 形态（["sh","-c",<script>]）——逐字作为容器
// 命令传入（与生产 JobRun 的 ContainerSpec.Cmd 同形）。
func runJobScript(t *testing.T, ctx context.Context, image, network string, script []string, repoVolume string, env ...string) string {
	t.Helper()
	args := []string{"run", "--rm", "--network", network, "-v", repoVolume + ":/repo"}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, image)
	args = append(args, script...)
	return run(t, ctx, "docker", args...)
}

// runJobScriptWithMount 同 runJobScript，另挂数据卷（恢复重放形态）。
func runJobScriptWithMount(t *testing.T, ctx context.Context, image, network string, script []string, repoVolume, dataVolume, mountPath string, env ...string) string {
	t.Helper()
	args := []string{"run", "--rm", "--network", network, "-v", repoVolume + ":/repo", "-v", dataVolume + ":" + mountPath}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, image)
	args = append(args, script...)
	return run(t, ctx, "docker", args...)
}

// tryRun 执行一条 docker 命令并返回合并输出（错误不致命——可用性探测用）。
func tryRun(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// run 执行一条 docker 命令并返回合并输出（失败即 Fatal——探针宁可快红）。
func run(t *testing.T, ctx context.Context, name string, args ...string) string {
	t.Helper()
	out, err := tryRun(ctx, name, args...)
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}
