package api

// E_APP_AMBIGUOUS 标准构造的守卫测试（IMPL-ARCH-L）。两件事：
//
//  1. ambiguousRefErr 的产出钉死——注册码 / HTTP 400 / 两族（app/database）
//     的标准文案逐字比对、引用值与候选列两类动态信息都在（候选列构造函数
//     内排序；空候选列不投影 candidates 键，非基准站点的信封形态保持）；
//  2. 源码扫描守卫——internal/api 非测试代码不得再手写 E_APP_AMBIGUOUS
//     字面量，白名单 = 构造函数本体（registration_test 的 labelscan 同款
//     idiom：清单与代码双向钉死，白名单失配即红）。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/apperr"
)

func TestAmbiguousRefErrEnvelope(t *testing.T) {
	// app 族：引用值进 detail 与 context，候选列构造函数内排序后逗号连接。
	err := ambiguousRefErr("app", "team/prj/app", "web", []string{"beta/beta/web", "acme/acme/web"})
	e, ok := apperr.FromError(err)
	if !ok {
		t.Fatalf("ambiguousRefErr = %v, want apperr envelope", err)
	}
	if e.Code() != "E_APP_AMBIGUOUS" {
		t.Fatalf("code = %q, want E_APP_AMBIGUOUS", e.Code())
	}
	if got := e.HTTPStatus(); got != 400 {
		t.Fatalf("http status = %d, want 400 (registry projection unchanged)", got)
	}
	wantMessage := `app "web" resolves to multiple rows across projects; use the team/prj/app qualified form`
	if e.Message() != wantMessage {
		t.Fatalf("message = %q, want %q", e.Message(), wantMessage)
	}
	if got := e.Context()["app"]; got != "web" {
		t.Fatalf("context[app] = %q, want %q", got, "web")
	}
	if got := e.Context()["candidates"]; got != "acme/acme/web,beta/beta/web" {
		t.Fatalf("context[candidates] = %q, want sorted comma join", got)
	}

	// database 族：kindName 换词即库族文案与 context 键（六站统一句式）。
	dbErr := ambiguousRefErr("database", "team/prj/db", "pg", nil)
	de, ok := apperr.FromError(dbErr)
	if !ok {
		t.Fatalf("ambiguousRefErr(database) = %v, want apperr envelope", dbErr)
	}
	if de.Code() != "E_APP_AMBIGUOUS" {
		t.Fatalf("database code = %q, want E_APP_AMBIGUOUS", de.Code())
	}
	wantDatabase := `database "pg" resolves to multiple rows across projects; use the team/prj/db qualified form`
	if de.Message() != wantDatabase {
		t.Fatalf("database message = %q, want %q", de.Message(), wantDatabase)
	}
	if got := de.Context()["database"]; got != "pg" {
		t.Fatalf("context[database] = %q, want %q", got, "pg")
	}
	// 空候选列不投影 candidates 键（非基准站点的信封形态保持原样）。
	if _, present := de.Context()["candidates"]; present {
		t.Fatalf("context[candidates] present for empty candidate list, want absent")
	}
}

// appAmbiguousLiteralWhitelist 是允许出现 "E_APP_AMBIGUOUS" 字符串字面量的
// 非测试文件（相对 internal/ 的斜杠路径）与一行理由。白名单必须随代码演进
// 收缩/修正——某文件不再命中模式时本测试会提示清理（清单与代码双向钉死）。
var appAmbiguousLiteralWhitelist = map[string]string{
	"api/errors.go": "ambiguousRefErr 构造函数本体：该注册码 detail 的唯一构造点（IMPL-ARCH-L）",
}

func TestNoStrayAppAmbiguousLiterals(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	internalRoot := filepath.Dir(filepath.Dir(thisFile)) // <repo>/internal

	violations := []string{}
	whitelistHits := map[string]bool{}
	scanErr := filepath.WalkDir(filepath.Join(internalRoot, "api"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(internalRoot, path)
		if relErr != nil {
			return relErr
		}
		relSlash := filepath.ToSlash(rel)
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			// 只匹配字符串字面量（注释里的码名引用不误报——守卫钉的是
			// detail 产出点，不是文档措辞）。
			lit, isLit := n.(*ast.BasicLit)
			if !isLit || lit.Kind != token.STRING || lit.Value != `"E_APP_AMBIGUOUS"` {
				return true
			}
			whitelistHits[relSlash] = true
			if _, allowed := appAmbiguousLiteralWhitelist[relSlash]; !allowed {
				violations = append(violations, fmt.Sprintf("%s at %s", relSlash, fset.Position(lit.Pos())))
			}
			return true
		})
		return nil
	})
	if scanErr != nil {
		t.Fatalf("scan internal/api: %v", scanErr)
	}

	for file, reason := range appAmbiguousLiteralWhitelist {
		if !whitelistHits[file] {
			t.Errorf("whitelist entry %s (%s) no longer matches any literal — prune the entry", file, reason)
		}
	}
	if len(violations) > 0 {
		t.Errorf("E_APP_AMBIGUOUS literal outside ambiguousRefErr in internal/api (the constructor in internal/api/errors.go is the single detail source; see IMPL-ARCH-L):")
		for _, v := range violations {
			t.Errorf("  %s", v)
		}
	}
}
