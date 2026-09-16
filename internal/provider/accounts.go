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
	drains map[string]accountDrain
	now    func() time.Time
}

type accountDrain struct {
	until       time.Time
	reason      string
	clientError bool
}

type accountUnavailableError struct {
	reason   string
	cooldown time.Duration
	// clientError records that the failure which cooled this account was a
	// client-side 4xx. A bad prompt must not drain the combo target, so the
	// classification has to survive the cooldown.
	clientError bool
}

func (e *accountUnavailableError) Error() string           { return e.reason }
func (e *accountUnavailableError) Cooldown() time.Duration { return e.cooldown }
func (e *accountUnavailableError) DrainReason() string     { return e.reason }
func (e *accountUnavailableError) Drainable() bool         { return !e.clientError }

func (p *AccountPool) Available(store CredStore) []Credentials {
	accounts, _ := p.AvailableWithError(store)
	return accounts
}

func (p *AccountPool) AvailableWithError(store CredStore) ([]Credentials, error) {
	accounts := accountsFromStore(store)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.drains == nil {
		p.drains = make(map[string]accountDrain)
	}
	now := p.currentTimeLocked()
	healthy := make([]Credentials, 0, len(accounts))
	var unavailable error
	for i, account := range accounts {
		key := accountKey(account, i)
		drain := p.drains[key]
		if drain.until.IsZero() || !now.Before(drain.until) {
			delete(p.drains, key)
			healthy = append(healthy, account)
			continue
		}
		if unavailable == nil && drain.reason != "" {
			unavailable = &accountUnavailableError{reason: drain.reason, cooldown: drain.until.Sub(now), clientError: drain.clientError}
		}
	}
	return healthy, unavailable
}

func (p *AccountPool) MarkFailed(account Credentials, err error) {
	cooldown := DefaultAccountCooldown
	var withCooldown interface{ Cooldown() time.Duration }
	if errors.As(err, &withCooldown) && withCooldown.Cooldown() > 0 {
		cooldown = withCooldown.Cooldown()
	}
	var reason string
	var withReason interface{ DrainReason() string }
	if errors.As(err, &withReason) {
		reason = withReason.DrainReason()
	}
	clientErr := isClientError(err)
	p.mu.Lock()
	if p.drains == nil {
		p.drains = make(map[string]accountDrain)
	}
	p.drains[accountKey(account, 0)] = accountDrain{until: p.currentTimeLocked().Add(cooldown), reason: reason, clientError: clientErr}
	p.mu.Unlock()
}

// isClientError reports whether an upstream failure is the caller's fault (a
// malformed or unsupported request) rather than the provider's. Such failures
// must not drain a combo target.
func isClientError(err error) bool {
	var status interface{ HTTPStatus() int }
	if !errors.As(err, &status) {
		return false
	}
	switch status.HTTPStatus() {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
		return true
	}
	return false
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

	mu      sync.RWMutex
	current Credentials
}

func (s *scopedStore) Get() Credentials {
	if pooled, ok := s.store.(AccountStore); ok {
		for _, account := range pooled.Accounts() {
			if s.identity != "" && account.Identity() == s.identity {
				return account
			}
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

func (s *scopedStore) Put(credentials Credentials) error {
	if pooled, ok := s.store.(AccountStore); ok {
		return pooled.PutAccount(s.identity, credentials)
	}
	s.mu.Lock()
	s.current = credentials
	s.mu.Unlock()
	return s.store.Put(credentials)
}

type AttemptWriter struct {
	destination http.ResponseWriter
	header      http.Header
	body        bytes.Buffer
	status      int
	stream      bool
	committed   bool
}

func NewStreamingAttemptWriter(destination http.ResponseWriter, stream bool) *AttemptWriter {
	return &AttemptWriter{destination: destination, header: make(http.Header), status: http.StatusOK, stream: stream}
}

func (w *AttemptWriter) Header() http.Header { return w.header }

func (w *AttemptWriter) WriteHeader(status int) {
	if w.committed {
		return
	}
	if w.status == http.StatusOK {
		w.status = status
	}
}

func (w *AttemptWriter) Write(data []byte) (int, error) {
	if w.committed && w.destination != nil {
		return w.destination.Write(data)
	}
	return w.body.Write(data)
}

func (w *AttemptWriter) Flush() {
	if !w.stream || w.destination == nil || w.status < http.StatusOK || w.status >= http.StatusMultipleChoices {
		return
	}
	if !w.committed {
		_ = w.commitTo(w.destination)
	}
	if flusher, ok := w.destination.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *AttemptWriter) Committed() bool {
	return w.committed
}

func (w *AttemptWriter) commitTo(dest http.ResponseWriter) error {
	if w.committed {
		return nil
	}
	for key := range dest.Header() {
		dest.Header().Del(key)
	}
	for key, values := range w.header {
		dest.Header()[key] = append([]string(nil), values...)
	}
	dest.WriteHeader(w.status)
	w.committed = true
	var err error
	if w.body.Len() > 0 {
		_, err = dest.Write(w.body.Bytes())
		w.body.Reset()
	}
	return err
}

func (w *AttemptWriter) Commit(destination http.ResponseWriter) error {
	if w.committed {
		if flusher, ok := destination.(http.Flusher); ok {
			flusher.Flush()
		}
		return nil
	}
	dest := destination
	if dest == nil {
		dest = w.destination
	}
	if dest == nil {
		return nil
	}
	err := w.commitTo(dest)
	if flusher, ok := dest.(http.Flusher); ok {
		flusher.Flush()
	}
	return err
}
