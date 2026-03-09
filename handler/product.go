package handler

import (
	"fmt"
	"log"
	"net/http"
	"seckill/repo"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

type ProductHandler struct {
	productRepo repo.ProductRepo
	rdb         *redis.Client
}

func NewProductHandler(productRepo repo.ProductRepo, rdb *redis.Client) *ProductHandler {
	return &ProductHandler{productRepo: productRepo, rdb: rdb}
}

func (h *ProductHandler) List(c *gin.Context) {
	products, err := h.productRepo.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": products})
}

func (h *ProductHandler) GetByID(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid product id"})
		return
	}

	product, err := h.productRepo.GetByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "product not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": product})
}

// InitSeckillStock seeds seckill:stock:{id} in Redis from the current DB stock.
// Call this endpoint before each flash sale event begins.
func (h *ProductHandler) InitSeckillStock(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("product_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid product id"})
		return
	}

	if err := h.productRepo.InitRedisStock(c.Request.Context(), h.rdb, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	productIDStr := fmt.Sprintf("%d", id)
	if err := h.rdb.Publish(c.Request.Context(), "channel:stock_restore", productIDStr).Err(); err != nil {
		log.Printf("Failed to publish stock restore product id %s: %v", productIDStr, err)
	}
	c.JSON(http.StatusOK, gin.H{"message": "redis stock init success"})
}
