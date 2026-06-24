package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"
)

func main() {
	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	client, err := newITSMClient(
		os.Getenv("ITSM_BASE_URL"),
		os.Getenv("ITSM_TOKEN_URL"),
		os.Getenv("ITSM_CLIENT_ID"),
		os.Getenv("ITSM_CLIENT_SECRET"),
		os.Getenv("ITSM_USERNAME"),
		os.Getenv("ITSM_PASSWORD"),
		os.Getenv("ITSM_SCOPE"),
	)
	if err != nil {
		logger.Error("failed to create ITSM client", "err", err)
		os.Exit(1)
	}

	validator := &Validator{itsm: client, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("/validate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			RFCID     string `json:"rfc_id"`
			Namespace string `json:"namespace"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		// ArgoCD proxy extension forwards these headers after JWT validation
		if req.Namespace == "" {
			req.Namespace = r.Header.Get("Argocd-Application-Namespace")
		}
		user := r.Header.Get("Argocd-Username")

		result, err := validator.Validate(r.Context(), req.RFCID, req.Namespace)
		if err != nil {
			logger.Error("validation error", "err", err, "rfc_id", req.RFCID, "user", user)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		logger.Info("rfc_validation",
			"event", "rfc_validation",
			"rfc_id", req.RFCID,
			"user", user,
			"namespace", req.Namespace,
			"result", map[bool]string{true: "approved", false: "rejected"}[result.Approved],
			"reason", result.Reason,
			"timestamp", time.Now().UTC().Format(time.RFC3339),
		)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	addr := ":8080"
	logger.Info("starting rfc-validator", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
}
