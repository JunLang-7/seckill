package model

import "time"

const (
	StatusPending = 0
	StatusSuccess = 1
	StatusFailed  = 2
)

type SecKillRecord struct {
	ID        uint      `gorm:"primaryKey;autoIncrement"`
	UserID    int       `gorm:"not null;index:id_user_product,unique,composite:up"`
	ProductID int       `gorm:"not null;index:id_user_product,unique,composite:up"`
	Status    int       `gorm:"type:tinyint(1);not null;default:0;comment:0=pending,1=success,2=failed"`
	CreatedAt time.Time `gorm:"index"`
	UpdatedAt time.Time
}
