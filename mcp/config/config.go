package config

import "os"

type Config struct {
	Port          string
	JavaURL       string
	InternalToken string
	RedisURL      string
	NatsURL       string
}

func Load() *Config {
	return &Config{
		Port:          getenv("PORT", "4000"),
		JavaURL:       getenv("JAVA_URL", "http://localhost:3001"),
		InternalToken: getenv("INTERNAL_TOKEN", "change-me-internal-token"),
		RedisURL:      getenv("REDIS_URL", "redis://localhost:6380"),
		NatsURL:       getenv("NATS_URL", ""),
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
