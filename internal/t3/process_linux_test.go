package t3

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestNativeRuntimeProcessAndListenerOwnership(t *testing.T) {
	server := httptest.NewServer(nil)
	defer server.Close()
	origin, _ := url.Parse(server.URL)
	created, err := processCreated(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyRuntimeProcess(context.Background(), os.Getpid(), created.Add(time.Second), origin); err != nil {
		t.Fatal(err)
	}
	if err := verifyRuntimeProcess(context.Background(), os.Getpid(), created.Add(time.Hour), origin); err == nil {
		t.Fatal("PID reuse accepted")
	}
	server.Close()
	if err := verifyRuntimeProcess(context.Background(), os.Getpid(), created.Add(time.Second), origin); err == nil {
		t.Fatal("missing listener accepted")
	}
	if err := verifyRuntimeProcess(context.Background(), 99999999, time.Now(), origin); err == nil {
		t.Fatal("missing PID accepted")
	}
}
