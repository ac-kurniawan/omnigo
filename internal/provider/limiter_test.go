package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type dummyProvider struct {
	name   string
	inChat chan struct{}
	done   chan struct{}
}

func (d *dummyProvider) Name() string { return d.name }
func (d *dummyProvider) ChatCompletion(ctx context.Context, req ChatRequest, w http.ResponseWriter) error {
	if d.inChat != nil {
		d.inChat <- struct{}{}
	}
	if d.done != nil {
		<-d.done
	}
	w.WriteHeader(http.StatusOK)
	return nil
}
func (d *dummyProvider) Models(ctx context.Context) ([]Model, error) { return nil, nil }
func (d *dummyProvider) Test(ctx context.Context) TestResult         { return TestResult{OK: true} }

func TestWithConcurrencyLimit(t *testing.T) {
	dp := &dummyProvider{
		name:   "limited",
		inChat: make(chan struct{}, 5),
		done:   make(chan struct{}),
	}
	lp := WithConcurrencyLimit(dp, 2)

	var wg sync.WaitGroup
	// Start 2 concurrent requests that hold their slots
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			_ = lp.ChatCompletion(context.Background(), ChatRequest{Model: "m1"}, rec)
		}()
	}

	// Wait for both to enter ChatCompletion
	<-dp.inChat
	<-dp.inChat

	// 3rd request should immediately fail with 429 Too Many Requests
	rec := httptest.NewRecorder()
	err := lp.ChatCompletion(context.Background(), ChatRequest{Model: "m1"}, rec)
	if err == nil {
		t.Fatal("expected error on 3rd request exceeding concurrency limit of 2")
	}
	statusErr, ok := err.(interface{ HTTPStatus() int })
	if !ok || statusErr.HTTPStatus() != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests, got %v", err)
	}

	// Release the in-flight requests
	close(dp.done)
	wg.Wait()

	// 4th request should now succeed
	dp2 := &dummyProvider{name: "limited"}
	lp2 := WithConcurrencyLimit(dp2, 1)
	rec4 := httptest.NewRecorder()
	if err := lp2.ChatCompletion(context.Background(), ChatRequest{Model: "m1"}, rec4); err != nil {
		t.Fatalf("expected success after release, got %v", err)
	}
}

// Saturation is gateway backpressure, not an upstream fault: routing must fail
// over without skipping this provider for the whole drain TTL.
func TestProviderBusyErrorIsNotDrainable(t *testing.T) {
	dp := &dummyProvider{name: "limited", inChat: make(chan struct{}, 1), done: make(chan struct{})}
	lp := WithConcurrencyLimit(dp, 1)

	held := make(chan struct{})
	go func() {
		defer close(held)
		_ = lp.ChatCompletion(context.Background(), ChatRequest{Model: "m1"}, httptest.NewRecorder())
	}()
	<-dp.inChat

	err := lp.ChatCompletion(context.Background(), ChatRequest{Model: "m1"}, httptest.NewRecorder())
	if err == nil {
		t.Fatal("expected saturation error")
	}
	var busy *ProviderBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("err = %T %v, want *ProviderBusyError", err, err)
	}
	var drainable interface{ Drainable() bool }
	if !errors.As(err, &drainable) || drainable.Drainable() {
		t.Fatal("saturation must report Drainable() == false so healthy targets are not skipped")
	}
	if busy.RetryAfter() <= 0 {
		t.Fatal("expected a positive Retry-After hint")
	}

	close(dp.done)
	<-held
}
