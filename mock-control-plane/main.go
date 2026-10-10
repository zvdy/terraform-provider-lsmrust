package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zvdy/terraform-provider-lsmrust/internal/mockserver"
)

func main() {
	addr := flag.String("addr", envOr("LSMRUST_ADDR", ":8080"), "listen address")
	delay := flag.Duration("provision-delay", 2*time.Second, "how long databases stay pending/updating before becoming ready")
	apiKey := flag.String("api-key", os.Getenv("LSMRUST_API_KEY"), "if set, require 'Authorization: Bearer <key>' on every request")
	certFile := flag.String("tls-cert", os.Getenv("LSMRUST_TLS_CERT"), "PEM certificate; with -tls-key enables HTTPS")
	keyFile := flag.String("tls-key", os.Getenv("LSMRUST_TLS_KEY"), "PEM private key")
	flag.Parse()

	logger := log.New(os.Stderr, "mock-control-plane: ", log.LstdFlags)
	srv := mockserver.New(mockserver.Config{
		ProvisionDelay: *delay,
		APIKey:         *apiKey,
		Logger:         logger,
	})

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		logger.Printf("listening on %s (provision delay %s, auth %v, tls %v)", *addr, *delay, *apiKey != "", *certFile != "")
		if *certFile != "" && *keyFile != "" {
			errc <- httpSrv.ListenAndServeTLS(*certFile, *keyFile)
		} else {
			errc <- httpSrv.ListenAndServe()
		}
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal(err)
		}
	case <-ctx.Done():
		logger.Print("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Fatal(err)
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
