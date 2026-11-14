package store

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis is a Store shared by all gateway replicas.
type Redis struct {
	c      redis.UniversalClient
	prefix string
	ttl    time.Duration
}

type RedisOptions struct {
	Address    string
	Username   string
	Password   string
	DB         int
	TLS        bool
	KeyPrefix  string
	SessionTTL time.Duration
}

func NewRedis(o RedisOptions) *Redis {
	opts := &redis.Options{
		Addr:         o.Address,
		Username:     o.Username,
		Password:     o.Password,
		DB:           o.DB,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	}
	if o.TLS {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return NewRedisFromClient(redis.NewClient(opts), o.KeyPrefix, o.SessionTTL)
}

func NewRedisFromClient(c redis.UniversalClient, prefix string, ttl time.Duration) *Redis {
	return &Redis{c: c, prefix: prefix, ttl: ttl}
}

func (r *Redis) k(kind, key string) string { return r.prefix + kind + ":" + key }

func (r *Redis) GetSession(ctx context.Context, key string) (*Session, error) {
	b, err := r.c.Get(ctx, r.k("session", key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *Redis) PutSession(ctx context.Context, key string, s *Session) error {
	s.UpdatedAt = time.Now()
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return r.c.Set(ctx, r.k("session", key), b, r.ttl).Err()
}

func (r *Redis) DeleteSession(ctx context.Context, key string) error {
	return r.c.Del(ctx, r.k("session", key)).Err()
}

func (r *Redis) FirstDelivery(ctx context.Context, id string, ttl time.Duration) (bool, error) {
	return r.c.SetNX(ctx, r.k("seen", id), 1, ttl).Result()
}

var acquireScript = redis.NewScript(`
local cur = redis.call("GET", KEYS[1])
if cur == false or cur == ARGV[1] then
  redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[2])
  return 1
end
return 0`)

var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0`)

func (r *Redis) Acquire(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	n, err := acquireScript.Run(ctx, r.c, []string{r.k("lock", key)}, owner, ttl.Milliseconds()).Int()
	return n == 1, err
}

func (r *Redis) Release(ctx context.Context, key, owner string) error {
	return releaseScript.Run(ctx, r.c, []string{r.k("lock", key)}, owner).Err()
}

func (r *Redis) Ping(ctx context.Context) error { return r.c.Ping(ctx).Err() }

func (r *Redis) Close() error { return r.c.Close() }
