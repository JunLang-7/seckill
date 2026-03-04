package repository

import "gorm.io/gorm"

type SeckillRepo interface {
}

type seckillRepo struct {
	db *gorm.DB
}

func NewSeckillRepo(db *gorm.DB) SeckillRepo {
	return &seckillRepo{db: db}
}
