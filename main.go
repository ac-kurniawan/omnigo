package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/api"
	"github.com/ac-kurniawan/omnigo/internal/combo"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/dashboard"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

var version = "dev"

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
func (s *appState) watch(ctx context.Context, interval time.Duration) {
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
				log.Printf("reload: %v", err)
				continue
			}
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

func newApp(getCfg func() *config.Config, store *vault.Store, mutate config.MutateFunc, tracker *combo.Tracker) http.Handler {
	apiHandler := api.NewRouter(getCfg, store, mutate, tracker)

	root := http.NewServeMux()
	root.Handle("/v1/", apiHandler)
	root.Handle("/internal/", apiHandler)
	root.Handle("/", dashboard.NewHandler(getCfg, store, mutate, tracker))
	return root
}

func main() {
	dirFlag := flag.String("dir", "", "path to omnigo config directory (default: ~/.config/omnigo)")
	cfgFlag := flag.String("config", "", "path to config.yaml (overrides default in config dir)")
	keyFlag := flag.String("key", "", "path to secret key file (overrides default in config dir)")
	authFlag := flag.String("auth", "", "path to auth.yaml (overrides default in config dir)")
	showVersion := flag.Bool("version", false, "display version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("omnigo %s\n", version)
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go state.watch(ctx, 2*time.Second)

	addr := state.getCfg().Server.Host + ":" + strconv.Itoa(state.getCfg().Server.Port)
	log.Printf("OmniGo %s listening on http://%s (config: %s)", version, addr, paths.Config)
	srv := &http.Server{
		Addr:              addr,
		Handler:           newApp(state.getCfg, state.store, state.mutate, tracker),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       state.getCfg().DefaultTimeout() * 2,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
