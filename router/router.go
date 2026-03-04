package router

import (
	"context"
	"seckill/config"
	"seckill/handler"
	"time"

	"github.com/gin-gonic/gin"
)

func Setup(productH *handler.ProductHandler, seckillH *handler.SeckillHandler, cfg config.ServerConfig) *gin.Engine {
	r := gin.Default()
	r.Use(RequestTimeout(cfg.RequestTimeout))

	v1 := r.Group("/api/v1")
	{
		v1.GET("/products", productH.List)
		v1.GET("/products/:id", productH.GetByID)
		v1.POST("/seckill/:product_id", seckillH.Execute)
		v1.GET("/seckill/records/:user_id", seckillH.GetRecords)
	}

	admin := r.Group("/admin")
	{
		admin.POST("/seckill/:product_id", productH.InitSeckillStock)
	}

	return r
}

func RequestTimeout(d time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
