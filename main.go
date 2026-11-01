// agende schedules posts on one Mastodon instance. The instance stores and
// publishes them; this app keeps sessions, settings and an audit log. See README.md.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // the runtime image has no zoneinfo

	"github.com/redis/go-redis/v9"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets
var assetFS embed.FS

// version is set at build time, see the Dockerfile.
var version = "dev"

const drainTimeout = 100 * time.Second

type config struct {
	listen        string
	baseURL       string
	instance      string
	domain        string
	clientID      string
	clientSecret  string
	encryptionKey string
	lang          string
	timezone      string
	markdown      bool
	localOnly     bool
	expiring      bool
	maxUpload     int64
	dbHost        string
	dbPort        string
	dbUser        string
	dbPassword    string
	dbName        string
	dbSSLMode     string
	redisAddr     string
	redisPassword string
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func loadConfig() (config, error) {
	c := config{
		listen:        env("AGENDE_LISTEN", ":8080"),
		baseURL:       strings.TrimRight(os.Getenv("AGENDE_BASE_URL"), "/"),
		instance:      strings.TrimRight(os.Getenv("AGENDE_INSTANCE"), "/"),
		clientID:      os.Getenv("AGENDE_CLIENT_ID"),
		clientSecret:  os.Getenv("AGENDE_CLIENT_SECRET"),
		encryptionKey: os.Getenv("AGENDE_ENCRYPTION_KEY"),
		lang:          env("AGENDE_LANG", "en"),
		timezone:      env("AGENDE_TIMEZONE", "UTC"),
		markdown:      os.Getenv("AGENDE_MARKDOWN") == "true",
		localOnly:     os.Getenv("AGENDE_LOCAL_ONLY") == "true",
		expiring:      os.Getenv("AGENDE_EXPIRING") == "true",
		dbHost:        os.Getenv("AGENDE_DB_HOST"),
		dbPort:        env("AGENDE_DB_PORT", "5432"),
		dbUser:        env("AGENDE_DB_USER", "agende"),
		dbPassword:    os.Getenv("AGENDE_DB_PASSWORD"),
		dbName:        env("AGENDE_DB_NAME", "agende"),
		dbSSLMode:     env("AGENDE_DB_SSLMODE", "prefer"),
		redisAddr:     os.Getenv("AGENDE_REDIS_ADDR"),
		redisPassword: os.Getenv("AGENDE_REDIS_PASSWORD"),
	}
	mb, err := strconv.Atoi(env("AGENDE_MAX_UPLOAD_MB", "100"))
	if err != nil || mb < 1 {
		return c, fmt.Errorf("AGENDE_MAX_UPLOAD_MB must be a positive number")
	}
	c.maxUpload = int64(mb) << 20
	if u, err := url.Parse(c.instance); err != nil || u.Scheme != "https" || u.Host == "" {
		return c, fmt.Errorf("AGENDE_INSTANCE must be a URL like https://example.social")
	} else {
		c.domain = u.Host
	}
	if u, err := url.Parse(c.baseURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return c, fmt.Errorf("AGENDE_BASE_URL must be an https URL")
	}
	if _, err := time.LoadLocation(c.timezone); err != nil {
		return c, fmt.Errorf("AGENDE_TIMEZONE: %w", err)
	}
	switch {
	case c.clientID == "" || c.clientSecret == "":
		return c, fmt.Errorf("AGENDE_CLIENT_ID and AGENDE_CLIENT_SECRET are required")
	case len(c.encryptionKey) < 32:
		return c, fmt.Errorf("AGENDE_ENCRYPTION_KEY must have at least 32 characters")
	case c.dbHost == "" || c.dbPassword == "":
		return c, fmt.Errorf("AGENDE_DB_HOST and AGENDE_DB_PASSWORD are required")
	case c.redisAddr == "":
		return c, fmt.Errorf("AGENDE_REDIS_ADDR is required")
	}
	return c, nil
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local server and exit")
	flag.Parse()
	if *healthcheck {
		resp, err := http.Get("http://127.0.0.1" + env("AGENDE_LISTEN", ":8080") + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	auditBox, err := newBox(cfg.encryptionKey, "audit")
	if err != nil {
		log.Fatal(err)
	}
	tokenBox, err := newBox(cfg.encryptionKey, "session-token")
	if err != nil {
		log.Fatal(err)
	}
	db, err := openStore(ctx, cfg, auditBox)
	if err != nil {
		log.Fatal(err)
	}
	if err := db.check(ctx); err != nil {
		log.Fatalf("database: %v (was schema.sql applied?)", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: cfg.redisAddr, Password: cfg.redisPassword})
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis: %v", err)
	}
	tmpl, err := parseTemplates()
	if err != nil {
		log.Fatal(err)
	}
	api := &mastodon{
		base:   cfg.instance,
		client: &http.Client{Timeout: 15 * time.Second},
		upload: &http.Client{Timeout: 5 * time.Minute},
	}
	s := newServer(cfg, tmpl, api, db, &sessions{rdb: rdb, box: tokenBox}, rdb)
	go func() {
		for range time.Tick(time.Minute) {
			s.sessions.sweep(ctx, s.revokeToken)
		}
	}()
	log.Printf("agende %s listening on %s for %s", version, cfg.listen, cfg.domain)
	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      15 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}
	stop, cancel := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-stop.Done()
		// A rollout must not cut a series in the middle.
		done, cancel := context.WithTimeout(ctx, drainTimeout)
		defer cancel()
		if err := srv.Shutdown(done); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-drained
}

func parseTemplates() (*template.Template, error) {
	return template.New("").Funcs(template.FuncMap{
		"inc": func(i int) int { return i + 1 },
	}).ParseFS(templateFS, "templates/*.html")
}
