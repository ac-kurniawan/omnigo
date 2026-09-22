package provider

import (
	"bytes"
	"net/http"
	"sync"
)

// AccountPool lists the credentials a provider may try for one request, in
// stored order. It does not remember failures: a 429 or auth error fails over
// to the next account only within the current request, and the next request
// starts from the full list again. Remembering a cooldown here hid healthy
// accounts (including after re-authentication) until the process restarted.
type AccountPool struct{}

// Available returns every stored account, in order.
func (p *AccountPool) Available(store CredStore) []Credentials {
	return accountsFromStore(store)
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
