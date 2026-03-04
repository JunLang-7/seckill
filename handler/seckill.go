package handler

import (
	"seckill/service"
)

type SeckillHandler struct {
	seckillSvc service.SeckillService
}

func NewSeckillHandler(seckillSvc service.SeckillService) *SeckillHandler {
	return &SeckillHandler{seckillSvc: seckillSvc}
}
