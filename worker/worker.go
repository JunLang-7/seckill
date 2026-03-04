package worker

import (
	"context"
	"errors"
	"log"
	"seckill/queue"
	"seckill/repo"
)

type Worker struct {
	queue       *queue.OrderDeque
	secKillRepo repo.SeckillRepo
}

func NewWorker(q *queue.OrderDeque, repo repo.SeckillRepo) *Worker {
	return &Worker{queue: q, secKillRepo: repo}
}

func (w *Worker) Start(ctx context.Context) {
	for {
		select {
		case msg := <-w.queue.Ch:
			w.processOrder(msg)
		case <-ctx.Done():
			return
		}
	}
}

func (w *Worker) processOrder(msg queue.SeckillMessage) {
	err := w.secKillRepo.CreateOrder(context.Background(), msg)
	if err != nil {
		if errors.Is(err, repo.ErrStockEmpty) {
			log.Printf("[worker] stock exhausted at DB level: userID=%d productID=%d", msg.UserID, msg.ProductID)
		} else {
			log.Printf("[worker] failed to create order: userID=%d productID=%d, err=%v", msg.UserID, msg.ProductID, err)
		}
		return
	}
	log.Printf("[worker] order created: userID=%d productID=%d", msg.UserID, msg.ProductID)
}
