package main

import (
	"context"
	"errors"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"example.com/isureal-company/isureal"
	"github.com/labstack/echo/v5"
)

// 2026-09-23

func main() {
	config := isureal.ConfigFromEnv()
	db := isureal.OpenDatabase(config)
	defer db.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	listeners := map[string]string{":8080": config.Region}
	if config.Region == "asia" {
		// Each ingress region has its own listener: client headers cannot select
		// a different region. Traffic from remote nginx still follows the router.
		listeners["192.168.1.10:8081"] = "europe"
		listeners["192.168.1.10:8082"] = "america"
		// This process serves every region, so it alone prepares image signatures.
		isureal.StartSignaturePrewarm(ctx, db)
	}
	var wg sync.WaitGroup
	errorsCh := make(chan error, len(listeners))
	for address, region := range listeners {
		regionalConfig := *config
		regionalConfig.Region = region
		server := isureal.NewServerWithDB(&regionalConfig, db)
		startConfig := echo.StartConfig{
			Address: address,
			BeforeServeFunc: func(s *http.Server) error {
				s.ReadHeaderTimeout = 10 * time.Second
				s.IdleTimeout = 120 * time.Second
				return nil
			},
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := startConfig.Start(ctx, server); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errorsCh <- err
				cancel()
			}
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		panic("failed to serve regional API: " + err.Error())
	}
}
