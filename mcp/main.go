package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"asynkor/mcp/config"
	"asynkor/mcp/internal/activity"
	"asynkor/mcp/internal/auth"
	"asynkor/mcp/internal/lease"
	"asynkor/mcp/internal/mcpserver"
	"asynkor/mcp/internal/natsbus"
	"asynkor/mcp/internal/redisstore"
	"asynkor/mcp/internal/session"
	"asynkor/mcp/internal/teamctx"
	"asynkor/mcp/internal/work"
)

func main() {
	cfg := config.Load()

	if cfg.InternalToken == "" || cfg.InternalToken == "change-me-internal-token" {
		log.Println("WARNING: INTERNAL_TOKEN is not set or uses default value. Set a strong token for production!")
	}

	redisClient, err := redisstore.New(cfg.RedisURL)
	if err != nil {
		log.Fatalf("redis: %v", err)
	}

	nats := natsbus.New(cfg.NatsURL)
	defer nats.Close()

	validator := auth.NewValidator(cfg.JavaURL, cfg.InternalToken)
	sessionStore := session.NewStore(redisClient)
	workStore := work.NewStore(redisClient)
	leaseStore := lease.NewStore(redisClient)
	activityStore := activity.NewStore(redisClient)
	teamCtxStore := teamctx.NewStore(cfg.JavaURL, cfg.InternalToken)

	srv := mcpserver.New(cfg, validator, sessionStore, workStore, leaseStore, nats, activityStore, teamCtxStore)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.Start(); err != nil {
			log.Printf("server stopped: %v", err)
		}
	}()

	<-stop
	log.Println("shutting down")
}
