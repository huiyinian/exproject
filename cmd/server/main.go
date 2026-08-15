package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/huiyinian/exproject/internal/app"
)

func main() {
	cfg, err := app.LoadConfig()
	if err != nil { log.Fatal(err) }
	a, err := app.New(context.Background(), cfg)
	if err != nil { log.Fatal(err) }
	defer a.Close()

	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: a.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("weekly contest listening on %s", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed { log.Fatal(err) }
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
