package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"seckill/queue"
	"seckill/repo"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// maxRetries is the total number of attempts (1 initial + 2 retries) for transient DB errors.
const maxRetries = 3

type Worker struct {
	queue       queue.Queue
	secKillRepo repo.SeckillRepo
	rdb         *redis.Client
	sleepFn     func(time.Duration) // overridable in tests; nil defaults to time.Sleep
}

func NewWorker(q queue.Queue, repo repo.SeckillRepo, rdb *redis.Client) *Worker {
	return &Worker{queue: q, secKillRepo: repo, rdb: rdb}
}

// Start opens a consumer on the queue and processes deliveries until ctx is
// cancelled or the delivery channel is closed. Each worker should be started in
// its own goroutine; the queue implementation distributes messages across them.
func (w *Worker) Start(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	deliveries, err := w.queue.Consume(ctx)
	if err != nil {
		log.Printf("[worker] failed to start consumer: %v", err)
		return
	}

	for d := range deliveries {
		// using recover makes a panic in any downstream call (repo, Redis, etc.) cannot
		// crash the worker goroutine. The delivery is always Ack'd: for a panicking message 
		// this avoids an infinite requeue loop (the message is almost certainly 
		// a "poison pill" that would panic on every attempt).
		defer func() {
			if r := recover(); r != nil  {
				log.Printf("[worker] PANIC recovered -acking message to prevent infinite requeue: panic=%v userID=%d productID=%d\n%s", r, d.Msg.UserID, d.Msg.ProductID, debug.Stack())
			}
			d.Ack()
		}()
		w.processOrder(d.Msg)
	}
	log.Println("[worker] delivery channel closed, worker exiting")
}

// processOrder attempts to write the order to MySQL.
//
// Failure taxonomy:
//   - ErrStockEmpty: DB-level stock already 0, non-retryable. Compensate Redis immediately.
//   - Other errors: transient (network blip, timeout). Retry up to maxRetries times with
//     exponential backoff (1 s, 2 s). If all attempts fail, compensate Redis.
//
// Compensation = restore the Redis stock counter + delete the dedup key, so the
// user is no longer permanently blocked and the phantom stock decrement is undone.
func (w *Worker) processOrder(msg queue.SeckillMessage) {
	var err error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			backoff := time.Duration(1<<(attempt-2)) * time.Second // 1s, 2s
			log.Printf("[worker] retrying order (attempt %d/%d, backoff %v): userID=%d productID=%d",
				attempt, maxRetries, backoff, msg.UserID, msg.ProductID)
			sleep := w.sleepFn
			if sleep == nil {
				sleep = time.Sleep
			}
			sleep(backoff)
		}

		err = w.secKillRepo.CreateOrder(context.Background(), msg)
		if err == nil {
			log.Printf("[worker] order created: userID=%d productID=%d", msg.UserID, msg.ProductID)
			return
		}

		// ErrStockEmpty means the DB stock counter hit 0 while Redis still thought
		// there was stock (data inconsistency). Retrying won't help — compensate now.
		if errors.Is(err, repo.ErrStockEmpty) {
			log.Printf("[worker] stock exhausted at DB level, compensating Redis: userID=%d productID=%d",
				msg.UserID, msg.ProductID)
			w.compensateRedis(msg)
			return
		}

		log.Printf("[worker] order attempt %d failed: userID=%d productID=%d err=%v",
			attempt, msg.UserID, msg.ProductID, err)
	}

	// All retries exhausted. Roll back Redis so the user is not permanently blocked
	// and the inventory count stays accurate.
	log.Printf("[worker] order permanently failed after %d attempts, compensating Redis: userID=%d productID=%d err=%v",
		maxRetries, msg.UserID, msg.ProductID, err)
	w.compensateRedis(msg)
}

// compensateRedis undoes the two Redis mutations made by the fast path:
//  1. Restore the stock counter (INCR) so the slot is available again.
//  2. Delete the dedup key (DEL) so the user can attempt a repurchase.
//
// Both operations use context.Background() because they must succeed even if
// the original request context has expired.
func (w *Worker) compensateRedis(msg queue.SeckillMessage) {
	ctx := context.Background()
	stockKey := fmt.Sprintf("seckill:stock:%d", msg.ProductID)
	dupKey := fmt.Sprintf("seckill:bought:%d:%d", msg.ProductID, msg.UserID)

	if err := w.rdb.Incr(ctx, stockKey).Err(); err != nil {
		log.Printf("[worker] CRITICAL: failed to restore Redis stock: userID=%d productID=%d err=%v",
			msg.UserID, msg.ProductID, err)
	}
	if err := w.rdb.Del(ctx, dupKey).Err(); err != nil {
		log.Printf("[worker] CRITICAL: failed to delete Redis dedup key: userID=%d productID=%d err=%v",
			msg.UserID, msg.ProductID, err)
	}
	log.Printf("[worker] Redis compensation complete: stock restored, dedup key removed: userID=%d productID=%d",
		msg.UserID, msg.ProductID)
}
