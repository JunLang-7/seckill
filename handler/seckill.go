package handler

import (
	"errors"
	"net/http"
	"seckill/service"
	"strconv"

	"github.com/gin-gonic/gin"
)

type SeckillHandler struct {
	seckillSvc service.SeckillService
}

func NewSeckillHandler(seckillSvc service.SeckillService) *SeckillHandler {
	return &SeckillHandler{seckillSvc: seckillSvc}
}

type seckillRequest struct {
	UserID int `json:"user_id" blinding:"required,gt=0"`
}

func (h *SeckillHandler) Execute(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("product_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid product id"})
		return
	}

	var req seckillRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id is required and should be positive"})
		return
	}

	err = h.seckillSvc.Execute(c.Request.Context(), id, req.UserID)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrAlreadyPurchased):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		case errors.Is(err, service.ErrSoldOut):
			c.JSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
		case errors.Is(err, service.ErrRedisUnavailable):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		case errors.Is(err, service.ErrQueueFull):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusAccepted, gin.H{"message": "order accepted", "status": "pending"})
}

func (h *SeckillHandler) GetRecords(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("user_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_id"})
		return
	}

	records, err := h.seckillSvc.GetRecords(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": records})
}
