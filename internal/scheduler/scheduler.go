// Package scheduler 提供 token 自动续期和活动积分自动领取。
//
// 增强功能：
// - 主动保活：定期发送轻量级请求保持会话活跃
// - 更积极的刷新策略：token 剩余少于 1 小时即主动刷新
package scheduler

import (
	"context"
	"log"
	"time"

	"codearts2api/internal/pool"
)

// Config 调度器配置。
type Config struct {
	Pool              *pool.Pool
	Enabled           bool
	CreditsAutoClaim  bool          // 自动领取每日签到/新手积分，与 Token 模型福利独立
	PollInterval      time.Duration // 默认 30m
	RefreshSkew       time.Duration // 到期前多久刷新，默认 30m
	KeepaliveInterval time.Duration // 保活心跳间隔，默认 15m
}

// Scheduler 定时任务。
type Scheduler struct {
	cfg Config
}

// New 构造调度器。
func New(cfg Config) *Scheduler {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 30 * time.Minute
	}
	if cfg.KeepaliveInterval <= 0 {
		cfg.KeepaliveInterval = 15 * time.Minute // 15 分钟保活一次
	}
	return &Scheduler{cfg: cfg}
}

// Run 启动定时循环。
func (s *Scheduler) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		log.Printf("token watchdog disabled")
		return
	}
	log.Printf("token watchdog enabled: poll=%s refresh_skew=%s keepalive=%s",
		s.cfg.PollInterval, s.cfg.RefreshSkew, s.cfg.KeepaliveInterval)
	// 结构性问题：轮询间隔一旦等于/大于刷新窗口，最后一次本该刷新的 tick 会正好
	// 落在 token 过期之后（remaining 卡在 skew 边界上还会被跳过），一次网络抖动
	// 就再也没有重试机会——实测 2026-11-04 因此永久掉了一个账号。
	if s.cfg.RefreshSkew <= s.cfg.PollInterval {
		log.Printf("WARNING: refresh_skew(%s) <= poll(%s)：建议 refresh_skew 至少是 poll 的 2~4 倍，否则 token 会过期后才刷新",
			s.cfg.RefreshSkew, s.cfg.PollInterval)
	}

	// 立即执行一次
	s.Tick(ctx)

	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick 校验启用账号（自动续期 + 保活 + 可选活动领取）。
func (s *Scheduler) Tick(ctx context.Context) {
	for _, acct := range s.cfg.Pool.Accounts() {
		if err := ctx.Err(); err != nil {
			return
		}
		if !s.cfg.Pool.Enabled(acct.Name) {
			continue
		}
		ok, verr := s.cfg.Pool.Validate(acct)
		if !ok {
			if verr != nil {
				// 可重试错误（网络/5xx）：账号没被禁用，下一轮继续。
				// 以前这里和「被禁用」打同一句日志，把真正原因吞掉了。
				log.Printf("token account=%s refresh deferred: %v", acct.Name, verr)
			} else {
				log.Printf("token account=%s invalid/disabled", acct.Name)
			}
			continue
		}
		before := acct.Auth.Remaining().Round(time.Minute)
		log.Printf("token account=%s ok remaining=%s", acct.Name, before)

		// ticket 登录不返回 refresh_token；这种账号跳过无意义的周期刷新。
		if acct.Auth.Refresh() != "" {
			if err := s.cfg.Pool.CheckAndRefreshTokenWithin(acct.Name, s.cfg.RefreshSkew); err != nil {
				log.Printf("proactive refresh failed account=%s remaining=%s err=%v", acct.Name, before, err)
			} else if after := acct.Auth.Remaining().Round(time.Minute); after > before {
				// 只有剩余时间真的变长了才算刷新成功。以前用「刷新前的 remaining
				// <= skew」判断，remaining 刚好卡在 skew 边界上被跳过时也会打印
				// "token refreshed"，把「没刷」伪装成「刷成功」（实测 2026-11-04）。
				log.Printf("token refreshed account=%s remaining %s -> %s", acct.Name, before, after)
			}
		}
		if s.cfg.CreditsAutoClaim {
			claimCtx, cancel := context.WithTimeout(ctx, time.Minute)
			result, err := s.cfg.Pool.Checkin(claimCtx, acct.Name)
			cancel()
			if err != nil {
				log.Printf("credit checkin failed account=%s err=%v", acct.Name, err)
			} else {
				log.Printf("credit checkin account=%s ok=%t message=%s", acct.Name, result.OK, result.Message)
			}
		}
	}
}
