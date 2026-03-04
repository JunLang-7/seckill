package worker

import (
	"context"
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

}
