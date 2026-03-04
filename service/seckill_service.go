package service

import (
	"seckill/queue"
	"seckill/repo"

	"github.com/redis/go-redis/v9"
)

type SeckillService interface {
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
