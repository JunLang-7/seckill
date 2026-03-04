package router

import (
	"seckill/config"
	"seckill/handler"

	"github.com/gin-gonic/gin"
)

func Setup(productH *handler.ProductHandler, seckillH *handler.SeckillHandler, cfg *config.Config) *gin.Engine {
	r := gin.New()

	return r
}
