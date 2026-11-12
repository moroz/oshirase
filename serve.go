package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const defaultListen = "127.0.0.1:4649"

// How long a backend may take to show one notification.
const notifyTimeout = 10 * time.Second

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := flags.String("listen", envOr("OSHIRASE_LISTEN", defaultListen), "address to listen on")
	backendName := flags.String("backend", "auto", "auto, log, or a platform backend (macos)")
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

	server := &http.Server{
		Addr:              *listen,
		Handler:           rejectBrowsers(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      notifyTimeout + 5*time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("oshirase %s listening on %s, backend %s", version, *listen, backend.Name())
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		log.Print("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
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
