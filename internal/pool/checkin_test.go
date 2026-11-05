package pool

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

type checkinClientFunc func(context.Context, upstream.SignCredential) (upstream.CheckinResult, error)

func (f checkinClientFunc) Checkin(ctx context.Context, cred upstream.SignCredential) (upstream.CheckinResult, error) {
	return f(ctx, cred)
}

func TestCheckinSerializesAndKeepsInferenceHealthy(t *testing.T) {
	var calls, active, peak atomic.Int32
	client := checkinClientFunc(func(ctx context.Context, cred upstream.SignCredential) (upstream.CheckinResult, error) {
		calls.Add(1)
		n := active.Add(1)
		if n > peak.Load() {
			peak.Store(n)
		}
		defer active.Add(-1)
		if cred.SecurityToken != "token" || cred.AccessKeyID != "ak" {
			t.Error("wrong credential snapshot")
		}
		time.Sleep(10 * time.Millisecond)
		return upstream.CheckinResult{}, errors.New("activity unavailable")
	})
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := New([]*auth.Auth{a}, Config{ErrThreshold: 1, CheckinClient: client}, "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = p.Checkin(context.Background(), "u1") }()
	}
	wg.Wait()
	if calls.Load() != 3 || peak.Load() != 1 {
		t.Fatalf("calls=%d peak=%d", calls.Load(), peak.Load())
	}
	st := p.List()[0]
	if !p.Healthy("u1") || st["err_count"] != 0 || st["active_concurrent"] != 0 {
		t.Fatalf("activity error damaged inference: %+v", st)
	}
	if st["checkin"].(*CheckinStatus).OK {
		t.Fatal("failed checkin reported as success")
	}
	p.Disable("u1", "manual")
	if _, err := p.Checkin(context.Background(), "u1"); err == nil || calls.Load() != 3 {
		t.Fatal("disabled account was claimed")
	}
}

// 累计领取积分：逐活动记账（幂等），已确认的活动也能补记面额，重启后保留。
func TestCheckinAccumulatesClaimedCredits(t *testing.T) {
	results := []upstream.CheckinResult{
		// 第 1 轮：新领活动 4（4000）
		{OK: true, Credits: 4000, Campaigns: []upstream.CreditClaimResult{
			{CampaignID: 4, Amount: 4000, Credits: 4000, Status: "CONFIRMED", OK: true},
		}},
		// 第 2 轮：活动 4 只是"已确认"，活动 1 新领 100
		{OK: true, Credits: 100, Campaigns: []upstream.CreditClaimResult{
			{CampaignID: 4, Amount: 4000, Status: "CONFIRMED", OK: true},
			{CampaignID: 1, Amount: 100, Credits: 100, Status: "CONFIRMED", OK: true},
		}},
		// 第 3 轮：都只是"已确认" —— 累计必须稳定，不能重复累加
		{OK: true, Campaigns: []upstream.CreditClaimResult{
			{CampaignID: 4, Amount: 4000, Status: "CONFIRMED", OK: true},
			{CampaignID: 1, Amount: 100, Status: "CONFIRMED", OK: true},
		}},
	}
	var i int32
	client := checkinClientFunc(func(context.Context, upstream.SignCredential) (upstream.CheckinResult, error) {
		n := int(atomic.AddInt32(&i, 1))
		if n > len(results) {
			n = len(results)
		}
		return results[n-1], nil
	})
	stateFile := filepath.Join(t.TempDir(), "state.json")
	a := auth.New("u-credits", "user", "domain", "token", "ak", "sk",
		time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := New([]*auth.Auth{a}, Config{CheckinClient: client}, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	for range results {
		if _, err := p.Checkin(context.Background(), "u-credits"); err != nil {
			t.Fatal(err)
		}
	}
	st := p.List()[0]
	if got := st["claimed_credits"]; got != float64(4100) {
		t.Fatalf("claimed_credits=%v，want 4100（4000+100，幂等）", got)
	}
	if cs, _ := st["checkin"].(*CheckinStatus); cs == nil || cs.ClaimedCredits != 4100 || cs.LastCredits != 0 {
		t.Fatalf("checkin status=%+v", cs)
	}

	// 重启（重新 New）后应从 state.json 恢复累计值。
	p2, err := New([]*auth.Auth{a}, Config{CheckinClient: client}, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := p2.List()[0]["claimed_credits"]; got != float64(4100) {
		t.Fatalf("重启后 claimed_credits=%v，want 4100", got)
	}
}

