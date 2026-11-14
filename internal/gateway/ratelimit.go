package gateway

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// userLimiter is a per-user token bucket. It is per replica, which is enough to stop a single
// user from flooding an agent; global quotas belong in the agent platform.
type userLimiter struct {
	limit rate.Limit
	burst int

	mu      sync.Mutex
	buckets map[string]*bucket
	sweep   time.Time
}

type bucket struct {
	l    *rate.Limiter
	seen time.Time
}

func newUserLimiter(perMinute float64, burst int) *userLimiter {
	return &userLimiter{limit: rate.Limit(perMinute / 60), burst: burst, buckets: map[string]*bucket{}}
}

func (u *userLimiter) Allow(user string) bool {
	now := time.Now()
	u.mu.Lock()
	defer u.mu.Unlock()
	if now.Sub(u.sweep) > 10*time.Minute {
		for k, b := range u.buckets {
			if now.Sub(b.seen) > 10*time.Minute {
				delete(u.buckets, k)
			}
		}
		u.sweep = now
	}
	b, ok := u.buckets[user]
	if !ok {
		b = &bucket{l: rate.NewLimiter(u.limit, u.burst)}
		u.buckets[user] = b
	}
	b.seen = now
	return b.l.AllowN(now, 1)
}
