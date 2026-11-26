// Package config loads runtime configuration from environment variables.
package config

import (
	"encoding/hex"
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
	UploadPartBytes    int64         // size of each part of a resumable upload
	UploadURLTTL       time.Duration // how long a presigned upload URL stays valid
	MaxTrackDuration   time.Duration // longest accepted track or episode
	TranscodeTimeout   time.Duration // wall-clock limit for one ffmpeg run
	WorkerConcurrency  int           // jobs processed in parallel (ffmpeg is CPU heavy)
	WorkerPollInterval time.Duration
	FFmpegPath         string
	FFprobePath        string

	// S3PublicHostFromRequest (development only) presigns storage links for the
	// host the client used to reach the API, so a phone on the LAN gets links
	// it can open. Refused in production.
	S3PublicHostFromRequest bool

	// TrustProxy makes the API believe X-Real-IP / X-Forwarded-For. Enable it only
	// when every request comes through a reverse proxy that overwrites those
	// headers (the API port must not be reachable any other way), otherwise
	// clients could fake their address and dodge the rate limits.
	TrustProxy bool

	// Public identity and mail (API only).
	PublicAPIURL     string // base URL clients reach the API at; used in image links
	FreeStorageBytes int64  // free storage every artist starts with
	SMTPHost         string // empty = log emails instead of sending (development only)
	SMTPPort         string
	SMTPUser         string
	SMTPPassword     string
	SMTPFrom         string
	// Plunk (https://www.useplunk.com): sends the emails through its API when PLUNK_API_KEY is set.
	PlunkAPIKey string // secret key, starts with sk_
	PlunkURL    string // API base URL; empty means Plunk's own
	// EditorialOwnerEmail is the administrator account that owns the editorial playlists the admin site edits.
	EditorialOwnerEmail string
	MailFrom            string // "Name <address>" the emails come from; the address's domain must be verified at the provider

	// Podcast feeds.
	FeedAllowPrivate    bool          // dev only: let feed fetches reach private addresses
	FeedRefreshInterval time.Duration // how often subscribed shows are re-fetched
	FeedFetchTimeout    time.Duration

	// YouTube connect and transfers. The connect feature is off unless
	// GOOGLE_CLIENT_ID is set (or YOUTUBE_FAKE is on in development).
	GoogleClientID      string
	GoogleClientSecret  string
	YouTubeRedirectURL  string // where Google sends the browser back to; must match the OAuth client
	YouTubeAppRedirect  string // where the browser goes after connecting (the app's deep link)
	TokenEncKey         []byte // 32 bytes; encrypts stored OAuth tokens
	YouTubeFake         bool   // dev only: a built-in fake YouTube so the flow works without Google
	TransferConcurrency int    // how many transfers are worked on at once

	// Automatic audio import from the artist's own YouTube videos with yt-dlp. Off unless YOUTUBE_AUDIO_FETCH=true.
	// It is separate from the YouTube Data API connection: it downloads the audio-only stream.
	YoutubeAudioFetch bool
	YtdlpPath         string
	YtdlpJSRuntime    string        // "node": YouTube's player needs a JavaScript runtime
	YtdlpMaxBytes     int64         // largest audio file accepted
	YtdlpMaxDuration  time.Duration // longest video accepted
	YtdlpTimeout      time.Duration

	// Sign in with Apple / Google.
	AppleBundleIDs        []string // app ids whose Apple identity tokens are accepted; empty turns Apple sign-in off
	AppleAuthFake         bool     // dev only: accept "fake:<id>:<email>" as an Apple identity token
	GoogleAuthClientID    string   // OAuth client (web application) for sign-in; defaults to GOOGLE_CLIENT_ID
	GoogleAuthSecret      string
	GoogleAuthRedirectURL string // Google sends the browser back here
	GoogleAuthAppRedirect string // where the browser goes next: the app's deep link
	GoogleAuthFake        bool   // dev only: a built-in fake Google

	// Push notifications through Expo.
	ExpoAccessToken string // optional; needed only when "enhanced push security" is on for the Expo project
	PushFake        bool   // dev only: send nothing, keep the messages readable at GET /v1/dev/push-outbox

	// The Privacy Policy and Terms pages.
	LegalOperator string // who runs the service, as the pages name it
	LegalEmail    string // contact address shown on the pages

	// Bulk catalogue import (API only).
	AdminSiteURL        string // base URL of the admin site, for links in import answers; empty leaves them out
	ImportMaxTotalBytes int64  // all files of one import together
	ImportMinFreeBytes  int64  // an import is refused if it would leave less free disk than this; 0 turns the check off
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
	partMiB, err := getInt("UPLOAD_PART_MB", 8)
	if err != nil {
		return c, err
	}
	// Object stores require every part but the last to be at least 5 MiB.
	if partMiB < 5 || partMiB > 512 {
		return c, fmt.Errorf("UPLOAD_PART_MB must be between 5 and 512")
	}
	c.UploadPartBytes = int64(partMiB) << 20
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

	if c.S3PublicHostFromRequest, err = getBool("S3_PUBLIC_HOST_FROM_REQUEST", false); err != nil {
		return c, err
	}
	if c.S3PublicHostFromRequest && c.Env == "production" {
		return c, fmt.Errorf("S3_PUBLIC_HOST_FROM_REQUEST must not be enabled in production")
	}
	if c.TrustProxy, err = getBool("TRUST_PROXY", false); err != nil {
		return c, err
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

	c.GoogleClientID = os.Getenv("GOOGLE_CLIENT_ID")
	c.GoogleClientSecret = os.Getenv("GOOGLE_CLIENT_SECRET")
	c.YouTubeRedirectURL = get("YOUTUBE_REDIRECT_URL", c.PublicAPIURL+"/v1/integrations/youtube/callback")
	c.YouTubeAppRedirect = get("YOUTUBE_APP_REDIRECT", "blacmusic://youtube/connected")
	if c.YouTubeFake, err = getBool("YOUTUBE_FAKE", false); err != nil {
		return c, err
	}
	if c.YouTubeFake && c.Env == "production" {
		return c, fmt.Errorf("YOUTUBE_FAKE must not be enabled in production")
	}
	if c.TransferConcurrency, err = getInt("TRANSFER_CONCURRENCY", 1); err != nil {
		return c, err
	}
	if c.TransferConcurrency < 1 || c.TransferConcurrency > 8 {
		return c, fmt.Errorf("TRANSFER_CONCURRENCY must be 1-8")
	}
	if c.YoutubeAudioFetch, err = getBool("YOUTUBE_AUDIO_FETCH", false); err != nil {
		return c, err
	}
	c.YtdlpPath = get("YTDLP_PATH", "yt-dlp")
	c.YtdlpJSRuntime = get("YTDLP_JS_RUNTIME", "node")
	maxMB, err := getInt("YTDLP_MAX_MB", 150)
	if err != nil {
		return c, err
	}
	if maxMB < 1 || maxMB > 2048 {
		return c, fmt.Errorf("YTDLP_MAX_MB must be 1-2048")
	}
	c.YtdlpMaxBytes = int64(maxMB) << 20
	if c.YtdlpMaxDuration, err = getDuration("YTDLP_MAX_DURATION", 20*time.Minute); err != nil {
		return c, err
	}
	if c.YtdlpTimeout, err = getDuration("YTDLP_TIMEOUT", 8*time.Minute); err != nil {
		return c, err
	}
	c.AppleBundleIDs = nil
	if ids := get("APPLE_BUNDLE_IDS", "com.blac.music,com.blac.music.dev"); ids != "none" {
		for _, id := range strings.Split(ids, ",") {
			if id = strings.TrimSpace(id); id != "" {
				c.AppleBundleIDs = append(c.AppleBundleIDs, id)
			}
		}
	}
	if c.AppleAuthFake, err = getBool("APPLE_AUTH_FAKE", false); err != nil {
		return c, err
	}
	c.GoogleAuthClientID = get("GOOGLE_AUTH_CLIENT_ID", c.GoogleClientID)
	c.GoogleAuthSecret = get("GOOGLE_AUTH_CLIENT_SECRET", c.GoogleClientSecret)
	c.GoogleAuthRedirectURL = get("GOOGLE_AUTH_REDIRECT_URL", c.PublicAPIURL+"/v1/auth/google/callback")
	c.GoogleAuthAppRedirect = get("GOOGLE_AUTH_APP_REDIRECT", "blacmusic://auth/google")
	if c.GoogleAuthFake, err = getBool("GOOGLE_AUTH_FAKE", false); err != nil {
		return c, err
	}
	c.ExpoAccessToken = os.Getenv("EXPO_ACCESS_TOKEN")
	c.PlunkAPIKey = os.Getenv("PLUNK_API_KEY")
	c.PlunkURL = os.Getenv("PLUNK_API_URL")
	c.EditorialOwnerEmail = strings.TrimSpace(os.Getenv("EDITORIAL_OWNER_EMAIL"))
	c.MailFrom = get("MAIL_FROM", c.SMTPFrom)
	c.LegalOperator = get("LEGAL_OPERATOR", "Blac Music")
	c.LegalEmail = os.Getenv("LEGAL_CONTACT_EMAIL")
	if c.PushFake, err = getBool("PUSH_FAKE", false); err != nil {
		return c, err
	}
	if c.PushFake && c.Env == "production" {
		return c, fmt.Errorf("PUSH_FAKE must not be enabled in production")
	}
	if (c.AppleAuthFake || c.GoogleAuthFake) && c.Env == "production" {
		return c, fmt.Errorf("APPLE_AUTH_FAKE and GOOGLE_AUTH_FAKE must not be enabled in production")
	}
	if k := os.Getenv("TOKEN_ENC_KEY"); k != "" {
		raw, derr := hex.DecodeString(k)
		if derr != nil || len(raw) != 32 {
			return c, fmt.Errorf("TOKEN_ENC_KEY must be 64 hex characters (32 bytes)")
		}
		c.TokenEncKey = raw
	}

	c.AdminSiteURL = strings.TrimRight(strings.TrimSpace(os.Getenv("ADMIN_SITE_URL")), "/")
	importMB, err := getInt("IMPORT_MAX_TOTAL_MB", 1024)
	if err != nil {
		return c, err
	}
	if importMB < 1 {
		return c, fmt.Errorf("IMPORT_MAX_TOTAL_MB must be at least 1")
	}
	c.ImportMaxTotalBytes = int64(importMB) << 20
	minFreeGB, err := getInt("IMPORT_MIN_FREE_GB", 2)
	if err != nil {
		return c, err
	}
	if minFreeGB < 0 {
		return c, fmt.Errorf("IMPORT_MIN_FREE_GB must not be negative")
	}
	c.ImportMinFreeBytes = int64(minFreeGB) << 30

	if !needJWT {
		return c, nil
	}
	if c.GoogleAuthClientID != "" && c.GoogleAuthSecret == "" {
		return c, fmt.Errorf("GOOGLE_AUTH_CLIENT_SECRET (or GOOGLE_CLIENT_SECRET) must be set with the client id")
	}
	if c.GoogleClientID != "" && c.GoogleClientSecret == "" {
		return c, fmt.Errorf("GOOGLE_CLIENT_SECRET must be set with GOOGLE_CLIENT_ID")
	}
	if c.GoogleClientID != "" && c.Env == "production" && c.TokenEncKey == nil {
		return c, fmt.Errorf("TOKEN_ENC_KEY must be set in production when YouTube connect is enabled")
	}
	if c.JWTSecret == "" && c.Env == "development" {
		c.JWTSecret = devJWTSecret
	}
	if c.Env == "production" && c.SMTPHost == "" && c.PlunkAPIKey == "" {
		return c, fmt.Errorf("PLUNK_API_KEY or SMTP_HOST must be set in production (verification emails cannot be sent otherwise)")
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
