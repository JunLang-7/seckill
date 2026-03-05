package router

import (
	"seckill/config"
	"seckill/handler"
	"seckill/middleware"

	"github.com/gin-gonic/gin"
)

var (
	ratePerSec    float64 = 5000
	burstCapacity         = 10000
)

func Setup(productH *handler.ProductHandler, seckillH *handler.SeckillHandler, cfg config.ServerConfig) *gin.Engine {
	r := gin.Default()
	r.Use(middleware.RequestTimeout(cfg.RequestTimeout))

	v1 := r.Group("/api/v1")
	{
		v1.GET("/products", productH.List)
		v1.GET("/products/:id", productH.GetByID)
		seckillGroup := v1.Group("/seckill")
		seckillGroup.Use(middleware.GlobalRateLimiter(ratePerSec, burstCapacity))
		{
			seckillGroup.POST("/:product_id", seckillH.Execute)
			seckillGroup.GET("/records/:user_id", seckillH.GetRecords)
		}
	}

	admin := r.Group("/admin")
	{
		admin.POST("/seckill/:product_id", productH.InitSeckillStock)
	}

	return r
}
