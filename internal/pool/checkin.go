package pool

import (
	"context"
	"fmt"
	"log"
	"time"

	"codearts2api/internal/upstream"
)

// CheckinClient 允许注入独立的活动客户端；默认使用各账号的上游客户端。
type CheckinClient interface {
	Checkin(context.Context, upstream.SignCredential) (upstream.CheckinResult, error)
}

type CheckinStatus struct {
	OK        bool      `json:"ok"`
	Message   string    `json:"message"`
	CheckedAt time.Time `json:"checked_at"`
	// ClaimedCredits 该账号**累计领取到**的积分（历次签到/新手活动累加）。
	// 注意这不是余额：上游余额接口 tokens/balance 自 2026-10-22 起被关闭
	// （error_code=4000），拿不到剩余积分。
	ClaimedCredits float64 `json:"claimed_credits"`
	// LastCredits 本次领取量。
	LastCredits float64 `json:"last_credits"`
}

// Checkin 串行领取指定账号的每日签到和新手福利。活动故障只记录签到结果，
// 不计入推理错误、冷却或禁用；token 仍由原有 Validate/RefreshToken 处理。
func (p *Pool) Checkin(ctx context.Context, name string) (upstream.CheckinResult, error) {
	a := p.Get(name)
	if a == nil {
		return upstream.CheckinResult{}, fmt.Errorf("account not found")
	}
	a.checkinMu.Lock()
	defer a.checkinMu.Unlock()
	if err := ctx.Err(); err != nil {
		return upstream.CheckinResult{}, err
	}
	a.mu.Lock()
	disabled := a.disabled
	a.mu.Unlock()
	if disabled {
		return upstream.CheckinResult{}, fmt.Errorf("account disabled")
	}
	ok, err := p.Validate(a)
	if err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("token invalid; re-login required")
		}
		return upstream.CheckinResult{}, err
	}
	token, ak, sk := a.Auth.Credentials()
	client := p.cfg.CheckinClient
	if client == nil {
		client = a.Client
	}
	result, err := client.Checkin(ctx, upstream.SignCredential{SecurityToken: token, AccessKeyID: ak, SecretAccessKey: sk})
	if err != nil {
		result.OK, result.Message = false, err.Error()
	}
	a.mu.Lock()
	// 逐活动记账，天然幂等：同一活动反复巡检不会重复累加。
	// CONFIRMED/CONSUMED 表示「之前已经领到」（面额仍能拿到），Credits>0 表示
	// 「本次刚领到」——两种都要计入累计，否则老账号永远是 0。
	if a.claimedCampaigns == nil {
		a.claimedCampaigns = map[int]float64{}
	}
	changed := false
	for _, c := range result.Campaigns {
		amount := c.Amount
		if amount <= 0 {
			amount = c.Credits
		}
		if amount <= 0 {
			continue
		}
		if c.Status == "CONFIRMED" || c.Status == "CONSUMED" || c.Credits > 0 {
			if old, ok := a.claimedCampaigns[c.CampaignID]; !ok || old != amount {
				a.claimedCampaigns[c.CampaignID] = amount
				changed = true
			}
		}
	}
	if changed {
		var total float64
		for _, v := range a.claimedCampaigns {
			total += v
		}
		a.claimedCredits = total
	}
	a.checkin = &CheckinStatus{
		OK:             result.OK,
		Message:        result.Message,
		CheckedAt:      time.Now(),
		ClaimedCredits: a.claimedCredits,
		LastCredits:    result.Credits,
	}
	claimed := a.claimedCredits
	a.mu.Unlock()
	// 累计值要落盘，否则重启就丢（state.json 里是 claimed_credits/claimed_campaigns）。
	if changed {
		p.saveState()
		log.Printf("pool credits account=%s this=%g total=%g", name, result.Credits, claimed)
	}
	return result, err
}
