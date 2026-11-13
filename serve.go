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
	listen := flags.String("listen", envOr("OSHIRASE_LISTEN", defaultListen),
		"address to listen on; several separated by commas, e.g. a VM bridge's besides loopback")
	backendName := flags.String("backend", envOr("OSHIRASE_BACKEND", "auto"),
		"auto, log, email, or a platform backend (macos, freedesktop)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	backend, err := selectBackend(*backendName)
	if err != nil {
		return err
	}
	routes := newRouter(backend)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", healthHandler(routes))
	mux.HandleFunc("POST /v1/notify", notifyHandler(routes))
	mux.HandleFunc("POST /v1/withdraw", withdrawHandler(backend))

	server := &http.Server{
		Handler:           rejectBrowsers(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      notifyTimeout + 5*time.Second,
	}

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
		// Asked to stop is a clean exit, even when a slow delivery outlasts
		// the grace period.
		log.Print("shutting down")
		if err := shutdown(); err != nil {
			log.Printf("shutdown: %v", err)
		}
		return nil
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

func healthHandler(routes *router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		response := HealthResponse{
			Protocol: ProtocolVersion,
			Version:  version,
			Backend:  routes.primary.Name(),
			Via:      routes.available(),
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func notifyHandler(routes *router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var notification Notification
		if !decodeJSON(w, r, &notification) {
			return
		}
		if err := notification.Normalize(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		backend, err := routes.pick(notification.Via)
		if err != nil {
			log.Printf("cannot deliver via %s: %v", notification.Via, err)
			writeError(w, http.StatusNotImplemented, err.Error())
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), notifyTimeout)
		defer cancel()
		if err := backend.Notify(ctx, notification); err != nil {
			log.Printf("delivery failed: backend=%s source=%q title=%q: %v", backend.Name(), notification.Source, notification.Title, err)
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}

		log.Printf("delivered: backend=%s urgency=%s source=%q title=%q", backend.Name(), notification.Urgency, notification.Source, notification.Title)
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
		var request WithdrawRequest
		if !decodeJSON(w, r, &request) {
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

// decodeJSON reads a request body into value, or answers the error itself
// and returns false. Requiring JSON also means a browser could only send the
// request after a CORS preflight, which the daemon never answers.
func decodeJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestSize)
	if err := json.NewDecoder(r.Body).Decode(value); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request is over %d bytes", maxRequestSize))
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
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
