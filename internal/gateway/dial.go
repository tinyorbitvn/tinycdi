package gateway

import (
	"context"
	"net"
	"time"
)

// UpstreamDialBudget is the default total time the gateway keeps retrying a
// refused or unreachable upstream connect before answering 502 (F2).
const UpstreamDialBudget = 15 * time.Second

// dialWithRetry returns a DialContext that retries failed connects with
// exponential backoff (200 ms doubling to 2 s) until budget elapses or ctx
// ends. Each attempt is bounded by 3 s. A cold workspace's Service route can
// trail its pod's readiness by a moment; a refused first dial must not
// become the user's first impression.
func dialWithRetry(budget time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		deadline := time.Now().Add(budget)
		backoff := 200 * time.Millisecond
		for {
			conn, err := d.DialContext(ctx, network, addr)
			if err == nil {
				return conn, nil
			}
			if ctx.Err() != nil || time.Now().Add(backoff).After(deadline) {
				return nil, err
			}
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, err
			case <-timer.C:
			}
			if backoff *= 2; backoff > 2*time.Second {
				backoff = 2 * time.Second
			}
		}
	}
}
