// Package tailscale provides a client for the Tailscale API.
package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	baseURL = "https://api.tailscale.com/api/v2"
)

// Client is a Tailscale API client.
type Client struct {
	httpClient *http.Client
	apiKey     string
	tailnet    string
	logger     *slog.Logger

	// OAuth credentials
	clientID     string
	clientSecret string
	oauthToken   string
	tokenExpiry  time.Time
	tokenMu      sync.Mutex
}

// NewClient creates a new Tailscale API client.
// If apiKey is empty, it reads from TAILSCALE_API_KEY environment variable.
// If tailnet is empty, it uses "-" to mean the default tailnet for the API key.
func NewClient(apiKey, tailnet string, logger *slog.Logger) (*Client, error) {
	if apiKey == "" {
		apiKey = os.Getenv("TAILSCALE_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("TAILSCALE_API_KEY not set")
	}
	if tailnet == "" {
		tailnet = "-"
	}
	if logger == nil {
		logger = slog.Default()
	}

	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		apiKey:     apiKey,
		tailnet:    tailnet,
		logger:     logger,
	}, nil
}

// NewClientWithOAuth creates a new Tailscale API client using OAuth credentials.
// clientID and clientSecret are the OAuth client credentials.
// If empty, reads from TAILSCALE_CLIENT_ID and TAILSCALE_CLIENT_SECRET env vars.
func NewClientWithOAuth(clientID, clientSecret, tailnet string, logger *slog.Logger) (*Client, error) {
	if clientID == "" {
		clientID = os.Getenv("TAILSCALE_CLIENT_ID")
	}
	if clientSecret == "" {
		clientSecret = os.Getenv("TAILSCALE_CLIENT_SECRET")
	}
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("TAILSCALE_CLIENT_ID and TAILSCALE_CLIENT_SECRET must be set for OAuth")
	}
	if tailnet == "" {
		tailnet = "-"
	}
	if logger == nil {
		logger = slog.Default()
	}

	return &Client{
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		clientID:     clientID,
		clientSecret: clientSecret,
		tailnet:      tailnet,
		logger:       logger,
	}, nil
}

// getOAuthToken gets or refreshes the OAuth token
func (c *Client) getOAuthToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	// Return cached token if still valid (with 60s buffer)
	if c.oauthToken != "" && time.Now().Add(60*time.Second).Before(c.tokenExpiry) {
		return c.oauthToken, nil
	}

	// Request new token
	data := url.Values{}
	data.Set("grant_type", "client_credentials")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.tailscale.com/api/v2/oauth/token",
		strings.NewReader(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("create token request: %w", err)
	}

	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("token request failed %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}

	c.oauthToken = tokenResp.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)

	return c.oauthToken, nil
}

// Device represents a Tailscale device.
type Device struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Hostname           string   `json:"hostname"`
	Addresses          []string `json:"addresses"`
	TailscaleIP        string   `json:"-"` // Populated from Addresses[0]
	Tags               []string `json:"tags"`
	AdvertisedRoutes   []string `json:"advertisedRoutes"`
	EnabledRoutes      []string `json:"enabledRoutes"`
	IsExternalDevice   bool     `json:"isExternal"`
	LastSeen           string   `json:"lastSeen"`
	OS                 string   `json:"os"`
	ClientVersion      string   `json:"clientVersion"`
	MachineKey         string   `json:"machineKey"`
	NodeKey            string   `json:"nodeKey"`
	TailnetLockKey     string   `json:"tailnetLockKey"`
	BlocksIncomingConn bool     `json:"blocksIncomingConnections"`
}

// DeviceRoutes represents the routes for a device.
type DeviceRoutes struct {
	AdvertisedRoutes []string `json:"advertisedRoutes"`
	EnabledRoutes    []string `json:"enabledRoutes"`
}

// AuthKey represents a Tailscale auth key.
type AuthKey struct {
	ID           string    `json:"id"`
	Key          string    `json:"key"`
	Created      time.Time `json:"created"`
	Expires      time.Time `json:"expires"`
	Capabilities struct {
		Devices struct {
			Create struct {
				Reusable      bool     `json:"reusable"`
				Ephemeral     bool     `json:"ephemeral"`
				Preauthorized bool     `json:"preauthorized"`
				Tags          []string `json:"tags"`
			} `json:"create"`
		} `json:"devices"`
	} `json:"capabilities"`
}

// CreateAuthKeyRequest is the request body for creating an auth key.
type CreateAuthKeyRequest struct {
	Capabilities struct {
		Devices struct {
			Create struct {
				Reusable      bool     `json:"reusable"`
				Ephemeral     bool     `json:"ephemeral"`
				Preauthorized bool     `json:"preauthorized"`
				Tags          []string `json:"tags"`
			} `json:"create"`
		} `json:"devices"`
	} `json:"capabilities"`
	ExpirySeconds int    `json:"expirySeconds,omitempty"`
	Description   string `json:"description,omitempty"`
}

// ACLPolicy represents a Tailscale ACL policy.
type ACLPolicy struct {
	TagOwners     map[string][]string            `json:"tagOwners,omitempty"`
	AutoApprovers map[string]map[string][]string `json:"autoApprovers,omitempty"`
	Grants        []ACLGrant                     `json:"grants,omitempty"`
	SSH           []SSHRule                      `json:"ssh,omitempty"`
}

// ACLGrant represents a grant in the ACL.
type ACLGrant struct {
	Src []string `json:"src"`
	Dst []string `json:"dst"`
	IP  []string `json:"ip,omitempty"`
}

// SSHRule represents an SSH rule in the ACL.
type SSHRule struct {
	Action string   `json:"action"`
	Src    []string `json:"src"`
	Dst    []string `json:"dst"`
	Users  []string `json:"users"`
}

// doRequest performs an HTTP request with authentication.
func (c *Client) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	reqURL := baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, reqURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Use OAuth token if available, otherwise use API key
	if c.clientID != "" && c.clientSecret != "" {
		token, err := c.getOAuthToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("get OAuth token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// ListDevices lists all devices in the tailnet.
func (c *Client) ListDevices(ctx context.Context) ([]*Device, error) {
	path := fmt.Sprintf("/tailnet/%s/devices", c.tailnet)
	data, err := c.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	var result struct {
		Devices []*Device `json:"devices"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("unmarshal devices: %w", err)
	}

	// Populate TailscaleIP from Addresses
	for _, d := range result.Devices {
		if len(d.Addresses) > 0 {
			d.TailscaleIP = d.Addresses[0]
		}
	}

	return result.Devices, nil
}

// GetDevice gets a device by ID.
func (c *Client) GetDevice(ctx context.Context, deviceID string) (*Device, error) {
	path := fmt.Sprintf("/device/%s", deviceID)
	data, err := c.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	var device Device
	if err := json.Unmarshal(data, &device); err != nil {
		return nil, fmt.Errorf("unmarshal device: %w", err)
	}

	return &device, nil
}

// FindDeviceByHostname finds a device by its hostname.
func (c *Client) FindDeviceByHostname(ctx context.Context, hostname string) (*Device, error) {
	devices, err := c.ListDevices(ctx)
	if err != nil {
		return nil, err
	}

	hostname = strings.ToLower(hostname)
	for _, d := range devices {
		if strings.ToLower(d.Hostname) == hostname || strings.ToLower(d.Name) == hostname {
			return d, nil
		}
		// Also check if the hostname matches the start of the FQDN
		if strings.HasPrefix(strings.ToLower(d.Name), hostname+".") {
			return d, nil
		}
	}

	return nil, fmt.Errorf("device not found: %s", hostname)
}

// GetDeviceRoutes gets the routes for a device.
func (c *Client) GetDeviceRoutes(ctx context.Context, deviceID string) (*DeviceRoutes, error) {
	path := fmt.Sprintf("/device/%s/routes", deviceID)
	data, err := c.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	var routes DeviceRoutes
	if err := json.Unmarshal(data, &routes); err != nil {
		return nil, fmt.Errorf("unmarshal routes: %w", err)
	}

	return &routes, nil
}

// SetDeviceRoutes sets which advertised routes are enabled for a device.
// The routes parameter should contain all routes that should be enabled.
func (c *Client) SetDeviceRoutes(ctx context.Context, deviceID string, routes []string) error {
	path := fmt.Sprintf("/device/%s/routes", deviceID)
	body := map[string][]string{"routes": routes}
	_, err := c.doRequest(ctx, http.MethodPost, path, body)
	return err
}

// EnableRoutes enables the specified routes for a device.
// This is an alias for SetDeviceRoutes for clarity.
func (c *Client) EnableRoutes(ctx context.Context, deviceID string, routes []string) error {
	return c.SetDeviceRoutes(ctx, deviceID, routes)
}

// EnableAllRoutes enables all advertised routes for a device.
func (c *Client) EnableAllRoutes(ctx context.Context, deviceID string) error {
	routes, err := c.GetDeviceRoutes(ctx, deviceID)
	if err != nil {
		return fmt.Errorf("get routes: %w", err)
	}

	if len(routes.AdvertisedRoutes) == 0 {
		c.logger.Info("no routes to enable", "deviceID", deviceID)
		return nil
	}

	c.logger.Info("enabling all routes", "deviceID", deviceID, "routes", routes.AdvertisedRoutes)
	return c.SetDeviceRoutes(ctx, deviceID, routes.AdvertisedRoutes)
}

// CreateAuthKey creates a new auth key.
func (c *Client) CreateAuthKey(ctx context.Context, req CreateAuthKeyRequest) (*AuthKey, error) {
	path := fmt.Sprintf("/tailnet/%s/keys", c.tailnet)
	data, err := c.doRequest(ctx, http.MethodPost, path, req)
	if err != nil {
		return nil, err
	}

	var key AuthKey
	if err := json.Unmarshal(data, &key); err != nil {
		return nil, fmt.Errorf("unmarshal auth key: %w", err)
	}

	return &key, nil
}

// CreateTaggedAuthKey creates a reusable, preauthorized auth key with tags.
// This is a convenience wrapper around CreateAuthKey.
func (c *Client) CreateTaggedAuthKey(ctx context.Context, tags []string, expirySeconds int, description string) (*AuthKey, error) {
	req := CreateAuthKeyRequest{
		ExpirySeconds: expirySeconds,
		Description:   description,
	}
	req.Capabilities.Devices.Create.Reusable = true
	req.Capabilities.Devices.Create.Preauthorized = true
	req.Capabilities.Devices.Create.Tags = tags

	return c.CreateAuthKey(ctx, req)
}

// GetACL gets the current ACL policy.
func (c *Client) GetACL(ctx context.Context) (*ACLPolicy, error) {
	path := fmt.Sprintf("/tailnet/%s/acl", c.tailnet)
	data, err := c.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	var policy ACLPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		return nil, fmt.Errorf("unmarshal ACL: %w", err)
	}

	return &policy, nil
}

// SetACL sets the ACL policy. This replaces the entire policy.
func (c *Client) SetACL(ctx context.Context, policy *ACLPolicy) error {
	path := fmt.Sprintf("/tailnet/%s/acl", c.tailnet)
	_, err := c.doRequest(ctx, http.MethodPost, path, policy)
	return err
}

// EnsureRouterSetup ensures a router is properly configured in Tailscale.
// It finds the device, enables all its advertised routes, and returns the device info.
func (c *Client) EnsureRouterSetup(ctx context.Context, hostname string) (*Device, error) {
	log := c.logger.With("operation", "EnsureRouterSetup", "hostname", hostname)

	// Find the device
	log.Info("finding device")
	device, err := c.FindDeviceByHostname(ctx, hostname)
	if err != nil {
		return nil, fmt.Errorf("find device: %w", err)
	}
	log.Info("found device", "deviceID", device.ID, "name", device.Name)

	// Enable all advertised routes
	log.Info("enabling routes")
	if err := c.EnableAllRoutes(ctx, device.ID); err != nil {
		return nil, fmt.Errorf("enable routes: %w", err)
	}

	// Get updated device info
	device, err = c.GetDevice(ctx, device.ID)
	if err != nil {
		return nil, fmt.Errorf("get updated device: %w", err)
	}

	log.Info("router setup complete",
		"advertisedRoutes", device.AdvertisedRoutes,
		"enabledRoutes", device.EnabledRoutes)

	return device, nil
}

// EnsureAutoApprovers ensures the ACL has autoApprovers for the specified routes and tags.
// This modifies the existing ACL to add/update autoApprovers.
func (c *Client) EnsureAutoApprovers(ctx context.Context, routeCIDRs []string, approverTags []string) error {
	log := c.logger.With("operation", "EnsureAutoApprovers")

	// Get current ACL
	log.Info("getting current ACL")
	policy, err := c.GetACL(ctx)
	if err != nil {
		return fmt.Errorf("get ACL: %w", err)
	}

	// Ensure autoApprovers structure exists
	if policy.AutoApprovers == nil {
		policy.AutoApprovers = make(map[string]map[string][]string)
	}
	if policy.AutoApprovers["routes"] == nil {
		policy.AutoApprovers["routes"] = make(map[string][]string)
	}

	// Add autoApprovers for each route CIDR
	changed := false
	for _, cidr := range routeCIDRs {
		existing := policy.AutoApprovers["routes"][cidr]
		if !stringSliceEqual(existing, approverTags) {
			policy.AutoApprovers["routes"][cidr] = approverTags
			changed = true
			log.Info("added autoApprover", "cidr", cidr, "tags", approverTags)
		}
	}

	if !changed {
		log.Info("autoApprovers already configured")
		return nil
	}

	// Update ACL
	log.Info("updating ACL with autoApprovers")
	if err := c.SetACL(ctx, policy); err != nil {
		return fmt.Errorf("set ACL: %w", err)
	}

	log.Info("autoApprovers configured")
	return nil
}

// stringSliceEqual checks if two string slices are equal.
func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i, v := range a {
		if v != b[i] {
			return false
		}
	}
	return true
}
