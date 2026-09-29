package runtime

// gateway JSON 投影钉测（2026-09-29 用户裁决「EmitUnpopulated 改为 true」）：
// REST 面零值字段显式输出——false/0/"" 不再省略，unset message 字段为 null。
// 此前 false 口径下「字段缺省」与「值就是零」不可区分，Console 真机验收两次
// 误判（suspended:false 缺席被当成投影缺失）。本文件钉死新口径不回漂。

import (
	"strings"
	"testing"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	sharedv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/shared/v1"
)

func TestGatewayJSONEmitsUnpopulated(t *testing.T) {
	m := newJSONMarshaler()

	// 标量零值显式：suspended=false 必须出现在输出中（本轮裁决的引子）。
	app, err := m.Marshal(&serverv1.AppView{Id: "a1", Name: "demo"})
	if err != nil {
		t.Fatalf("marshal AppView: %v", err)
	}
	for _, want := range []string{`"suspended":false`, `"derived_state":""`, `"team_slug":""`} {
		if !strings.Contains(string(app), want) {
			t.Fatalf("AppView JSON missing %s (zero values must be emitted): %s", want, app)
		}
	}

	// 错误信封同口径：空 code/suggestion 显式输出（413 信封复用本 marshaler）。
	env, err := m.Marshal(&sharedv1.ErrorResponse{Message: "boom"})
	if err != nil {
		t.Fatalf("marshal ErrorResponse: %v", err)
	}
	for _, want := range []string{`"code":""`, `"message":"boom"`} {
		if !strings.Contains(string(env), want) {
			t.Fatalf("ErrorResponse JSON missing %s: %s", want, env)
		}
	}

	// 入向不受影响：DiscardUnknown 兼容客户端多发字段。
	var back sharedv1.ErrorResponse
	if err := (m.UnmarshalOptions).Unmarshal([]byte(`{"message":"m","future_field":1}`), &back); err != nil {
		t.Fatalf("unmarshal with unknown field: %v", err)
	}
	if back.Message != "m" {
		t.Fatalf("roundtrip message = %q", back.Message)
	}
}
