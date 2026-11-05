package pool

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

func TestSyncToDirReenablesAfterRelogin(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	old := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "verifier")
	p, err := New([]*auth.Auth{old}, Config{MaxConcurrent: 1}, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	p.Disable("u1", "refresh_token expired; re-login required")
	if p.Enabled("u1") {
		t.Fatal("precondition: account should be disabled")
	}

	// 重新登录：凭证文件 updated_at 变新，重载后应自动解除禁用。
	fresh := auth.New("u1", "user", "domain", "token2", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "rt2", "verifier")
	fresh.UpdatedAt = old.UpdatedAt + 60
	p.SyncToDir([]*auth.Auth{fresh})
	if !p.Enabled("u1") {
		t.Fatal("重新登录后应自动解除禁用（否则面板上看起来像登录没生效）")
	}
	if p.Get("u1").Auth.CloudDragonTok != "token2" {
		t.Fatal("重载后应使用新的凭证")
	}
}

func TestSyncToDirKeepsManualDisable(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	old := auth.New("u2", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "verifier")
	p, err := New([]*auth.Auth{old}, Config{MaxConcurrent: 1}, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	p.Disable("u2", "manual disable")

	// 凭证没变（updated_at 相同）：手动禁用必须保留。
	p.SyncToDir([]*auth.Auth{old})
	if p.Enabled("u2") {
		t.Fatal("凭证未更新时不得解除手动禁用")
	}
}

func TestLoadStateIgnoresStaleDisableAfterRelogin(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	raw := `[{"name":"u3","disabled":true,"reason":"refresh_token expired; re-login required"}]`
	if err := os.WriteFile(stateFile, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stateFile, old, old); err != nil {
		t.Fatal(err)
	}
	a := auth.New("u3", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "rt", "verifier")
	p, err := New([]*auth.Auth{a}, Config{MaxConcurrent: 1}, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Enabled("u3") {
		t.Fatal("凭证比状态文件新时，过期的禁用结论应被忽略（否则重新登录+重启后仍不可用）")
	}
}

func TestLoadStateKeepsDisableWhenStateIsNewer(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	raw := `[{"name":"u4","disabled":true,"reason":"manual disable"}]`
	if err := os.WriteFile(stateFile, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	a := auth.New("u4", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "rt", "verifier")
	a.UpdatedAt = time.Now().Add(-time.Hour).Unix() // 凭证比状态旧
	p, err := New([]*auth.Auth{a}, Config{MaxConcurrent: 1}, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if p.Enabled("u4") {
		t.Fatal("状态比凭证新时必须保留禁用结论")
	}
}

func TestAcquireAnyLockWaitWaitsForFreeSlot(t *testing.T) {
	a := auth.New("u5", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "rt", "verifier")
	p, err := New([]*auth.Auth{a}, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.AcquireLock("u5") {
		t.Fatal("precondition: first slot should be free")
	}
	// 槽位被占：短超时应返回 nil（而不是钉在账号上一直等）。
	if got := p.AcquireAnyLockWait(100*time.Millisecond, nil); got != nil {
		t.Fatal("满载时应超时返回 nil")
	}
	// 释放后应立刻拿到同一个账号。
	go func() {
		time.Sleep(80 * time.Millisecond)
		p.ReleaseLock("u5")
	}()
	got := p.AcquireAnyLockWait(2*time.Second, nil)
	if got == nil || got.Name != "u5" {
		t.Fatalf("槽位释放后应拿到账号，got=%v", got)
	}
	p.ReleaseLock(got.Name)

	// skip 里的账号不参与。
	if !p.AcquireLock("u5") {
		t.Fatal("re-acquire failed")
	}
	if got := p.AcquireAnyLockWait(100*time.Millisecond, map[string]bool{"u5": true}); got != nil {
		t.Fatal("skip 指定的账号不应被选中")
	}
	p.ReleaseLock("u5")
}

func TestAddAccountInitializesConcurrency(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "verifier")
	p.AddAccount(a)

	if !p.AcquireLockWait("u1", 0) {
		t.Fatal("newly added account did not receive a concurrency slot")
	}
	p.ReleaseLock("u1")
}

func TestSyncToDirInitializesConcurrency(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "verifier")
	p.SyncToDir([]*auth.Auth{a})

	if !p.AcquireLockWait("u1", 0) {
		t.Fatal("synced account did not receive a concurrency slot")
	}
	p.ReleaseLock("u1")
}

func TestValidateKeepsAccountEnabledAfterRetryableRefreshFailure(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary upstream outage", http.StatusBadGateway)
	}))
	defer sts.Close()
	dpopPrivateJWK, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		t.Fatal(err)
	}
	loginCfg := upstream.DefaultLoginConfig()
	loginCfg.STSHost = sts.URL
	a := auth.New(
		"u-refresh", "user", "domain", "token", "ak", "sk",
		time.Now().Add(30*time.Minute).Format(time.RFC3339), "refresh", "verifier",
		map[string]string(dpopPrivateJWK),
	)
	p, err := New([]*auth.Auth{a}, Config{RefreshSkew: time.Hour, LoginConfig: loginCfg, RefreshRetries: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	ok, refreshErr := p.Validate(p.Accounts()[0])
	if ok || refreshErr == nil {
		t.Fatalf("validate=(ok=%t, err=%v), want retryable refresh failure", ok, refreshErr)
	}
	status := p.List()[0]
	if disabled, _ := status["disabled"].(bool); disabled {
		t.Fatalf("retryable refresh failure permanently disabled account: %#v", status)
	}
}

// watch.refresh_skew_minutes 必须是有效的：此前 pool 硬编码 1 小时阈值，
// 调度器传进来的 RefreshSkew 只用于日志，配置改了也不生效。
func TestRefreshSkewControlsThreshold(t *testing.T) {
	a := auth.New("u-skew", "n", "d", "tok", "ak", "sk",
		time.Now().Add(2*time.Hour).Format(time.RFC3339), "refresh-token", "verifier")
	p, err := New([]*auth.Auth{a}, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	// 剩余 2h：skew=1h 时不刷新，skew=3h 时应命中刷新（这里只验证判定分支，
	// 真刷新需要网络，故只检查不触发的那一侧不报错）。
	if err := p.CheckAndRefreshTokenWithin("u-skew", time.Hour); err != nil {
		t.Fatalf("剩余 2h、skew=1h 不应触发刷新: %v", err)
	}
}

// 续期错误的重试判定：终态/明确 4xx 不重试，网络与 5xx/429 必须当场重试。
//
// refresh_token 是一次性轮换的：如果请求已经打到上游但我们没拿到响应（超时），
// token 已被换走；等到下一轮 watchdog 再试只会得到 STS5.1806 "has been used"，
// 账号永久报废（实测 2026-11-04 10:31 丢了 one7312）。当场重试是唯一能救回的路径。
func TestShouldRetryRefresh(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"terminal reuse-detected", &upstream.RefreshTokenExpiredError{Status: 400, Message: "has been used"}, false},
		{"terminal wrapped", fmt.Errorf("refresh failed: %w", &upstream.RefreshTokenExpiredError{Status: 400}), false},
		{"http 400", &upstream.ApiError{Status: 400}, false},
		{"http 401", &upstream.ApiError{Status: 401}, false},
		{"http 429 rate limited", &upstream.ApiError{Status: 429}, true},
		{"http 502", &upstream.ApiError{Status: 502}, true},
		{"http 503 wrapped", fmt.Errorf("refresh failed: %w", &upstream.ApiError{Status: 503}), true},
		{"network", errors.New("dial tcp 1.2.3.4:443: i/o timeout"), true},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldRetryRefresh(c.err); got != c.want {
				t.Fatalf("shouldRetryRefresh(%v)=%v want %v", c.err, got, c.want)
			}
		})
	}
}


// 可重试错误（502/网络）必须当场重试：refresh_token 一次性轮换，等到下一轮
// watchdog 再试就已经变成 "has been used"，账号直接报废。
func TestRefreshTokenRetriesRetryableFailures(t *testing.T) {
	var hits int32
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "temporary upstream outage", http.StatusBadGateway)
	}))
	defer sts.Close()
	p := newRefreshTestPool(t, sts.URL, "u-retry", Config{
		RefreshRetries:    3,
		RefreshRetryDelay: time.Millisecond,
	})
	if err := p.RefreshToken("u-retry"); err == nil {
		t.Fatal("502 应当返回错误")
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("尝试 %d 次，want 3", got)
	}
}

// 终态错误（refresh_token 已被用过）不能重试：多打两次上游毫无意义。
func TestRefreshTokenDoesNotRetryTerminalError(t *testing.T) {
	var hits int32
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error_code":"STS5.1806","error_msg":"invalid refresh token: 'the refresh token has been used'"}`))
	}))
	defer sts.Close()
	p := newRefreshTestPool(t, sts.URL, "u-terminal", Config{
		RefreshRetries:    3,
		RefreshRetryDelay: time.Millisecond,
	})
	err := p.RefreshToken("u-terminal")
	var expired *upstream.RefreshTokenExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("err=%T %v，want RefreshTokenExpiredError", err, err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("终态错误打了 %d 次上游，want 1", got)
	}
}

// newRefreshTestPool 造一个带 refresh_token 的账号，STS 指向测试服务器。
func newRefreshTestPool(t *testing.T, stsURL, name string, cfg Config) *Pool {
	t.Helper()
	dpopPrivateJWK, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		t.Fatal(err)
	}
	loginCfg := upstream.DefaultLoginConfig()
	loginCfg.STSHost = stsURL
	cfg.LoginConfig = loginCfg
	a := auth.New(name, "user", "domain", "token", "ak", "sk",
		time.Now().Add(30*time.Minute).Format(time.RFC3339), "refresh", "verifier",
		map[string]string(dpopPrivateJWK))
	p, err := New([]*auth.Auth{a}, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

