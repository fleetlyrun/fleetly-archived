# internal/build — 构建管线（T2.8/T2.9；上传构建面 IMPL-T2-2/DT-6）

包内选型与硬约束见 `doc.go`；本文件只写实机复跑方法。

## 上传构建面（IMPL-T2-2/DT-6）

- 入口：`BuildsService.BuildFromUpload`（client-streaming，gRPC-only——无
  HTTP 注解；REST 面 404 是契约，CLI/SDK 消费）。SDK 访问器
  `fleetly.Client.BuildFromUpload`；CLI `fleetly builds upload --name <n>
  [--dockerfile <path>] <context.tar | ->` 与回读 `fleetly builds get <id>`。
- 落点/清理/配额：`build.Config.UploadsRoot`（缺省 `<数据根>/build-uploads`，
  经 Normalize 恒并入受管根）与 `MaxUploadBytes`（runtime 键
  `build.max_upload_mb`，缺省 256MiB）；解包安全与四层清理钩子见
  `upload.go` 头注释。
- 本机真机探针（默认不跑）：
  `FLEETLY_MANUAL_SWARM=1 go test -tags manual ./internal/api -run TestManualBuildFromUpload -v`
  （SDK 上传 → 自管 buildkitd → 本机 digest → 解析腿直通 → 引擎语义引用
  拉起 swarm 服务 running → 会话零残留 → 超限拒绝）。

## 单元测试（默认进 CI）

```bat
go test ./internal/build/ ./internal/substrate/ ./internal/state/ -race
```

覆盖：railpack/buildkit 钉版互钉、队列并发上限（假执行器）、E_BUILD_FAILED
信封、preflight 两分支（假镜像端口）、provenance/sbom 关闭参数断言
（buildopt_test.go）。

## 实机验证（不进 CI；本机 Docker 可用时）

### 1. preflight 真实 daemon 两分支

```bat
go test -tags fleetly_docker ./internal/substrate/ -run TestRealDaemonPreflight -v
```

用镜像 `moby/buildkit:v0.32.2`（平台自管 buildkitd 钉版镜像，跑过 fleetlyd
即存在；缺失自动 pull）。验证：可得 → digest 带出；打临时 tag → `rmi` →
preflight 缺失 → `E_IMAGE_UNAVAILABLE` + `W_ROLLBACK_IMAGE_RISK`。

### 2. 端到端真实构建（dind，生产同构拓扑）

fleetly v0.1 的宿主目标是 Linux；railpack 在 Windows 宿主上 plan 生成有
两处上游缺陷（mise zip 解包名不匹配、`filepath.Dir` 反斜杠泄入 LLB），
故端到端实机验收在 `docker:29.8.1-dind` 内做（与 e2e/smoke.sh 同拓扑）。

```bat
:: 1) 交叉编译 linux 二进制（CGO 关闭，modernc sqlite 纯 Go）
set GOOS=linux& set GOARCH=amd64& set CGO_ENABLED=0
go build -o <acc>\bin-linux\fleetlyd ./cmd/fleetlyd
go build -o <acc>\bin-linux\fleetly ./cmd/fleetly

:: 2) 验收目录（不在仓库根；bind mount 逐字节保真——spike/a FINDINGS #5）
docker run -d --name fleetly-acc-dind --privileged -v "<acc>:/work-src" docker:29.8.1-dind
docker exec -i fleetly-acc-dind sh -c "cat > /tmp/acc.sh" < <acc>\acc.sh
docker exec fleetly-acc-dind sh -c "sed -i 's/\r$//' /tmp/acc.sh"
docker exec fleetly-acc-dind sh /tmp/acc.sh

:: 3) 清理
docker rm -f fleetly-acc-dind
```

`acc.sh`（本次验收用脚本，要点）：
- dind 内起 fleetlyd（build.* 全缺省 → 自管 `fleetly-buildkit` 容器 +
  双缓存持久化：内部层缓存命名卷挂 /var/lib/buildkit、local cache 宿主
  目录 ./build-cache）；
- `fleetly build --service web my-api/compose.yaml` 两次（railpack 驱动；
  第二次吃缓存，对照耗时）；全量 compose 构建（web+worker 并行）；`fleetly
  build static-web/compose.yaml`（dockerfile 驱动）；`fleetly builds list
  --json my-api` 查 builds 表；
- 产物：`build-artifacts/<app>/<buildid>/build.log`（railpack 另有
  `railpack-plan.json`）；镜像 `fleetly-local/<app>:<app>-<buildid>` 落
  dind daemon。

## 缓存语义勘误（实机验证结论）

buildkit local cache 的导入/导出是**客户端侧**行为（经 solve session 流式
传输）——`src`/`dest` 路径解析在 fleetlyd 宿主，不在 buildkitd 容器内。
持久化因此分两层（config.go 头注释）：`cache_volume` 命名卷挂
`/var/lib/buildkit` 保 buildkitd 内部层缓存跨容器重建；`cache_dir` 宿主
目录为 local cache 数据根（跨重建/跨实例可移植）。

## 上游缺陷备忘（Windows 宿主 railpack，v0.39.0）

1. `core/mise` 的 Windows zip 解包按 `mise-<版本>.exe` 找文件，mise 发布
   zip 内是 `mise/bin/mise.exe` → 下载安装必失败。宿主侧绕过：把 mise.exe
   预置到 `filepath.Join("/tmp/railpack/mise", "mise-"+版本+".exe")`（当前
   盘符根 `\tmp\railpack\mise\`）。
2. `buildkit/build_llb` 的 FileCommand 转换用 `filepath.Dir(cmd.Path)`，
   Windows 编译目标产出 `\etc\mise` 反斜杠路径进 LLB → solve 报
   `open /etc/mise/config.toml: no such file or directory`。
两条均不影响 linux 宿主（v0.1 生产形态）。
