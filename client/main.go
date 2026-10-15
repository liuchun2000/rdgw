package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"strings"

	"github.com/rancher/remotedialer"
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

	remotedialer.ClientConnect(context.Background(), addr, headers, nil, func(string, string) bool { return true }, nil)
}
