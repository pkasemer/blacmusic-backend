// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const devJWTSecret = "dev-only-insecure-secret-change-me!!"

// Config holds every setting the API needs.
type Config struct {
	Env         string // development | production
	HTTPAddr    string
	DatabaseURL string
	RedisURL    string

	S3Endpoint       string // host:port the API uses to reach storage (inside Docker: storage:9000)
	S3PublicEndpoint string // host:port clients use in presigned URLs (e.g. localhost:9000)
	S3Region         string
	S3AccessKey      string
	S3SecretKey      string
	S3Bucket         string
	S3UseSSL         bool
	S3PublicUseSSL   bool

	JWTSecret      string
	AccessTTL      time.Duration
	RefreshTTL     time.Duration
	PlaybackURLTTL time.Duration

	AuthRateLimit  int // requests per window per IP on auth endpoints
	AuthRateWindow time.Duration

	// Uploads and the transcoding worker.
	UploadMaxBytes     int64         // largest accepted original
	UploadURLTTL       time.Duration // how long a presigned upload URL stays valid
	MaxTrackDuration   time.Duration // longest accepted track or episode
	TranscodeTimeout   time.Duration // wall-clock limit for one ffmpeg run
	WorkerConcurrency  int           // jobs processed in parallel (ffmpeg is CPU heavy)
	WorkerPollInterval time.Duration
	FFmpegPath         string
	FFprobePath        string

	// Public identity and mail (API only).
	PublicAPIURL     string // base URL clients reach the API at; used in image links
	FreeStorageBytes int64  // free storage every artist starts with
	SMTPHost         string // empty = log emails instead of sending (development only)
	SMTPPort         string
	SMTPUser         string
	SMTPPassword     string
	SMTPFrom         string

	// Podcast feeds.
	FeedAllowPrivate    bool          // dev only: let feed fetches reach private addresses
	FeedRefreshInterval time.Duration // how often subscribed shows are re-fetched
	FeedFetchTimeout    time.Duration
}

// Load reads the environment and validates it for the API.
func Load() (Config, error) { return load(true) }

// LoadWorker is Load for the transcoding worker, which has no use for a JWT secret.
func LoadWorker() (Config, error) { return load(false) }

func load(needJWT bool) (Config, error) {
	var err error
	c := Config{
		Env:         get("APP_ENV", "development"),
		HTTPAddr:    get("HTTP_ADDR", ":8080"),
		DatabaseURL: get("DATABASE_URL", "postgres://blac:blac@localhost:5432/blac?sslmode=disable"),
		RedisURL:    get("REDIS_URL", "redis://localhost:6379/0"),
		S3Endpoint:  get("S3_ENDPOINT", "localhost:9000"),
		S3Region:    get("S3_REGION", "us-east-1"),
		S3AccessKey: get("S3_ACCESS_KEY", "blacminio"),
		S3SecretKey: get("S3_SECRET_KEY", "blacminio-secret"),
		S3Bucket:    get("S3_BUCKET", "blac-media"),
		JWTSecret:   os.Getenv("JWT_SECRET"),

		FFmpegPath:  get("FFMPEG_PATH", "ffmpeg"),
		FFprobePath: get("FFPROBE_PATH", "ffprobe"),

		PublicAPIURL: strings.TrimRight(get("PUBLIC_API_URL", "http://localhost:8080"), "/"),
		SMTPHost:     os.Getenv("SMTP_HOST"),
		SMTPPort:     get("SMTP_PORT", "587"),
		SMTPUser:     os.Getenv("SMTP_USER"),
		SMTPPassword: os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:     get("SMTP_FROM", "Blac Music <no-reply@localhost>"),
	}
	c.S3PublicEndpoint = get("S3_PUBLIC_ENDPOINT", c.S3Endpoint)

	if c.S3UseSSL, err = getBool("S3_USE_SSL", false); err != nil {
		return c, err
	}
	if c.S3PublicUseSSL, err = getBool("S3_PUBLIC_USE_SSL", c.S3UseSSL); err != nil {
		return c, err
	}
	if c.AccessTTL, err = getDuration("ACCESS_TOKEN_TTL", 15*time.Minute); err != nil {
		return c, err
	}
	if c.RefreshTTL, err = getDuration("REFRESH_TOKEN_TTL", 30*24*time.Hour); err != nil {
		return c, err
	}
	if c.PlaybackURLTTL, err = getDuration("PLAYBACK_URL_TTL", time.Hour); err != nil {
		return c, err
	}
	if c.AuthRateWindow, err = getDuration("AUTH_RATE_WINDOW", time.Minute); err != nil {
		return c, err
	}
	rate, err := getInt("AUTH_RATE_LIMIT", 20)
	if err != nil {
		return c, err
	}
	c.AuthRateLimit = rate

	freeGB, err := getInt("FREE_STORAGE_GB", 10)
	if err != nil {
		return c, err
	}
	if freeGB < 0 {
		return c, fmt.Errorf("FREE_STORAGE_GB must not be negative")
	}
	c.FreeStorageBytes = int64(freeGB) << 30

	maxMiB, err := getInt("UPLOAD_MAX_MB", 2048)
	if err != nil {
		return c, err
	}
	if maxMiB < 1 {
		return c, fmt.Errorf("UPLOAD_MAX_MB must be at least 1")
	}
	c.UploadMaxBytes = int64(maxMiB) << 20
	if c.UploadURLTTL, err = getDuration("UPLOAD_URL_TTL", time.Hour); err != nil {
		return c, err
	}
	if c.MaxTrackDuration, err = getDuration("MAX_TRACK_DURATION", 4*time.Hour); err != nil {
		return c, err
	}
	if c.TranscodeTimeout, err = getDuration("TRANSCODE_TIMEOUT", 30*time.Minute); err != nil {
		return c, err
	}
	if c.WorkerPollInterval, err = getDuration("WORKER_POLL_INTERVAL", 2*time.Second); err != nil {
		return c, err
	}
	if c.WorkerConcurrency, err = getInt("WORKER_CONCURRENCY", 1); err != nil {
		return c, err
	}
	if c.WorkerConcurrency < 1 || c.WorkerConcurrency > 16 {
		return c, fmt.Errorf("WORKER_CONCURRENCY must be 1-16")
	}

	if c.FeedAllowPrivate, err = getBool("FEED_ALLOW_PRIVATE", false); err != nil {
		return c, err
	}
	if c.FeedAllowPrivate && c.Env == "production" {
		return c, fmt.Errorf("FEED_ALLOW_PRIVATE must not be enabled in production")
	}
	if c.FeedRefreshInterval, err = getDuration("FEED_REFRESH_INTERVAL", time.Hour); err != nil {
		return c, err
	}
	if c.FeedRefreshInterval < time.Minute {
		return c, fmt.Errorf("FEED_REFRESH_INTERVAL must be at least 1m")
	}
	if c.FeedFetchTimeout, err = getDuration("FEED_FETCH_TIMEOUT", 15*time.Second); err != nil {
		return c, err
	}

	if !needJWT {
		return c, nil
	}
	if c.JWTSecret == "" && c.Env == "development" {
		c.JWTSecret = devJWTSecret
	}
	if c.Env == "production" && c.SMTPHost == "" {
		return c, fmt.Errorf("SMTP_HOST must be set in production (verification emails cannot be sent otherwise)")
	}
	if len(c.JWTSecret) < 32 {
		return c, fmt.Errorf("JWT_SECRET must be set and at least 32 characters")
	}
	if c.Env == "production" && c.JWTSecret == devJWTSecret {
		return c, fmt.Errorf("JWT_SECRET must not be the development default in production")
	}
	return c, nil
}

func get(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

func getInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func getDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
