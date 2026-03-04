package worker

import (
	"context"
	"errors"
	"log"
	"seckill/queue"
	"seckill/repo"
	"sync"
)

type Worker struct {
	queue       *queue.OrderDeque
	secKillRepo repo.SeckillRepo
}

func NewWorker(q *queue.OrderDeque, repo repo.SeckillRepo) *Worker {
	return &Worker{queue: q, secKillRepo: repo}
}

// Start drains the order channel until it is closed and empty, then signals the
// WaitGroup that this worker has exited. Shutdown is driven by main closing the
// channel — NOT by cancelling a context — so no in-flight orders are dropped.
func (w *Worker) Start(wg *sync.WaitGroup) {
	defer wg.Done()
	for msg := range w.queue.Ch {
		w.processOrder(msg)
	}
	log.Println("[worker] channel closed, worker exiting")
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
