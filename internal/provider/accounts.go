package provider

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const DefaultAccountCooldown = 60 * time.Second

type AccountPool struct {
	mu     sync.Mutex
	drains map[string]time.Time
	now    func() time.Time
}

func (p *AccountPool) Available(store CredStore) []Credentials {
	accounts := accountsFromStore(store)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.drains == nil {
		p.drains = make(map[string]time.Time)
	}
	now := p.currentTimeLocked()
	healthy := make([]Credentials, 0, len(accounts))
	for i, account := range accounts {
		key := accountKey(account, i)
		until := p.drains[key]
		if until.IsZero() || !now.Before(until) {
			delete(p.drains, key)
			healthy = append(healthy, account)
		}
	}
	return healthy
}

func (p *AccountPool) MarkFailed(account Credentials, err error) {
	cooldown := DefaultAccountCooldown
	var withCooldown interface{ Cooldown() time.Duration }
	if errors.As(err, &withCooldown) && withCooldown.Cooldown() > 0 {
		cooldown = withCooldown.Cooldown()
	}
	p.mu.Lock()
	if p.drains == nil {
		p.drains = make(map[string]time.Time)
	}
	p.drains[accountKey(account, 0)] = p.currentTimeLocked().Add(cooldown)
	p.mu.Unlock()
}

func (p *AccountPool) currentTimeLocked() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *AccountPool) SetClock(now func() time.Time) {
	p.mu.Lock()
	p.now = now
	p.mu.Unlock()
}

func accountsFromStore(store CredStore) []Credentials {
	if pooled, ok := store.(AccountStore); ok {
		return pooled.Accounts()
	}
	account := store.Get()
	if account == (Credentials{}) {
		return nil
	}
	return []Credentials{account}
}

func accountKey(account Credentials, index int) string {
	if identity := account.Identity(); identity != "" {
		return identity
	}
	return fmt.Sprintf("account-%d", index)
}

func ScopedStore(store CredStore, account Credentials) CredStore {
	return &scopedStore{store: store, identity: account.Identity(), current: account}
}

type scopedStore struct {
	store    CredStore
	identity string
	current  Credentials
}

func (s *scopedStore) Get() Credentials {
	if pooled, ok := s.store.(AccountStore); ok {
		for _, account := range pooled.Accounts() {
			if s.identity != "" && account.Identity() == s.identity {
				return account
			}
		}
	}
	return s.current
}

func (s *scopedStore) Put(credentials Credentials) error {
	if pooled, ok := s.store.(AccountStore); ok {
		return pooled.PutAccount(s.identity, credentials)
	}
	s.current = credentials
	return s.store.Put(credentials)
}

type AttemptWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func NewAttemptWriter() *AttemptWriter {
	return &AttemptWriter{header: make(http.Header), status: http.StatusOK}
}

func (w *AttemptWriter) Header() http.Header { return w.header }

func (w *AttemptWriter) WriteHeader(status int) {
	if w.status == http.StatusOK {
		w.status = status
	}
}

func (w *AttemptWriter) Write(data []byte) (int, error) { return w.body.Write(data) }

func (w *AttemptWriter) Flush() {}

func (w *AttemptWriter) Commit(destination http.ResponseWriter) error {
	for key := range destination.Header() {
		destination.Header().Del(key)
	}
	for key, values := range w.header {
		destination.Header()[key] = append([]string(nil), values...)
	}
	destination.WriteHeader(w.status)
	_, err := destination.Write(w.body.Bytes())
	if flusher, ok := destination.(http.Flusher); ok {
		flusher.Flush()
	}
	return err
}
