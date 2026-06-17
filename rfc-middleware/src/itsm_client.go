package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type ChangeRecord struct {
	Title           string
	State           string
	Approver        string
	WindowStart     time.Time
	WindowEnd       time.Time
	TargetNamespace string
}

type ITSMClient struct {
	baseURL    string
	httpClient *http.Client
}

func newITSMClient(baseURL, certPath, keyPath string) (*ITSMClient, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load mTLS keypair: %w", err)
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}

	return &ITSMClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Second,
		},
	}, nil
}

func (c *ITSMClient) FetchChangeRecord(ctx context.Context, rfcID string) (*ChangeRecord, error) {
	url := fmt.Sprintf("%s/api/now/table/change_request/%s", c.baseURL, rfcID)

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("RFC %q not found", rfcID)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("ITSM returned status %d", resp.StatusCode)
		}

		var payload struct {
			Result struct {
				ShortDescription string `json:"short_description"`
				State            string `json:"state"`
				ApprovedBy       struct {
					DisplayValue string `json:"display_value"`
				} `json:"approved_by"`
				StartDate       string `json:"start_date"`
				EndDate         string `json:"end_date"`
				CmdbCi          struct {
					DisplayValue string `json:"display_value"`
				} `json:"cmdb_ci"`
			} `json:"result"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			return nil, fmt.Errorf("decode ITSM response: %w", err)
		}

		r := payload.Result
		start, _ := time.Parse("2006-01-02 15:04:05", r.StartDate)
		end, _ := time.Parse("2006-01-02 15:04:05", r.EndDate)

		return &ChangeRecord{
			Title:           r.ShortDescription,
			State:           r.State,
			Approver:        r.ApprovedBy.DisplayValue,
			WindowStart:     start.UTC(),
			WindowEnd:       end.UTC(),
			TargetNamespace: r.CmdbCi.DisplayValue,
		}, nil
	}

	return nil, fmt.Errorf("ITSM unreachable after retries: %w", lastErr)
}
