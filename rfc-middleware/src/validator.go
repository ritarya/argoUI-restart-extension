package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

type ValidationResult struct {
	Approved    bool   `json:"approved"`
	RFCTitle    string `json:"rfc_title"`
	Approver    string `json:"approver"`
	WindowStart string `json:"window_start"`
	WindowEnd   string `json:"window_end"`
	Reason      string `json:"reason"`
}

type Validator struct {
	cache  *Cache
	itsm   *ITSMClient
	logger *slog.Logger
}

func (v *Validator) Validate(ctx context.Context, rfcID, namespace string) (*ValidationResult, error) {
	if rfcID == "" {
		return &ValidationResult{Approved: false, Reason: "rfc_id is required"}, nil
	}

	cacheKey := fmt.Sprintf("rfc:%s:%s", rfcID, namespace)

	if cached, ok := v.cache.Get(ctx, cacheKey); ok {
		v.logger.Debug("cache hit", "key", cacheKey)
		return cached, nil
	}

	record, err := v.itsm.FetchChangeRecord(ctx, rfcID)
	if err != nil {
		v.logger.Error("itsm fetch failed", "rfc_id", rfcID, "err", err)
		return &ValidationResult{Approved: false, Reason: "itsm_unreachable"}, nil
	}

	result := v.evaluate(record, namespace)
	v.cache.Set(ctx, cacheKey, result)
	return result, nil
}

func (v *Validator) evaluate(r *ChangeRecord, namespace string) *ValidationResult {
	result := &ValidationResult{
		RFCTitle:    r.Title,
		Approver:    r.Approver,
		WindowStart: r.WindowStart.Format(time.RFC3339),
		WindowEnd:   r.WindowEnd.Format(time.RFC3339),
	}

	if r.State != "approved" && r.State != "implement" {
		result.Reason = fmt.Sprintf("RFC state is %q, expected approved or implement", r.State)
		return result
	}

	now := time.Now().UTC()
	if now.Before(r.WindowStart) || now.After(r.WindowEnd) {
		result.Reason = fmt.Sprintf("current time %s is outside change window %s – %s",
			now.Format(time.RFC3339), result.WindowStart, result.WindowEnd)
		return result
	}

	if namespace != "" && r.TargetNamespace != "" && r.TargetNamespace != namespace {
		result.Reason = fmt.Sprintf("RFC targets namespace %q, pod is in %q", r.TargetNamespace, namespace)
		return result
	}

	result.Approved = true
	return result
}
