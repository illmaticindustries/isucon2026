package isureal

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

func TestBulkAdmissionSharesCapacityAndCancelsWaitingWork(t *testing.T) {
	gate := newBulkAdmission(1)
	makeContext := func(ctx context.Context) *echo.Context {
		return echo.New().NewContext(httptest.NewRequest("GET", "/", nil).WithContext(ctx), httptest.NewRecorder())
	}
	entered, release := make(chan struct{}), make(chan struct{})
	firstDone := make(chan error, 1)
	wantErr := errors.New("handler failure")
	go func() {
		firstDone <- gate(func(*echo.Context) error {
			close(entered)
			<-release
			return wantErr
		})(makeContext(context.Background()))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first handler did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	called := make(chan struct{}, 1)
	// A separately wrapped endpoint shares the same admission capacity.
	go func() {
		secondDone <- gate(func(*echo.Context) error { called <- struct{}{}; return nil })(makeContext(ctx))
	}()
	cancel()
	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter did not return")
	}
	select {
	case <-called:
		t.Fatal("waiting handler ran while capacity was occupied")
	default:
	}
	close(release)
	if err := <-firstDone; err != wantErr {
		t.Fatalf("handler error changed: %v", err)
	}
	// Handler failure must release capacity for subsequent work.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := gate(func(*echo.Context) error { return nil })(makeContext(ctx)); err != nil {
		t.Fatal(err)
	}
}
