package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"seckill/config"
	"seckill/handler"
	"seckill/model"
	"seckill/pkg"
	"seckill/queue"
	"seckill/repo"
	"seckill/router"
	"seckill/service"
	"seckill/worker"
	"sync"
	"syscall"
	"time"
)

func main() {
	// 1. Load config
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("failed to load config:", err)
	}

	// 2. Connect to MySQL
	db, err := pkg.NewDB(cfg.MySQL)
	if err != nil {
		log.Fatal("failed to connect to mysql:", err)
	}
	if err := db.AutoMigrate(&model.Product{}, &model.SecKillRecord{}); err != nil {
		log.Fatal("failed to auto migrate:", err)
	}
	log.Println("mysql connection success")

	// 3. Connect to Redis
	rdb, err := pkg.NewClient(cfg.Redis)
	if err != nil {
		log.Fatal("failed to connect to redis:", err)
	}
	log.Println("redis connection success")

	// 4. Build order queue
	orderDeque := queue.NewOrderDeque(cfg.Queue.BufferSize)

	// 5. Wire repositories, servers & handlers
	productRepo := repo.NewProductRepo(db)
	seckillRepo := repo.NewSeckillRepo(db)

	seckillSvc := service.NewSeckillService(rdb, orderDeque, seckillRepo)

	productH := handler.NewProductHandler(productRepo, rdb)
	seckillH := handler.NewSeckillHandler(seckillSvc)

	// 6. Start worker pool
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	w := worker.NewWorker(orderDeque, seckillRepo)
	for i := 0; i < cfg.Queue.WorkerCount; i++ {
		wg.Add(1)
		go w.Start(&wg)
	}
	log.Println("start ", cfg.Queue.WorkerCount, " worker success")

	// 7. Start Http Server
	engine := router.Setup(productH, seckillH, cfg.Server)
	srv := &http.Server{
		Addr:    ":" + cfg.Server.Port,
		Handler: engine,
	}

	go func() {
		log.Println("server listening on :", cfg.Server.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	// 8. Graceful shutdown
	<-signalCtx.Done()
	log.Println("shutting down server...")

	// Step 1: Stop accepting new HTTP requests.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatal("server shutdown:", err)
	}
	log.Println("server shutdown success")

	// Step 2: Close the channel — signals workers that no new orders will arrive.
	close(orderDeque.Ch)

	// Step 3: Wait for every worker to finish processing whatever remains in the channel.
	wg.Wait()
	log.Println("all workers drained, process exiting")
}
