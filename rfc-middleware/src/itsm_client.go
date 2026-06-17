package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
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

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

type ITSMClient struct {
	baseURL      string
	tokenURL     string
	clientID     string // Okta app client ID  — Basic auth username on the token endpoint
	clientSecret string // Okta app client secret — Basic auth password on the token endpoint
	username     string // ITSM service-account username — goes in the form body
	password     string // ITSM service-account password — goes in the form body
	scope        string // OAuth2 scope, e.g. "openid"
	httpClient   *http.Client

	mu          sync.Mutex
	cachedToken string
	tokenExpiry time.Time
}

func newITSMClient(baseURL, tokenURL, clientID, clientSecret, username, password, scope string) (*ITSMClient, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("ITSM_BASE_URL is required")
	}
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("ITSM_CLIENT_ID and ITSM_CLIENT_SECRET are required")
	}
	if username == "" || password == "" {
		return nil, fmt.Errorf("ITSM_USERNAME and ITSM_PASSWORD are required")
	}
	if tokenURL == "" {
		tokenURL = baseURL + "/oauth_token.do"
	}
	if scope == "" {
		scope = "openid"
	}
	return &ITSMClient{
		baseURL:      baseURL,
		tokenURL:     tokenURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		username:     username,
		password:     password,
		scope:        scope,
		httpClient:   &http.Client{Timeout: 5 * time.Second},
	}, nil
}

// getToken returns a valid bearer token, fetching a new one when the cached
// token is absent or within 30 seconds of expiry.
//
// Equivalent curl:
//
//	curl -X POST "<tokenURL>" \
//	  -H "Content-Type: application/x-www-form-urlencoded" \
//	  -H "Authorization: Basic base64($CLIENT_ID:$CLIENT_SECRET)" \
//	  -d "grant_type=password&username=<u>&password=<p>&scope=openid"
func (c *ITSMClient) getToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cachedToken != "" && time.Now().Before(c.tokenExpiry.Add(-30*time.Second)) {
		return c.cachedToken, nil
	}

	form := url.Values{
		"grant_type": {"password"},
		"username":   {c.username},
		"password":   {c.password},
		"scope":      {c.scope},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("token endpoint returned status %d: %s", resp.StatusCode, body)
	}

	var tok tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("token endpoint returned empty access_token")
	}

	ttl := 3600 // fallback when expires_in is absent
	if tok.ExpiresIn > 0 {
		ttl = tok.ExpiresIn
	}
	c.cachedToken = tok.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(ttl) * time.Second)
	return c.cachedToken, nil
}

// invalidateToken clears the in-memory token so the next getToken call fetches
// a fresh one from the token endpoint.
func (c *ITSMClient) invalidateToken() {
	c.mu.Lock()
	c.cachedToken = ""
	c.mu.Unlock()
}

// FetchChangeRecord calls GET {baseURL}/api/now/table/change_request/{rfcID}
// with a Bearer token. On a 401 it invalidates the cached token and retries once.
func (c *ITSMClient) FetchChangeRecord(ctx context.Context, rfcID string) (*ChangeRecord, error) {
	endpoint := fmt.Sprintf("%s/api/now/table/change_request/%s", c.baseURL, rfcID)

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
			}
			c.invalidateToken()
		}

		token, err := c.getToken(ctx)
		if err != nil {
			lastErr = fmt.Errorf("obtain token: %w", err)
			continue
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		record, err := c.parseChangeRecord(resp, rfcID)
		resp.Body.Close()
		if err != nil {
			if resp.StatusCode == http.StatusUnauthorized {
				lastErr = err
				continue
			}
			return nil, err
		}
		return record, nil
	}

	return nil, fmt.Errorf("ITSM unreachable after retries: %w", lastErr)
}

func (c *ITSMClient) parseChangeRecord(resp *http.Response, rfcID string) (*ChangeRecord, error) {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("token rejected by ITSM (401)")
	case http.StatusNotFound:
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
			StartDate string `json:"start_date"`
			EndDate   string `json:"end_date"`
			CmdbCi    struct {
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
