package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/zvdy/terraform-provider-lsmrust/internal/mockserver"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	delay := flag.Duration("provision-delay", 2*time.Second, "how long databases stay pending/updating before becoming ready")
	apiKey := flag.String("api-key", os.Getenv("LSMRUST_API_KEY"), "if set, require 'Authorization: Bearer <key>' on every request")
	flag.Parse()

	logger := log.New(os.Stderr, "mock-control-plane: ", log.LstdFlags)
	srv := mockserver.New(mockserver.Config{
		ProvisionDelay: *delay,
		APIKey:         *apiKey,
		Logger:         logger,
	})

	logger.Printf("listening on %s (provision delay %s, auth %v)", *addr, *delay, *apiKey != "")
	if err := http.ListenAndServe(*addr, srv); err != nil {
		logger.Fatal(err)
	}
}
