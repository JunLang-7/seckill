package repo

import (
	"context"
	"errors"
	"seckill/model"
	"seckill/queue"
	"time"

	"gorm.io/gorm"
)

var ErrStockEmpty = errors.New("product stock is empty")

type SeckillRepo interface {
	CreateOrder(ctx context.Context, msg queue.SeckillMessage) error
	GetRecordByUserID(ctx context.Context, userID int) ([]model.SecKillRecord, error)
}

type seckillRepo struct {
	db *gorm.DB
}

func NewSeckillRepo(db *gorm.DB) SeckillRepo {
	return &seckillRepo{db: db}
}

// CreateOrder runs an atomic transaction: deduct stock AND insert seckill record.
// The WHERE stock > 0 predicate is the DB-level last line of defense against overselling.
func (r *seckillRepo) CreateOrder(ctx context.Context, msg queue.SeckillMessage) error {
	dbCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	return r.db.WithContext(dbCtx).Transaction(func(tx *gorm.DB) error {
		// deduct stock
		res := tx.Exec(
			"UPDATE products SET stock = stock - 1 WHERE id = ? AND stock > 0",
			msg.ProductID,
		)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrStockEmpty
		}

		// insert a seckill record
		record := model.SecKillRecord{
			UserID:    msg.UserID,
			ProductID: msg.ProductID,
			Status:    model.StatusSuccess,
		}
		return tx.Create(&record).Error
	})
}

func (r *seckillRepo) GetRecordByUserID(ctx context.Context, userID int) ([]model.SecKillRecord, error) {
	var records []model.SecKillRecord
	if err := r.db.WithContext(ctx).Where("user_id = ?", userID).Find(&records).Error; err != nil {
		return nil, err
	}
	return records, nil
}
