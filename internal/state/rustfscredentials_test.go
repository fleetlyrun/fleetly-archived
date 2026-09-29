package state

import (
	"context"
	"testing"
)

// 托管 RustFS root 凭据的存储面测试（E3-5，resticpassword.go 同型）：
// 两键原子的存取/清场、半份密文的损坏检测（loud-fail 不静默再生成）、
// 内部键不泄进 s3.* typed 设置面（负面测试，八键纪律）。

// TestRustfsCredentialsRoundtrip 存取与清场：未生成 found=false；保存后
// 原样读回（state 层存密文不解释）；覆写幂等；删除后回到未生成态。
func TestRustfsCredentialsRoundtrip(t *testing.T) {
	st := newSettingsStore(t)
	ctx := context.Background()

	if _, _, found, err := st.LoadRustfsCredentialsCiphertext(ctx); err != nil || found {
		t.Fatalf("fresh store: found=%v err=%v, want false/nil", found, err)
	}
	if err := st.SaveRustfsCredentialsCiphertext(ctx, "ct-access", "ct-secret"); err != nil {
		t.Fatalf("SaveRustfsCredentialsCiphertext: %v", err)
	}
	a, k, found, err := st.LoadRustfsCredentialsCiphertext(ctx)
	if err != nil || !found || a != "ct-access" || k != "ct-secret" {
		t.Fatalf("roundtrip a=%q k=%q found=%v err=%v, want stored pair", a, k, found, err)
	}
	// 覆写（轮换路径：管理器再生成后整对替换）。
	if err := st.SaveRustfsCredentialsCiphertext(ctx, "ct-access-2", "ct-secret-2"); err != nil {
		t.Fatalf("SaveRustfsCredentialsCiphertext (rotate): %v", err)
	}
	if a, k, _, _ := st.LoadRustfsCredentialsCiphertext(ctx); a != "ct-access-2" || k != "ct-secret-2" {
		t.Fatalf("rotated pair = %q/%q, want replaced", a, k)
	}
	// 空密文显式拒绝（防半份空条目）。
	if err := st.SaveRustfsCredentialsCiphertext(ctx, "ct", ""); err == nil {
		t.Fatal("empty secret ciphertext must be rejected")
	}
	// 清场（mode 离开 rustfs）→ 回到未生成态；幂等。
	if err := st.DeleteRustfsCredentialsCiphertext(ctx); err != nil {
		t.Fatalf("DeleteRustfsCredentialsCiphertext: %v", err)
	}
	if _, _, found, err := st.LoadRustfsCredentialsCiphertext(ctx); err != nil || found {
		t.Fatalf("after delete: found=%v err=%v, want false/nil", found, err)
	}
	if err := st.DeleteRustfsCredentialsCiphertext(ctx); err != nil {
		t.Fatalf("DeleteRustfsCredentialsCiphertext (idempotent): %v", err)
	}
}

// TestRustfsCredentialsPartialCorruption 半份密文 = 存储损坏：一存在一缺失
// → 显式报错，不静默回落「未生成」（覆盖写会掩盖现场——loud-fail 纪律）。
func TestRustfsCredentialsPartialCorruption(t *testing.T) {
	st := newSettingsStore(t)
	ctx := context.Background()
	if err := st.SaveRustfsCredentialsCiphertext(ctx, "ct-access", "ct-secret"); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 直接删一个键（模拟磁盘/库损坏）。
	if err := st.InTx(ctx, func(tx *Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM platform_settings WHERE key = ?`, S3KeyRustfsSecretKey)
		return err
	}); err != nil {
		t.Fatalf("delete one key: %v", err)
	}
	if _, _, _, err := st.LoadRustfsCredentialsCiphertext(ctx); err == nil {
		t.Fatal("partial credentials must fail loudly (possible store corruption), not report not-found")
	}
}

// TestRustfsCredentialsAreInternalKeys 负面测试（设计 §2.5 内部键，与
// s3.restic_password 同纪律）：凭据键不在 LoadS3Settings 的 typed 八键
// 投影；SaveS3Settings 的 PUT 全量落库不触碰凭据条目（不残留、不覆写
// ——用户设置面与平台内部凭据是两个语义族，物理复用一张表而已）。
func TestRustfsCredentialsAreInternalKeys(t *testing.T) {
	st := newSettingsStore(t)
	ctx := context.Background()
	if err := st.SaveRustfsCredentialsCiphertext(ctx, "ct-access", "ct-secret"); err != nil {
		t.Fatalf("SaveRustfsCredentialsCiphertext: %v", err)
	}
	// PUT 一轮合法 rustfs 设置（外部四字段必须为空）。
	if err := st.SaveS3Settings(ctx, S3Settings{Mode: S3ModeRustfs}, S3SaveOptions{Actor: "human"}); err != nil {
		t.Fatalf("SaveS3Settings(rustfs): %v", err)
	}
	got, err := st.LoadS3Settings(ctx)
	if err != nil {
		t.Fatalf("LoadS3Settings: %v", err)
	}
	// 八键投影与托管凭据无关（typed 视图天然不含它——本断言钉住结构面）。
	if got.Mode != S3ModeRustfs || got.EndpointURL != "" || got.Bucket != "" ||
		got.AccessKeyID != "" || got.SecretAccessKey != "" {
		t.Fatalf("typed settings drifted: %+v", got)
	}
	a, k, found, err := st.LoadRustfsCredentialsCiphertext(ctx)
	if err != nil || !found || a != "ct-access" || k != "ct-secret" {
		t.Fatalf("credentials after PUT save: found=%v a=%q k=%q err=%v, want untouched", found, a, k, err)
	}
	// 凭据键不是用户设置键：s3SettingsKeys 全量键集不含它。
	for _, key := range s3SettingsKeys {
		if key == S3KeyRustfsAccessKey || key == S3KeyRustfsSecretKey {
			t.Fatalf("internal key %s leaked into the user-facing PUT key set", key)
		}
	}
}
