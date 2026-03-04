package handler

import (
	"seckill/repo"

	"github.com/redis/go-redis/v9"
)

type ProductHandler struct {
	productRepo repo.ProductRepo
	rdb         *redis.Client
}

func NewProductHandler(productRepo repo.ProductRepo, rdb *redis.Client) *ProductHandler {
	return &ProductHandler{productRepo: productRepo, rdb: rdb}
}
