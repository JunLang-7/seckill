package model

type Product struct {
	ID    int    `gorm:"primaryKey"`
	Name  string `gorm:"unique;not null"`
	Stock int    `gorm:"type:int;check stock>=0"`
	Price int    `gorm:"type:int;check price>=0;comment:Price in cents"`
}
