package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/api"
	"github.com/ac-kurniawan/omnigo/internal/combo"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/dashboard"
	"github.com/ac-kurniawan/omnigo/internal/observability"
	"github.com/ac-kurniawan/omnigo/internal/quota"
	"github.com/ac-kurniawan/omnigo/internal/vault"
	"github.com/ac-kurniawan/omnigo/internal/version"
)

// appState holds the reloadable config snapshot and the vault store.
type appState struct {
	cfgPath  string
	authPath string
	store    *vault.Store

	cfg atomic.Pointer[config.Config]
}

func newState(cfgPath, authPath string, key []byte) (*appState, error) {
	s := &appState{cfgPath: cfgPath, authPath: authPath}
	store, err := vault.NewStore(authPath, key)
	if err != nil {
		return nil, err
	}
	s.store = store
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *appState) reload() error {
	cfg, err := config.Load(s.cfgPath)
	if err != nil {
		return err
	}
	s.cfg.Store(cfg)
	if _, err := os.Stat(s.authPath); err == nil {
		if err := s.store.Reload(); err != nil {
			return err
		}
	}
	return nil
}

func (s *appState) getCfg() *config.Config { return s.cfg.Load() }

// watch polls file mtimes and reloads when either changes.
func (s *appState) watch(ctx context.Context, interval time.Duration, metrics *observability.Metrics) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	cfgMod := modTime(s.cfgPath)
	authMod := modTime(s.authPath)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cm, am := modTime(s.cfgPath), modTime(s.authPath)
			if cm == cfgMod && am == authMod {
				continue
			}
			if err := s.reload(); err != nil {
				metrics.RecordConfigReload(false)
				log.Printf("reload: %v", err)
				continue
			}
			metrics.RecordConfigReload(true)
			cfgMod, authMod = cm, am
			log.Printf("config reloaded")
		}
	}
}

func modTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func (s *appState) mutate(fn func(*config.Config) error) error {
	if err := config.Mutate(s.cfgPath, fn); err != nil {
		return err
	}
	return s.reload()
}

func newApp(getCfg func() *config.Config, store *vault.Store, mutate config.MutateFunc, tracker *combo.Tracker, metrics *observability.Metrics, quotaCache *quota.Cache) (http.Handler, *api.ProviderRuntime) {
	apiHandler, runtime := api.NewRouterWithQuota(getCfg, store, mutate, tracker, version.Value, metrics, quotaCache)

	root := http.NewServeMux()
	root.Handle("/health", apiHandler)
	root.Handle("/actuator/", apiHandler)
	root.Handle("/v1/", apiHandler)
	root.Handle("/internal/", apiHandler)
	root.Handle("/", dashboard.NewHandlerWithQuota(getCfg, store, mutate, quotaCache, tracker))
	return metrics.Middleware(root), runtime
}

// Server lifecycle bounds are transport concerns, independent of the
// configured upstream timeout: a request may legitimately wait longer than a
// keep-alive or a shutdown drain should.
const (
	// idleTimeout closes keep-alive connections abandoned between requests.
	idleTimeout = 120 * time.Second
	// shutdownDrainTimeout gives in-flight generations time to finish before
	// the process exits.
	shutdownDrainTimeout = 30 * time.Second
)

func main() {
	dirFlag := flag.String("dir", "", "path to omnigo config directory (default: ~/.config/omnigo)")
	cfgFlag := flag.String("config", "", "path to config.yaml (overrides default in config dir)")
	keyFlag := flag.String("key", "", "path to secret key file (overrides default in config dir)")
	authFlag := flag.String("auth", "", "path to auth.yaml (overrides default in config dir)")
	showVersion := flag.Bool("version", false, "display version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("omnigo %s\n", version.Value)
		return
	}

	paths, err := config.ResolvePaths(*dirFlag, *cfgFlag, *authFlag, *keyFlag)
	if err != nil {
		log.Fatalf("paths: %v", err)
	}

	keyEnv := os.Getenv("OMNIGO_SECRET_KEY")
	if keyEnv == "" {
		keyEnv = os.Getenv("AIGO_SECRET_KEY")
	}
	key, err := vault.ResolveKey(keyEnv, paths.Key)
	if err != nil {
		log.Fatalf("secret key: %v", err)
	}

	state, err := newState(paths.Config, paths.Auth, key)
	if err != nil {
		log.Fatalf("startup: %v", err)
	}

	tracker := combo.NewTracker(paths.Drains)

	// Metrics are always constructed: the enabled gate is read from the live
	// config on every request, so toggling observability.metrics in config.yaml
	// takes effect on reload without a restart.
	metrics, err := observability.New(func() bool {
		cfg := state.getCfg()
		return cfg != nil && cfg.Observability.MetricsEnabled()
	}, version.Value)
	if err != nil {
		log.Fatalf("observability: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := metrics.Shutdown(shutdownCtx); err != nil {
			log.Printf("metrics shutdown: %v", err)
		}
	}()

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go state.watch(ctx, 2*time.Second, metrics)

	// Quota state is collected by a background syncer that polls each
	// provider's quota endpoint and stores the snapshot for the dashboard.
	// Polling never changes routing: an exhausted reading stays a badge, and
	// the next request still tries the account. The syncer reuses the
	// router's provider instances, so polling and serving share one set of
	// per-account token managers.
	quotaCache := quota.NewCache()
	app, runtime := newApp(state.getCfg, state.store, state.mutate, tracker, metrics, quotaCache)
	quotaSyncer := quota.NewSyncer(quota.Options{
		Cache:   quotaCache,
		Targets: runtime.QuotaTargets(state.getCfg),
		Fetch:   runtime.QuotaFetch(state.getCfg),
		Sink:    metrics,
		// Read live so a config reload applies on the next cycle.
		Interval: func() time.Duration { return state.getCfg().Quota.ParsedInterval() },
	})
	if state.getCfg().Quota.IsEnabled() {
		go quotaSyncer.Run(ctx)
		log.Printf("quota polling every %s", state.getCfg().Quota.ParsedInterval())
	}

	addr := state.getCfg().Server.Host + ":" + strconv.Itoa(state.getCfg().Server.Port)
	log.Printf("OmniGo %s listening on http://%s (config: %s)", version.Value, addr, paths.Config)
	if state.getCfg().Observability.MetricsEnabled() {
		log.Printf("metrics enabled at http://%s/actuator/metrics", addr)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           app,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       idleTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		log.Fatalf("server: %v", err)
	case <-sigCtx.Done():
		log.Printf("shutting down OmniGo gracefully...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}
}
