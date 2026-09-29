package dockerapi

// moby client 连接面的源码扫描守卫（convergescan_test.go / labelscan_test.go
// 同款 idiom）：internal/ 下所有 .go 文件（含测试）对 github.com/moby/moby/
// client 的导入只允许出现在白名单四包——Docker API 连接的持有纪律从包名
// 暗示（2026-09-29 C1 前六包各持一份逐字拷贝的 realDockerClient）升级为
// 测试门禁。新包需要直连 Docker API 时先扩白名单并写明理由（评审口径），
// 默认路径是消费本包原语。
//
// 只管 moby/moby/client（连接面）：moby swarm/mount/container 等纯类型导
// 入不在此列——构造载荷「只进不出」是既定形态（本包包注释）；build 子系
// 统的 github.com/moby/buildkit/* 是另一张消费脸，不在本守卫管辖。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// mobyClientImport 是被守卫的导入路径（Docker API 连接面）。
const mobyClientImport = `"github.com/moby/moby/client"`

// mobyClientWhitelist 是允许持有 moby client 连接的包（相对 internal/ 的
// 目录名）与一行理由。白名单必须随代码演进收缩/修正——某包不再导入时本
// 测试会提示清理（清单与代码双向钉死）。
var mobyClientWhitelist = map[string]string{
	"substrate": "引擎侧受管服务消费面的既有 moby 网关（engine.Substrate 端口实现，C1 裁定的两处之一）",
	"dockerapi": "受管组件部署器/收敛循环的共享消费面（2026-09-29 C1 收编单点，本包）",
	"database":  "一次性领域执行体（ContainerRun/JobRun）自留连接——C1 明确不属共享包的领域原语",
	"execrelay": "relay exec 面（终端容器执行体 ExecCreate/Start）自有 Docker 面——C1 零触碰的独立面，与部署收敛面无关",
}

// TestMobyClientConfinedToGateways 扫描 internal/ 全部 .go 文件：白名单外
// 的包导入 github.com/moby/moby/client 即红（Docker API 连接不得绕开网关
// 自建——拷贝漂移的温床正是 C1 要消灭的形态）。
func TestMobyClientConfinedToGateways(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// <repo>/internal/dockerapi → <repo>/internal
	internalRoot := filepath.Dir(filepath.Dir(thisFile))

	violations := map[string][]string{} // 包名 → 命中文件
	whitelistHits := map[string]bool{}
	walkErr := filepath.WalkDir(internalRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		raw, rerr := os.ReadFile(path) //nolint:gosec // G304：扫描本仓源码树，路径自 WalkDir
		if rerr != nil {
			return rerr
		}
		// 行尾归一到 LF：Windows 检出（core.autocrlf=true）把磁盘文件写成
		// CRLF——判定不归一则结果随检出环境漂移（labelscan_test.go 同款教训）。
		src := strings.ReplaceAll(string(raw), "\r\n", "\n")
		if !strings.Contains(src, mobyClientImport) {
			return nil
		}
		pkg := filepath.Base(filepath.Dir(path)) // 相对 internal/ 的包目录名
		whitelistHits[pkg] = true
		if _, allowed := mobyClientWhitelist[pkg]; !allowed {
			violations[pkg] = append(violations[pkg],
				strings.TrimPrefix(strings.TrimPrefix(path, internalRoot+string(filepath.Separator)), "internal"+string(filepath.Separator)))
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk internal sources: %v", walkErr)
	}

	for pkg, files := range violations {
		t.Errorf("package %s imports github.com/moby/moby/client outside the gateways (consume internal/dockerapi primitives or extend the whitelist with a reason):", pkg)
		for _, f := range files {
			t.Errorf("  %s", f)
		}
	}
	// 白名单保鲜：条目不再命中即提示收缩（清单与代码一致纪律）。
	for pkg, reason := range mobyClientWhitelist {
		if !whitelistHits[pkg] {
			t.Errorf("whitelist entry %s no longer matches (remove it or fix the reason: %s)", pkg, reason)
		}
	}
}
