package flyapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: baseURL,
		Token:   token,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Machine represents the subset of Fly Machine fields we care about.
// Unknown fields are ignored.
type Machine struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	State      string         `json:"state"`
	Region     string         `json:"region"`
	InstanceID string         `json:"instance_id"`
	PrivateIP  string         `json:"private_ip"`
	CreatedAt  string         `json:"created_at"`
	UpdatedAt  string         `json:"updated_at"`
	ImageRef   ImageRef       `json:"image_ref"`
	Config     MachineConfig  `json:"config"`
	Events     []MachineEvent `json:"events"`
	Checks     []CheckStatus  `json:"checks"`
}

type ImageRef struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Digest     string `json:"digest"`
	Labels     map[string]string `json:"labels"`
}

func (i ImageRef) String() string {
	if i.Repository == "" {
		return ""
	}
	s := i.Repository
	if i.Tag != "" {
		s += ":" + i.Tag
	}
	if i.Digest != "" {
		s += "@" + i.Digest
	}
	return s
}

type MachineConfig struct {
	Image string `json:"image"`
}

type MachineEvent struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Status    string          `json:"status"`
	Source    string          `json:"source"`
	Timestamp int64           `json:"timestamp"`
	Request   json.RawMessage `json:"request,omitempty"`
}

func (e MachineEvent) Time() time.Time {
	return time.UnixMilli(e.Timestamp)
}

type CheckStatus struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Output    string `json:"output"`
	UpdatedAt string `json:"updated_at"`
}

func (c *Client) ListMachines(ctx context.Context, app string) ([]Machine, error) {
	url := fmt.Sprintf("%s/v1/apps/%s/machines", c.BaseURL, app)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("app %q not found", app)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("list machines for %q: status %d: %s", app, resp.StatusCode, string(body))
	}

	var machines []Machine
	if err := json.NewDecoder(resp.Body).Decode(&machines); err != nil {
		return nil, fmt.Errorf("decode machines: %w", err)
	}
	return machines, nil
}
