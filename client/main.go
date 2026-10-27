package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/liuchun2000/rdgw"
	"github.com/sirupsen/logrus"
)

var (
	addr      string
	id        string
	tokenFile string
	debug     bool
)

func main() {
	flag.StringVar(&addr, "connect", "ws://localhost:8123/connect", "Address to connect to")
	flag.StringVar(&id, "id", "foo", "Client ID")
	flag.StringVar(&tokenFile, "token-file", "", "Client token file")
	flag.BoolVar(&debug, "debug", true, "Debug logging")
	flag.Parse()
	if tokenFile == "" {
		logrus.Fatal("-token-file is required")
	}
	var token string
	if tokenFile != "" {
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			logrus.Fatalf("read token file: %v", err)
		}
		token = strings.TrimRight(string(data), "\r\n")
		if token == "" {
			logrus.Fatal("client token file is empty")
		}
	}

	if debug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	headers := http.Header{
		"X-Tunnel-ID": []string{id},
	}
	if token != "" {
		headers.Set("X-Tunnel-Token", token)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runWithReconnect(ctx, time.Second, time.Minute, func(ctx context.Context) error {
		return rdgw.ConnectToProxy(ctx, addr, headers, func(string, string) bool { return true }, nil, nil)
	}); err != nil {
		logrus.WithError(err).Error("client stopped")
	}
}

func runWithReconnect(ctx context.Context, initialBackoff, maxBackoff time.Duration, connect func(context.Context) error) error {
	if initialBackoff <= 0 || maxBackoff < initialBackoff {
		return fmt.Errorf("invalid reconnect backoff range")
	}
	backoff := initialBackoff
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		err := connect(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			logrus.WithError(err).Warn("connection failed; retrying")
		} else {
			logrus.Warn("connection ended; retrying")
		}
		jittered := backoff/2 + time.Duration(rand.Int63n(int64(backoff/2)+1))
		timer := time.NewTimer(jittered)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if backoff >= maxBackoff/2 {
			backoff = maxBackoff
		} else {
			backoff *= 2
		}
	}
}
