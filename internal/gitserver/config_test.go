package gitserver

import (
	"testing"
)

// 配置面的单元断言。

func TestConfigNormalize(t *testing.T) {
	cfg := Config{}.Normalize()
	if cfg.ReplayTTL != DefaultReplayTTL {
		t.Errorf("ReplayTTL = %s, want %s", cfg.ReplayTTL, DefaultReplayTTL)
	}
	if err := (Config{}).Validate(); err == nil {
		t.Error("empty root accepted")
	}
	if err := (Config{Root: "/x"}).Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}
