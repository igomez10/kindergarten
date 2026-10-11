// Package dns publishes instance hostnames. The only implementation is
// Cloudflare; records carry an ownership comment and records without it are
// never modified or deleted.
package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Provider manages A records for instance hostnames.
type Provider interface {
	// Ensure makes name resolve to ip, owned by owner.
	Ensure(ctx context.Context, name, ip, owner string) error
	// Lookup returns the record's address, or "" if there is none.
	Lookup(ctx context.Context, name string) (ip string, err error)
	// Delete removes the record if owner owns it. A missing record is success.
	Delete(ctx context.Context, name, owner string) error
}

// ErrNotOwned means a record exists under the name but belongs to someone else.
var ErrNotOwned = errors.New("dns record exists and is not owned by this instance")

type Cloudflare struct {
	Token string
	Zone  string // zone name, e.g. example.com
	HTTP  *http.Client

	mu     sync.Mutex
	zoneID string
}

func NewCloudflare(token, zone string) *Cloudflare {
	return &Cloudflare{Token: token, Zone: zone, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

func ownerComment(owner string) string { return "kindergarten:" + owner }

type cfRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment"`
}

func (c *Cloudflare) call(ctx context.Context, method, path string, body any, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.cloudflare.com/client/v4"+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare %s: %w", method, err)
	}
	defer res.Body.Close()
	var env struct {
		Success bool            `json:"success"`
		Errors  []any           `json:"errors"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		return fmt.Errorf("cloudflare %s: status %d: %w", method, res.StatusCode, err)
	}
	if !env.Success {
		return fmt.Errorf("cloudflare %s: status %d: %v", method, res.StatusCode, env.Errors)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func (c *Cloudflare) zone(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.zoneID != "" {
		return c.zoneID, nil
	}
	var zones []struct {
		ID string `json:"id"`
	}
	if err := c.call(ctx, "GET", "/zones?name="+url.QueryEscape(c.Zone), nil, &zones); err != nil {
		return "", err
	}
	if len(zones) != 1 {
		return "", fmt.Errorf("cloudflare zone %q not found", c.Zone)
	}
	c.zoneID = zones[0].ID
	return c.zoneID, nil
}

func (c *Cloudflare) find(ctx context.Context, name string) (zone string, rec *cfRecord, err error) {
	zone, err = c.zone(ctx)
	if err != nil {
		return "", nil, err
	}
	var recs []cfRecord
	if err := c.call(ctx, "GET", fmt.Sprintf("/zones/%s/dns_records?type=A&name=%s", zone, url.QueryEscape(name)), nil, &recs); err != nil {
		return "", nil, err
	}
	if len(recs) == 0 {
		return zone, nil, nil
	}
	return zone, &recs[0], nil
}

func (c *Cloudflare) Ensure(ctx context.Context, name, ip, owner string) error {
	zone, rec, err := c.find(ctx, name)
	if err != nil {
		return err
	}
	want := cfRecord{Type: "A", Name: name, Content: ip, TTL: 60, Proxied: false, Comment: ownerComment(owner)}
	if rec == nil {
		return c.call(ctx, "POST", fmt.Sprintf("/zones/%s/dns_records", zone), want, nil)
	}
	if rec.Comment != want.Comment {
		return ErrNotOwned
	}
	if rec.Content == ip {
		return nil
	}
	return c.call(ctx, "PUT", fmt.Sprintf("/zones/%s/dns_records/%s", zone, rec.ID), want, nil)
}

func (c *Cloudflare) Lookup(ctx context.Context, name string) (string, error) {
	_, rec, err := c.find(ctx, name)
	if err != nil || rec == nil {
		return "", err
	}
	return rec.Content, nil
}

func (c *Cloudflare) Delete(ctx context.Context, name, owner string) error {
	zone, rec, err := c.find(ctx, name)
	if err != nil || rec == nil {
		return err
	}
	if rec.Comment != ownerComment(owner) {
		return ErrNotOwned
	}
	return c.call(ctx, "DELETE", fmt.Sprintf("/zones/%s/dns_records/%s", zone, rec.ID), nil, nil)
}
