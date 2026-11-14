package store

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func stores(t *testing.T) map[string]Store {
	mr := miniredis.RunT(t)
	return map[string]Store{
		"memory": NewMemory(time.Hour, 2),
		"redis":  NewRedisFromClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}), "t:", time.Hour),
	}
}

func TestStoreContract(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			defer s.Close()

			if got, err := s.GetSession(ctx, "k"); err != nil || got != nil {
				t.Fatalf("missing session: %v %v", got, err)
			}
			in := &Session{ContextID: "ctx-1", Pending: &Pending{Kind: PendingAskUser, Nonce: "n", Questions: []Question{{Text: "q?"}}}}
			if err := s.PutSession(ctx, "k", in); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetSession(ctx, "k")
			if err != nil || got.ContextID != "ctx-1" || got.Pending.Questions[0].Text != "q?" {
				t.Fatalf("round trip: %+v %v", got, err)
			}
			got.ContextID = "mutated"
			again, _ := s.GetSession(ctx, "k")
			if again.ContextID != "ctx-1" {
				t.Fatal("stored sessions must not alias returned values")
			}
			_ = s.DeleteSession(ctx, "k")
			if got, _ := s.GetSession(ctx, "k"); got != nil {
				t.Fatal("delete failed")
			}

			if first, _ := s.FirstDelivery(ctx, "a1", time.Minute); !first {
				t.Fatal("first delivery should be new")
			}
			if first, _ := s.FirstDelivery(ctx, "a1", time.Minute); first {
				t.Fatal("redelivery must be detected")
			}

			if ok, _ := s.Acquire(ctx, "l", "owner-1", time.Minute); !ok {
				t.Fatal("free lock should be acquired")
			}
			if ok, _ := s.Acquire(ctx, "l", "owner-2", time.Minute); ok {
				t.Fatal("held lock must not be acquired by another owner")
			}
			_ = s.Release(ctx, "l", "owner-2") // not the holder: no effect
			if ok, _ := s.Acquire(ctx, "l", "owner-2", time.Minute); ok {
				t.Fatal("release by a non-holder must not free the lock")
			}
			_ = s.Release(ctx, "l", "owner-1")
			if ok, _ := s.Acquire(ctx, "l", "owner-2", time.Minute); !ok {
				t.Fatal("released lock should be acquirable")
			}
		})
	}
}

func TestMemoryBoundsSessions(t *testing.T) {
	m := NewMemory(time.Hour, 2)
	defer m.Close()
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c"} {
		_ = m.PutSession(ctx, k, &Session{ContextID: k})
		time.Sleep(time.Millisecond)
	}
	if got, _ := m.GetSession(ctx, "a"); got != nil {
		t.Error("oldest session should have been evicted")
	}
	if got, _ := m.GetSession(ctx, "c"); got == nil {
		t.Error("newest session must be kept")
	}
}
