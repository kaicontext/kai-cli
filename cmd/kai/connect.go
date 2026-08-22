package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/remote"
	"github.com/spf13/cobra"
)

// kai connect — link external services (Gmail first) to the signed-in
// kailab account. The OAuth dance is brokered server-side (the server
// holds the Composio key; this command never sees a provider token):
// we ask the server to start a connection, open the consent URL in the
// user's browser, then sit polling the connectors list until the
// server observes the grant complete. The same endpoints back the
// desktop's Connectors UI — this is just the terminal door to them.

var connectDisconnect bool

var connectCmd = &cobra.Command{
	Use:   "connect [provider]",
	Short: "Connect external services (gmail) to your Kai account",
	Long: `Link an external service so Kai's chat and robot can use it as data.

With no provider, lists every connector and its status.

Examples:
  kai connect                      # list connectors
  kai connect gmail                # link Gmail (opens browser consent)
  kai connect gmail --disconnect   # revoke the link`,
	Args: cobra.MaximumNArgs(1),
	RunE: runConnect,
}

func init() {
	connectCmd.Flags().BoolVar(&connectDisconnect, "disconnect", false, "revoke the provider's link instead of creating one")
	rootCmd.AddCommand(connectCmd)
}

// connectPollInterval / connectPollBudget pace the wait for the human
// to finish the consent screen. Five minutes is generous for reading a
// Google consent page; past it we leave the link pending with
// instructions rather than hanging the terminal forever.
const (
	connectPollInterval = 3 * time.Second
	connectPollBudget   = 5 * time.Minute
)

func runConnect(cmd *cobra.Command, args []string) error {
	base := os.Getenv("KAI_SERVER")
	if base == "" {
		if creds, _ := remote.LoadCredentials(); creds != nil && creds.ServerURL != "" {
			base = creds.ServerURL
		} else {
			base = remote.DefaultServer
		}
	}
	token, err := remote.GetValidAccessToken()
	if err != nil || token == "" {
		return fmt.Errorf("not logged in — run `kai auth login` first")
	}

	if len(args) == 0 {
		return connectList(base, token)
	}
	provider := strings.ToLower(args[0])
	if connectDisconnect {
		if _, err := connectorAPI(base, token, http.MethodDelete, "/api/v1/connectors/"+provider, nil); err != nil {
			return err
		}
		fmt.Printf("✓ %s disconnected\n", provider)
		return nil
	}

	resp, err := connectorAPI(base, token, http.MethodPost, "/api/v1/connectors/"+provider+"/connect", map[string]any{})
	if err != nil {
		return err
	}
	consentURL, _ := resp["url"].(string)
	if consentURL == "" {
		return fmt.Errorf("server started the connection but returned no consent URL")
	}

	fmt.Printf("Opening browser to authorize %s…\n", provider)
	fmt.Printf("If it doesn't open, visit:\n  %s\n\n", consentURL)
	openBrowser(consentURL)

	fmt.Print("Waiting for authorization")
	deadline := time.Now().Add(connectPollBudget)
	for time.Now().Before(deadline) {
		time.Sleep(connectPollInterval)
		fmt.Print(".")
		status, email := connectorStatus(base, token, provider)
		switch status {
		case "active":
			fmt.Println()
			if email != "" {
				fmt.Printf("✓ %s connected (%s)\n", provider, email)
			} else {
				fmt.Printf("✓ %s connected\n", provider)
			}
			return nil
		case "error":
			fmt.Println()
			return fmt.Errorf("%s connection failed — run `kai connect %s` to retry", provider, provider)
		}
	}
	fmt.Println()
	fmt.Printf("Still pending. Finish the consent screen, then check with `kai connect`.\n")
	return nil
}

func connectList(base, token string) error {
	resp, err := connectorAPI(base, token, http.MethodGet, "/api/v1/connectors", nil)
	if err != nil {
		return err
	}
	rows, _ := resp["connectors"].([]any)
	if len(rows) == 0 {
		fmt.Println("No connectors available on this server.")
		return nil
	}
	for _, it := range rows {
		m, _ := it.(map[string]any)
		provider, _ := m["provider"].(string)
		status, _ := m["status"].(string)
		email, _ := m["email"].(string)
		line := fmt.Sprintf("  %-10s %s", provider, status)
		if email != "" {
			line += "  (" + email + ")"
		}
		fmt.Println(line)
	}
	return nil
}

// connectorStatus returns ("", "") on any polling error — transient
// network noise during the wait loop must not abort the flow.
func connectorStatus(base, token, provider string) (status, email string) {
	resp, err := connectorAPI(base, token, http.MethodGet, "/api/v1/connectors", nil)
	if err != nil {
		return "", ""
	}
	rows, _ := resp["connectors"].([]any)
	for _, it := range rows {
		m, _ := it.(map[string]any)
		if p, _ := m["provider"].(string); p == provider {
			status, _ = m["status"].(string)
			email, _ = m["email"].(string)
			return status, email
		}
	}
	return "", ""
}

func connectorAPI(base, token, method, path string, body any) (map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, strings.TrimRight(base, "/")+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if msg, _ := out["error"].(string); msg != "" {
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, fmt.Errorf("server returned %d", resp.StatusCode)
	}
	return out, nil
}
