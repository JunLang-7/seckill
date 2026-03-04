package repository

import "gorm.io/gorm"

type ProductRepo interface {
}
type productRepo struct {
	db *gorm.DB
}

func NewProductRepo(db *gorm.DB) ProductRepo {
	return &productRepo{db: db}
}
