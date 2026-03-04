package service

import (
	"context"
	"errors"
	"fmt"
	"seckill/queue"
	"seckill/repo"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	ErrAlreadyPurchased = errors.New("user has already purchased this product")
	ErrSoldOut          = errors.New("product is sold out")
	ErrRedisUnavailable = errors.New("redis service unavailable")
	ErrQueueFull        = errors.New("server is busy, please try again later")
)

type SeckillService interface {
	Execute(ctx context.Context, userID, productID int) error
	GetRecords(ctx context.Context, userID int) (interface{}, error)
}

type seckillService struct {
	rdb         *redis.Client
	queue       *queue.OrderDeque
	seckillRepo repo.SeckillRepo
}

func NewSeckillService(rdb *redis.Client, q *queue.OrderDeque, repo repo.SeckillRepo) SeckillService {
	return &seckillService{
		rdb:         rdb,
		queue:       q,
		seckillRepo: repo,
	}
}

// Execute is the fast-path handler for a seckill request.
// It performs Redis-level pre-interception and enqueues accepted orders for async DB writes.
//
// Rollback discipline: every Redis mutation that succeeds must be undone if a subsequent step fails.
// Rollback calls use context.Background() because the request context may already be expired.
func (s *seckillService) Execute(ctx context.Context, userID, productID int) error {
	// Step 1: Idempotency guard — prevent one user from buying the same product twice.
	dupKey := fmt.Sprintf("seckill:bought:%d:%d", productID, userID)
	result, err := s.rdb.SetNX(ctx, dupKey, "1", 24*time.Hour).Result()
	if err != nil {
		return ErrRedisUnavailable
	}
	if !result {
		return ErrAlreadyPurchased
	}

	// Step 2: Atomic stock decrement.
	stockKey := fmt.Sprintf("seckill:stock:%d", productID)
	remaining, err := s.rdb.Decr(ctx, stockKey).Result()
	if err != nil {
		// redis failed after set dup key
		s.rdb.Del(context.Background(), dupKey)
		return ErrRedisUnavailable
	}
	if remaining < 0 {
		// stock exhausted: restore counter and clean up dup key
		s.rdb.Incr(ctx, stockKey)
		s.rdb.Del(context.Background(), dupKey)
		return ErrSoldOut
	}

	// Step 3: Enqueue for async DB write (non-blocking).
	msg := queue.SeckillMessage{
		UserID:    userID,
		ProductID: productID,
	}
	if err := s.queue.Push(msg); err != nil {
		// queue saturated
		s.rdb.Incr(ctx, stockKey)
		s.rdb.Del(context.Background(), dupKey)
		return ErrQueueFull
	}

	return nil
}

func (s *seckillService) GetRecords(ctx context.Context, userID int) (interface{}, error) {
	return s.seckillRepo.GetRecordByUserID(ctx, userID)
}
