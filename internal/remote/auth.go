// Package remote provides auth functionality for Kailab servers.
package remote

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Credentials stores authentication tokens.
type Credentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Email        string `json:"email,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	ServerURL    string `json:"server_url,omitempty"`
}

// CredentialsPath returns the path to the credentials file.
func CredentialsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kai", "credentials.json")
}

// LoadCredentials loads stored credentials.
func LoadCredentials() (*Credentials, error) {
	path := CredentialsPath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading credentials: %w", err)
	}

	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("parsing credentials: %w", err)
	}
	return &creds, nil
}

// SaveCredentials saves credentials.
func SaveCredentials(creds *Credentials) error {
	path := CredentialsPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling credentials: %w", err)
	}

	// Restrict permissions to owner only
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing credentials: %w", err)
	}
	return nil
}

// ClearCredentials removes stored credentials.
func ClearCredentials() error {
	path := CredentialsPath()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing credentials: %w", err)
	}
	return nil
}

// AuthClient handles authentication with kailab-control.
type AuthClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewAuthClient creates a new auth client.
func NewAuthClient(baseURL string) *AuthClient {
	return &AuthClient{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// MagicLinkResponse is the response from sending a magic link.
type MagicLinkResponse struct {
	Message  string `json:"message"`
	DevToken string `json:"dev_token,omitempty"`
}

// TokenResponse is the response from exchanging a token.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// MeResponse is the response from /api/v1/me.
type MeResponse struct {
	ID       string      `json:"id"`
	Email    string      `json:"email"`
	Username string      `json:"username,omitempty"`
	Name     string      `json:"name,omitempty"`
	Orgs     []OrgBrief  `json:"orgs,omitempty"`
	CreatedAt string     `json:"created_at"`
}

// OrgBrief is a summary of an org in the me response.
type OrgBrief struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	Role string `json:"role,omitempty"`
}

// SendMagicLink requests a magic link email.
// If fromCLI is true, passes ?source=cli to skip signup gating.
func (c *AuthClient) SendMagicLink(email string) (*MagicLinkResponse, error) {
	return c.SendMagicLinkWithSource(email, "")
}

// SendMagicLinkWithSource requests a magic link email with an optional source param.
func (c *AuthClient) SendMagicLinkWithSource(email, source string) (*MagicLinkResponse, error) {
	body, _ := json.Marshal(map[string]string{"email": email})
	url := c.BaseURL + "/api/v1/auth/magic-link"
	if source != "" {
		url += "?source=" + source
	}
	resp, err := c.HTTPClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result MagicLinkResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

// ExchangeToken exchanges a magic link token for access/refresh tokens.
func (c *AuthClient) ExchangeToken(magicToken string) (*TokenResponse, error) {
	body, _ := json.Marshal(map[string]string{"magic_token": magicToken})
	resp, err := c.HTTPClient.Post(c.BaseURL+"/api/v1/auth/token", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

// RefreshAccessToken refreshes the access token using the refresh token.
func (c *AuthClient) RefreshAccessToken(refreshToken string) (*TokenResponse, error) {
	body, _ := json.Marshal(map[string]string{"refresh_token": refreshToken})
	resp, err := c.HTTPClient.Post(c.BaseURL+"/api/v1/auth/refresh", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

// GetMe retrieves the current user info.
func (c *AuthClient) GetMe(accessToken string) (*MeResponse, error) {
	req, _ := http.NewRequest("GET", c.BaseURL+"/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result MeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

// DailyUsageResponse is the LLM-rate-limit snapshot returned by
// `GET /api/v1/usage/daily`. Cents (not dollars) so the same
// integer arithmetic flows from kailab's stored counter through
// the wire format and into the CLI display.
type DailyUsageResponse struct {
	DailyCostCents int `json:"daily_cost_cents"`
	DailyCapCents  int `json:"daily_cap_cents"`
}

// GetDailyUsage fetches the user's per-day cost ceiling and
// today's accumulated cost from kailab. Used by `kai auth
// status` and `kai auth login` so the user sees the cap before
// they hit it. Returns an error wrapped from the standard
// transport layer; callers should treat any failure as
// "skip the daily-cap line in the output" rather than aborting
// auth status entirely.
func (c *AuthClient) GetDailyUsage(accessToken string) (*DailyUsageResponse, error) {
	req, _ := http.NewRequest("GET", c.BaseURL+"/api/v1/usage/daily", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var result DailyUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

// CreateOrgResponse is the response from creating an org.
type CreateOrgResponse struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// CreateOrg creates an org.
func (c *AuthClient) CreateOrg(accessToken, slug, name string) (*CreateOrgResponse, error) {
	body, _ := json.Marshal(map[string]string{"slug": slug, "name": name})
	req, _ := http.NewRequest("POST", c.BaseURL+"/api/v1/orgs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return nil, c.parseError(resp)
	}

	var result CreateOrgResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

// CreateRepoResponse is the response from creating a repo.
type CreateRepoResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CreateRepo creates a repo in an org.
func (c *AuthClient) CreateRepo(accessToken, orgSlug, repoName string) (*CreateRepoResponse, error) {
	body, _ := json.Marshal(map[string]string{"name": repoName, "visibility": "private"})
	req, _ := http.NewRequest("POST", c.BaseURL+"/api/v1/orgs/"+orgSlug+"/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return nil, c.parseError(resp)
	}

	var result CreateRepoResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

func (c *AuthClient) parseError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var errResp struct {
		Error   string `json:"error"`
		Details string `json:"details,omitempty"`
	}
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error != "" {
		return fmt.Errorf("%s", errResp.Error)
	}
	return fmt.Errorf("server error: %d %s", resp.StatusCode, string(body))
}

// Login performs an interactive login flow.
func Login(serverURL string) error {
	client := NewAuthClient(serverURL)

	// Prompt for email
	fmt.Print("Email: ")
	reader := bufio.NewReader(os.Stdin)
	email, _ := reader.ReadString('\n')
	email = strings.TrimSpace(email)

	if email == "" {
		return fmt.Errorf("email required")
	}

	// Send magic link (source=cli skips signup gating for instant access)
	fmt.Printf("Sending login link to %s...\n", email)
	result, err := client.SendMagicLinkWithSource(email, "cli")
	if err != nil {
		return fmt.Errorf("sending magic link: %w", err)
	}

	var token string

	// In dev mode, the token might be returned directly
	if result.DevToken != "" {
		fmt.Println("Dev mode: Token received directly")
		token = result.DevToken
	} else {
		fmt.Println("Check your email for a login link (from support@kaicontext.com).")
		fmt.Print("Copy the token from the email and paste it here: ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		// Handle if user pasted a URL
		if strings.Contains(input, "token=") {
			parts := strings.Split(input, "token=")
			if len(parts) > 1 {
				token = strings.Split(parts[1], "&")[0]
			}
		} else {
			token = input
		}
	}

	if token == "" {
		return fmt.Errorf("token required")
	}

	// Exchange token
	fmt.Println("Logging in...")
	tokens, err := client.ExchangeToken(token)
	if err != nil {
		return fmt.Errorf("exchanging token: %w", err)
	}

	// Save credentials
	creds := &Credentials{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		Email:        email,
		ExpiresAt:    time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second).Unix(),
		ServerURL:    serverURL,
	}
	if err := SaveCredentials(creds); err != nil {
		return fmt.Errorf("saving credentials: %w", err)
	}

	fmt.Printf("Logged in as %s\n", email)
	return nil
}

// Logout clears stored credentials.
func Logout() error {
	return ClearCredentials()
}

// GetValidAccessToken returns a valid access token, refreshing if needed.
func GetValidAccessToken() (string, error) {
	creds, err := LoadCredentials()
	if err != nil {
		return "", err
	}
	if creds == nil {
		return "", fmt.Errorf("not logged in (run 'kai auth login')")
	}

	// Check if token is expired or about to expire (within 60 seconds)
	if creds.ExpiresAt > 0 && time.Now().Unix() > creds.ExpiresAt-60 {
		if creds.RefreshToken == "" || creds.ServerURL == "" {
			return "", fmt.Errorf("token expired and no refresh token available (run 'kai auth login')")
		}

		// Try to refresh, retry once on transient failure (e.g. deploy in progress)
		client := NewAuthClient(creds.ServerURL)
		tokens, err := client.RefreshAccessToken(creds.RefreshToken)
		if err != nil {
			// Retry once after a short delay (server may be restarting)
			time.Sleep(2 * time.Second)
			tokens, err = client.RefreshAccessToken(creds.RefreshToken)
		}
		if err != nil {
			return "", fmt.Errorf("token expired and refresh failed: %w\nRun 'kai auth login' to re-authenticate", err)
		}

		creds.AccessToken = tokens.AccessToken
		if tokens.RefreshToken != "" {
			creds.RefreshToken = tokens.RefreshToken
		}
		creds.ExpiresAt = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second).Unix()
		SaveCredentials(creds)
	}

	return creds.AccessToken, nil
}

// GetAuthStatus returns the current auth status.
func GetAuthStatus() (email string, serverURL string, loggedIn bool) {
	creds, err := LoadCredentials()
	if err != nil || creds == nil {
		return "", "", false
	}
	return creds.Email, creds.ServerURL, true
}
