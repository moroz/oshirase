package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const defaultListen = "127.0.0.1:4649"

// How long a backend may take to show one notification.
const notifyTimeout = 10 * time.Second

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := flags.String("listen", envOr("OSHIRASE_LISTEN", defaultListen),
		"address to listen on; several separated by commas, e.g. a VM bridge's besides loopback")
	backendName := flags.String("backend", "auto", "auto, log, or a platform backend (macos, freedesktop)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	backend, err := selectBackend(*backendName)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", healthHandler(backend))
	mux.HandleFunc("POST /v1/notify", notifyHandler(backend))
	mux.HandleFunc("POST /v1/withdraw", withdrawHandler(backend))

	server := &http.Server{
		Handler:           rejectBrowsers(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      notifyTimeout + 5*time.Second,
	}

	// Bind every address before serving any, so a daemon that cannot have
	// all of them fails at once (and its supervisor retries) rather than
	// running half-reachable.
	listeners, err := listenAll(listenAddresses(*listen))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, len(listeners))
	for _, listener := range listeners {
		go func() {
			log.Printf("oshirase %s listening on %s, backend %s", version, listener.Addr(), backend.Name())
			serveErr <- server.Serve(listener)
		}()
	}

	shutdown := func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
	select {
	case err := <-serveErr:
		// One listener failing takes the others down with it.
		_ = shutdown()
		return err
	case <-ctx.Done():
		log.Print("shutting down")
		return shutdown()
	}
}

// listenAddresses splits a comma-separated -listen value, dropping blanks.
func listenAddresses(value string) []string {
	var addresses []string
	for _, address := range strings.Split(value, ",") {
		if address = strings.TrimSpace(address); address != "" {
			addresses = append(addresses, address)
		}
	}
	return addresses
}

func listenAll(addresses []string) ([]net.Listener, error) {
	if len(addresses) == 0 {
		return nil, errors.New("no address to listen on")
	}
	var listeners []net.Listener
	for _, address := range addresses {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			for _, open := range listeners {
				open.Close()
			}
			return nil, err
		}
		listeners = append(listeners, listener)
	}
	return listeners, nil
}

// rejectBrowsers turns away anything a web page could send. A page on the
// host can reach 127.0.0.1 too; browsers put Origin on every cross-origin
// request and on every POST, and no legitimate client of this daemon does.
func rejectBrowsers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			writeError(w, http.StatusForbidden, "requests from browsers are not accepted")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func healthHandler(backend Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		response := HealthResponse{
			Protocol: ProtocolVersion,
			Version:  version,
			Backend:  backend.Name(),
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func notifyHandler(backend Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Requiring JSON also means a browser could only send this after a
		// CORS preflight, which the daemon never answers.
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestSize)
		var notification Notification
		if err := json.NewDecoder(r.Body).Decode(&notification); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request is over %d bytes", maxRequestSize))
				return
			}
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := notification.Normalize(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), notifyTimeout)
		defer cancel()
		if err := backend.Notify(ctx, notification); err != nil {
			log.Printf("delivery failed: source=%q title=%q: %v", notification.Source, notification.Title, err)
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}

		log.Printf("delivered: urgency=%s source=%q title=%q", notification.Urgency, notification.Source, notification.Title)
		response := NotifyResponse{Delivered: true, Backend: backend.Name()}
		writeJSON(w, http.StatusOK, response)
	}
}

func withdrawHandler(backend Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		withdrawer, ok := backend.(Withdrawer)
		if !ok {
			writeError(w, http.StatusNotImplemented, "the "+backend.Name()+" backend cannot withdraw notifications")
			return
		}
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestSize)
		var request WithdrawRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := NormalizeTag(request.Tag); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), notifyTimeout)
		defer cancel()
		count, err := withdrawer.Withdraw(ctx, request.Tag)
		if err != nil {
			log.Printf("withdraw failed: tag=%q after %d: %v", request.Tag, count, err)
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		if count > 0 {
			log.Printf("withdrew %d: tag=%q", count, request.Tag)
		}
		writeJSON(w, http.StatusOK, WithdrawResponse{Withdrawn: count, Backend: backend.Name()})
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("writing response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, ErrorResponse{Error: message})
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
