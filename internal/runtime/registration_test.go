package runtime

// 服务登记表的守卫测试（IMPL-ARCH-I，labelscan_test.go 同款 idiom）：
//
//  1. TestServiceRegistrationsWellFormed——表形状（非空/名非空且唯一）+
//     「表 ⇆ NewGRPCServer 签名」结构断言：生产装配函数的服务参数个数
//     必须等于表条目数（新增服务漏加表条目或漏加装配参数都即红——平行
//     清单收敛后，这张表就是服务面的唯一清单，漂移在提交期拦截）；
//  2. TestNoStrayServiceRegistrations——源码扫描：internal/runtime 与
//     internal/apitest 的非测试 .go 文件不得出现登记表外的
//     serverv1.RegisterXxx 调用（手写清单的复发形态 = 第三份平行清单）。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// newGRPCServerNonServiceParams 是 NewGRPCServer 签名中**非服务**装配参数
// 的个数（lynx.App、*AppConfig、*ControlPlaneTLS、*api.Authenticator）。
// 签名演进时同步维护本常数——断言失配本身即红，提示人工复核表 ⇆ 装配集。
const newGRPCServerNonServiceParams = 4

func TestServiceRegistrationsWellFormed(t *testing.T) {
	if len(ServiceRegistrations) == 0 {
		t.Fatal("ServiceRegistrations is empty (registration table must cover the control-plane service face)")
	}
	seen := map[string]bool{}
	for i, entry := range ServiceRegistrations {
		name := entry.serviceName()
		if name == "" {
			t.Fatalf("ServiceRegistrations[%d] has an empty service name", i)
		}
		if seen[name] {
			t.Fatalf("service %s registered more than once in ServiceRegistrations", name)
		}
		seen[name] = true
	}

	// 表 ⇆ 生产装配签名：服务参数个数必须与表条目数一致（结构断言——
	// 注册顺序在表序单点，本断言只钉「个数不漂移」；名对位与类型对位由
	// RegisterGRPCServices 的双向对账在构造期兜底）。
	fnType := reflect.TypeOf(NewGRPCServer)
	if fnType.Kind() != reflect.Func {
		t.Fatalf("NewGRPCServer is not a function (%v)", fnType.Kind())
	}
	serviceParams := fnType.NumIn() - newGRPCServerNonServiceParams
	if serviceParams != len(ServiceRegistrations) {
		t.Fatalf("NewGRPCServer takes %d service params (num in %d minus %d non-service params) but ServiceRegistrations has %d entries — the table and the production assembly have drifted",
			serviceParams, fnType.NumIn(), newGRPCServerNonServiceParams, len(ServiceRegistrations))
	}
}

// TestGRPCAssemblyKeysMatchTable 断言 grpc.go 生产装配集的键集 ≡ 登记表
// 服务名集（双向）。NewGRPCServer 只在守护进程启动时执行（不在测试内），
// 反射个数断言钉不住键名手误（错名实例 = 启动期对账红，比静默缺面好，
// 但提交期就该红）——此处经 AST 直读装配集字面量，把对账前移到 CI。
func TestGRPCAssemblyKeysMatchTable(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	grpcSource, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "grpc.go"))
	if err != nil {
		t.Fatalf("read grpc.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "grpc.go", grpcSource, 0)
	if err != nil {
		t.Fatalf("parse grpc.go: %v", err)
	}
	tableNames := map[string]bool{}
	for _, entry := range ServiceRegistrations {
		tableNames[entry.serviceName()] = true
	}

	var assemblyKeys map[string]bool
	ast.Inspect(file, func(n ast.Node) bool {
		assign, isAssign := n.(*ast.AssignStmt)
		if !isAssign || len(assign.Rhs) != 1 {
			return true
		}
		lhs, lhsIsIdent := assign.Lhs[0].(*ast.Ident)
		if !lhsIsIdent || lhs.Name != "instances" {
			return true
		}
		composite, isComposite := assign.Rhs[0].(*ast.CompositeLit)
		if !isComposite {
			return true
		}
		mapType, isMap := composite.Type.(*ast.MapType)
		if !isMap {
			return true
		}
		if keyIdent, keyIsIdent := mapType.Key.(*ast.Ident); !keyIsIdent || keyIdent.Name != "string" {
			// map 键类型不是 string（本仓无此形态）——跳过而非误配。
			return true
		}
		keys := map[string]bool{}
		for _, elt := range composite.Elts {
			kv, isKV := elt.(*ast.KeyValueExpr)
			if !isKV {
				continue
			}
			keyLit, keyIsLit := kv.Key.(*ast.BasicLit)
			if !keyIsLit || keyLit.Kind != token.STRING {
				continue
			}
			unquoted, uerr := strconv.Unquote(keyLit.Value)
			if uerr != nil {
				continue
			}
			keys[unquoted] = true
		}
		if len(keys) > len(assemblyKeys) {
			assemblyKeys = keys
		}
		return true
	})
	if assemblyKeys == nil {
		t.Fatal("no `instances := map[…]` composite literal found in grpc.go (assembly shape changed — update this guard)")
	}
	for name := range tableNames {
		if !assemblyKeys[name] {
			t.Errorf("table service %s has no key in the grpc.go assembly set (production assembly is behind the table)", name)
		}
	}
	for name := range assemblyKeys {
		if !tableNames[name] {
			t.Errorf("grpc.go assembly key %q is not in ServiceRegistrations (typo or missing table entry)", name)
		}
	}
}

// registrationScanDirs 是源码扫描的目录（相对 internal/ 的斜杠路径）。
var registrationScanDirs = []string{"runtime", "apitest"}

// serviceRegistrationWhitelist 是允许出现 serverv1.RegisterXxx 的非测试
// 文件（相对 internal/ 的斜杠路径）与一行理由。白名单必须随代码演进收缩/
// 修正——某文件不再命中模式时本测试会提示清理（清单与代码双向钉死）。
var serviceRegistrationWhitelist = map[string]string{
	"runtime/registration.go": "登记表本体：register 函数与 gateway 挂载函数的唯一登记点（ServiceRegistrations）",
}

func TestNoStrayServiceRegistrations(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	internalRoot := filepath.Dir(filepath.Dir(thisFile)) // <repo>/internal

	violations := map[string][]string{} // 相对路径 → 命中行
	whitelistHits := map[string]bool{}
	scanOne := func(relDir string) error {
		return filepath.WalkDir(filepath.Join(internalRoot, filepath.FromSlash(relDir)), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel(internalRoot, path)
			if rerr != nil {
				return rerr
			}
			relSlash := filepath.ToSlash(rel)
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			ast.Inspect(file, func(n ast.Node) bool {
				// 匹配 serverv1.RegisterXxx 的**引用**而非仅调用：登记表
				// 本体把 register 函数作值传入构造助手（不是调用点），
				// 手写清单的两种复发形态（直接调用 / 函数值搬运）都要红。
				sel, isSel := n.(*ast.SelectorExpr)
				if !isSel {
					return true
				}
				ident, isIdent := sel.X.(*ast.Ident)
				if !isIdent || ident.Name != "serverv1" {
					return true
				}
				if !strings.HasPrefix(sel.Sel.Name, "Register") {
					return true
				}
				whitelistHits[relSlash] = true
				if _, allowed := serviceRegistrationWhitelist[relSlash]; !allowed {
					violations[relSlash] = append(violations[relSlash],
						fmt.Sprintf("%s at %s", sel.Sel.Name, fset.Position(sel.Pos())))
				}
				return true
			})
			return nil
		})
	}
	for _, dir := range registrationScanDirs {
		if err := scanOne(dir); err != nil {
			t.Fatalf("scan internal/%s: %v", dir, err)
		}
	}

	for file, hits := range violations {
		t.Errorf("service registration outside ServiceRegistrations in %s (the table in internal/runtime/registration.go is the single service-face list; see IMPL-ARCH-I):", file)
		for _, h := range hits {
			t.Errorf("  %s", h)
		}
	}
	// 白名单保鲜：条目不再命中模式即提示收缩（清单与代码一致纪律）。
	for file, reason := range serviceRegistrationWhitelist {
		if !whitelistHits[file] {
			t.Errorf("whitelist entry %s no longer matches (remove it or fix the reason: %s)", file, reason)
		}
	}
}
