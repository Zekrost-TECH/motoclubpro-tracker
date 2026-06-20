package redis

import (
    "context"
    "log"
    "os"
    "time"

    "github.com/redis/go-redis/v9"
)

var (
    Client *redis.Client
    Ctx    = context.Background()
)

func InitRedis() {
    url := os.Getenv("REDIS_URL")
    if url == "" {
        url = "redis://localhost:6379"
    }

    opt, err := redis.ParseURL(url)
    if err != nil {
        log.Fatalf("Error parsing REDIS_URL: %v", err)
    }

    Client = redis.NewClient(opt)

    const maxAttempts = 10
    for i := range maxAttempts {
        if err := Client.Ping(Ctx).Err(); err != nil {
            wait := time.Duration(i+1) * 2 * time.Second
            log.Printf("Redis not ready (attempt %d/%d): %v — retrying in %s",
                i+1, maxAttempts, err, wait)
            time.Sleep(wait)
            continue
        }
        log.Println("Connected to Redis successfully")
        return
    }

    log.Fatalf("Failed to connect to Redis after %d attempts", maxAttempts)
}