package main

import (
	"log"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type Event struct {
	ID        uint      `gorm:"primaryKey"`
	SessionID string    `gorm:"index"`
	Timestamp time.Time `gorm:"index"`
	Type      string    `gorm:"index"`
	Role      string
	Cwd       string
	Raw       string
}

func main() {
	db, err := gorm.Open(sqlite.Open("aichronicles.db"), &gorm.Config{})
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&Event{}); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Println("aichronicles: db ready")
}
