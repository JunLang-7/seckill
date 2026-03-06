package service_test

// Benchmarks for the hot path in seckill service.
//
// Run with:
//
//	go test ./service/ -bench=. -benchtime=5s -benchmem
//
// or with parallel goroutines matching a specific GOMAXPROCS:
//
//	GOMAXPROCS=8 go test ./service/ -bench=. -benchtime=10s
//
// These benchmarks use miniredis, so latency numbers reflect Go-side overhead
// (Lua script dispatch, key formatting, queue push) rather than real Redis RTT.
// They are most useful as relative comparisons across code changes.

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"seckill/queue/queuetest"
	"seckill/service"
)

// BenchmarkExecute_HappyPath measures end-to-end throughput of the happy path:
// unique user, sufficient stock, queue has capacity.  Uses atomic counter for
// unique per-goroutine user IDs so the dedup key never fires.
func BenchmarkExecute_HappyPath(b *testing.B) {
	mr := miniredis.RunT(b)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	// Pre-set stock high enough to never reach sold-out during benchmark.
	rdb.Set(ctx, "seckill:stock:1", 10_000_000, 0)

	// Queue buffer large enough to absorb every message without blocking.
	q := queuetest.NewFakeQueue(b.N + 1_000_000)
	svc := service.NewSeckillService(rdb, q, &mockRepo{})

	var uid int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			u := int(atomic.AddInt64(&uid, 1))
			_ = svc.Execute(ctx, u, 1)
		}
	})
}

// BenchmarkExecute_AlreadyPurchased measures the dedup fast-rejection path:
// the Lua script returns 1 immediately on SET NX miss (no stock operation).
// All goroutines share the same user ID, so every call hits the cached dedup key.
func BenchmarkExecute_AlreadyPurchased(b *testing.B) {
	mr := miniredis.RunT(b)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	rdb.Set(ctx, "seckill:stock:1", 10_000_000, 0)
	// Pre-set the dedup key so every request is rejected at the SET NX check.
	rdb.Set(ctx, "seckill:bought:1:99", "1", 0)

	q := queuetest.NewFakeQueue(10)
	svc := service.NewSeckillService(rdb, q, &mockRepo{})

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = svc.Execute(ctx, 99, 1)
		}
	})
}

// BenchmarkExecute_SoldOut measures the sold-out rejection path: the Lua script
// decrements stock below zero, rolls back internally (INCR + DEL), and returns 2.
// Each goroutine uses a unique user ID so the dedup check always passes.
func BenchmarkExecute_SoldOut(b *testing.B) {
	mr := miniredis.RunT(b)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	// Stock is 0: every DECR goes to -1 → immediate sold-out rollback.
	rdb.Set(ctx, "seckill:stock:1", 0, 0)

	q := queuetest.NewFakeQueue(10)
	svc := service.NewSeckillService(rdb, q, &mockRepo{})

	var uid int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			u := int(atomic.AddInt64(&uid, 1))
			_ = svc.Execute(ctx, u, 1)
		}
	})
}
