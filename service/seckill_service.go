package service

import (
	"context"
	"errors"
	"fmt"
	"seckill/queue"
	"seckill/repo"

	"github.com/redis/go-redis/v9"
)

var (
	ErrAlreadyPurchased = errors.New("user has already purchased this product")
	ErrSoldOut          = errors.New("product is sold out")
	ErrRedisUnavailable = errors.New("redis service unavailable")
	ErrQueueFull        = errors.New("server is busy, please try again later")
)

// dupKeyTTLSec is the TTL (in seconds) for the per-user dedup key in Redis.
const dupKeyTTLSec = 86400 // 24 h

// seckillScript atomically handles the two Redis mutations that guard against
// overselling and duplicate purchases.  Because Redis executes Lua scripts as a
// single unit, no other client can observe an intermediate state and no Go-side
// rollback logic is needed for these two steps.
//
// KEYS[1] = dupKey   (seckill:bought:{productID}:{userID})
// KEYS[2] = stockKey (seckill:stock:{productID})
// ARGV[1] = TTL in seconds for the dedup key
//
// Return codes:
//
//	0 = success (dedup key written, stock decremented)
//	1 = already purchased (SET NX failed — key already existed)
//	2 = sold out (DECR drove stock below zero; both mutations rolled back inside script)
//	3 = Redis DECR error (e.g. stock key holds a non-integer; dupKey rolled back inside script)
var seckillScript = redis.NewScript(`
local set = redis.call('SET', KEYS[1], '1', 'NX', 'EX', ARGV[1])
if not set then
    return 1
end

local ok, remaining = pcall(redis.call, 'DECR', KEYS[2])
if not ok then
    redis.call('DEL', KEYS[1])
    return 3
end

if remaining < 0 then
    redis.call('INCR', KEYS[2])
    redis.call('DEL', KEYS[1])
    return 2
end

return 0
`)

type SeckillService interface {
	Execute(ctx context.Context, userID, productID int) error
	GetRecords(ctx context.Context, userID int) (interface{}, error)
}

type seckillService struct {
	rdb         *redis.Client
	queue       queue.Queue
	seckillRepo repo.SeckillRepo
}

func NewSeckillService(rdb *redis.Client, q queue.Queue, repo repo.SeckillRepo) SeckillService {
	return &seckillService{
		rdb:         rdb,
		queue:       q,
		seckillRepo: repo,
	}
}

// Execute is the fast-path handler for a seckill request.
//
// The idempotency check and stock decrement are fused into a single atomic Lua
// script (one EVALSHA roundtrip).  The only remaining failure path that requires
// manual Redis rollback is queue saturation (Step 2 below).
func (s *seckillService) Execute(ctx context.Context, userID, productID int) error {
	dupKey := fmt.Sprintf("seckill:bought:%d:%d", productID, userID)
	stockKey := fmt.Sprintf("seckill:stock:%d", productID)

	// Step 1: Atomic idempotency guard + stock decrement via Lua script.
	res, err := seckillScript.Run(ctx, s.rdb, []string{dupKey, stockKey}, dupKeyTTLSec).Int()
	if err != nil {
		return ErrRedisUnavailable
	}
	switch res {
	case 1:
		return ErrAlreadyPurchased
	case 2:
		return ErrSoldOut
	case 3:
		return ErrRedisUnavailable
	}

	// Step 2: Enqueue for async DB write (non-blocking).
	// On failure both script mutations must be manually undone because they
	// were committed to Redis before this point.
	msg := queue.SeckillMessage{UserID: userID, ProductID: productID}
	if err := s.queue.Push(msg); err != nil {
		s.rdb.Incr(context.Background(), stockKey)
		s.rdb.Del(context.Background(), dupKey)
		return ErrQueueFull
	}

	return nil
}

func (s *seckillService) GetRecords(ctx context.Context, userID int) (interface{}, error) {
	return s.seckillRepo.GetRecordByUserID(ctx, userID)
}
