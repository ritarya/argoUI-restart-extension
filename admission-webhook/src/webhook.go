package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/redis/go-redis/v9"
)

const (
	annotationRestartedAt = "kubectl.kubernetes.io/restartedAt"
	annotationRFCID       = "rfc-validation/rfc-id"
	annotationBypass      = "rfc-validation/emergency-bypass"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	redisClient := redis.NewClient(&redis.Options{
		Addr: os.Getenv("REDIS_ADDR"),
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/validate", func(w http.ResponseWriter, r *http.Request) {
		handleAdmission(w, r, redisClient, logger)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	certFile := os.Getenv("TLS_CERT_FILE")
	keyFile := os.Getenv("TLS_KEY_FILE")
	logger.Info("starting webhook server", "addr", ":8443")
	if err := http.ListenAndServeTLS(":8443", certFile, keyFile, mux); err != nil {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
}

func handleAdmission(w http.ResponseWriter, r *http.Request, rc *redis.Client, logger *slog.Logger) {
	var review admissionv1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
		http.Error(w, "decode error", http.StatusBadRequest)
		return
	}
	review.Response = evaluate(r.Context(), review.Request, rc, logger)
	review.Response.UID = review.Request.UID
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(review)
}

func evaluate(ctx context.Context, req *admissionv1.AdmissionRequest, rc *redis.Client, logger *slog.Logger) *admissionv1.AdmissionResponse {
	allow := func() *admissionv1.AdmissionResponse { return &admissionv1.AdmissionResponse{Allowed: true} }
	deny := func(msg string) *admissionv1.AdmissionResponse {
		return &admissionv1.AdmissionResponse{
			Allowed: false,
			Result:  &metav1.Status{Message: msg},
		}
	}

	// Only intercept PATCH on Deployments
	if req.Kind.Kind != "Deployment" || req.Operation != admissionv1.Update {
		return allow()
	}

	var deployment appsv1.Deployment
	if err := json.Unmarshal(req.Object.Raw, &deployment); err != nil {
		logger.Error("unmarshal deployment", "err", err)
		return deny("internal error: could not parse deployment")
	}

	// Only care about patches that include restartedAt — not every Deployment update
	podAnnotations := deployment.Spec.Template.Annotations
	if podAnnotations[annotationRestartedAt] == "" {
		return allow()
	}

	metaAnnotations := deployment.Annotations

	// Emergency bypass — cluster-admins only
	if metaAnnotations[annotationBypass] == "true" {
		for _, g := range req.UserInfo.Groups {
			if g == "cluster-admins" {
				logger.Warn("emergency bypass used",
					"deployment", deployment.Name, "namespace", deployment.Namespace,
					"user", req.UserInfo.Username)
				logger.Info("RFC/EmergencyBypass", "metric", true,
					"deployment", deployment.Name, "namespace", deployment.Namespace,
					"user", req.UserInfo.Username)
				return allow()
			}
		}
		return deny("rfc-validation/emergency-bypass requires cluster-admins group membership")
	}

	rfcID := metaAnnotations[annotationRFCID]
	if rfcID == "" {
		return deny("rfc-validation/rfc-id annotation is required to restart a Deployment")
	}

	cacheKey := fmt.Sprintf("rfc:%s:%s", rfcID, deployment.Namespace)
	data, err := rc.Get(ctx, cacheKey).Bytes()
	if err != nil {
		return deny(fmt.Sprintf(
			"no approved validation found for RFC %s in namespace %s; validate via the RFC Validation tab first",
			rfcID, deployment.Namespace,
		))
	}

	var result struct {
		Approved bool `json:"approved"`
	}
	if err := json.Unmarshal(data, &result); err != nil || !result.Approved {
		return deny(fmt.Sprintf("RFC %s is not approved or has expired", rfcID))
	}

	return allow()
}
