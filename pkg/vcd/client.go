/*
Copyright AppsCode Inc. and Contributors

Licensed under the AppsCode Free Trial License 1.0.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://github.com/appscode/licenses/raw/1.0.0/AppsCode-Free-Trial-1.0.0.md

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package vcd is a minimal OpenAPI client for VMware Cloud Director, scoped
// to the load-balancer and edge-gateway NAT endpoints needed by the GC.
package vcd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	apiVersion = "39.0" // VCD 10.5; works on 10.4 too via OpenAPI
	tokenTTL   = 25 * time.Minute
)

type Config struct {
	Endpoint string // e.g. https://vcd-ng.cloud.example.com
	Org      string // tenant org name, e.g. "dbaas"
	User     string // tenant user
	Password string
	Insecure bool
}

type Client struct {
	cfg  Config
	http *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

func New(cfg Config) *Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.Insecure},
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Transport: tr, Timeout: 60 * time.Second},
	}
}

// login fetches a bearer token. VCD returns it in the X-VMWARE-VCLOUD-ACCESS-TOKEN
// response header on POST /cloudapi/1.0.0/sessions with Basic auth.
func (c *Client) login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExp) {
		return nil
	}
	u := strings.TrimRight(c.cfg.Endpoint, "/") + "/cloudapi/1.0.0/sessions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.cfg.User+"@"+c.cfg.Org, c.cfg.Password)
	req.Header.Set("Accept", "application/json;version="+apiVersion)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("vcd login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vcd login: status %d: %s", resp.StatusCode, string(body))
	}
	tok := resp.Header.Get("X-VMWARE-VCLOUD-ACCESS-TOKEN")
	if tok == "" {
		return errors.New("vcd login: empty access token in response")
	}
	c.token = tok
	c.tokenExp = time.Now().Add(tokenTTL)
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	if err := c.login(ctx); err != nil {
		return err
	}
	u := strings.TrimRight(c.cfg.Endpoint, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json;version="+apiVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		// token expired between login and now — drop and retry once.
		c.mu.Lock()
		c.token = ""
		c.mu.Unlock()
		if err := c.login(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		tok = c.token
		c.mu.Unlock()
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err = c.http.Do(req)
		if err != nil {
			return fmt.Errorf("%s %s (retry): %w", method, path, err)
		}
		defer resp.Body.Close()
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, string(b))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---- typed wrappers ----

type pageResult[T any] struct {
	ResultTotal int `json:"resultTotal"`
	PageCount   int `json:"pageCount"`
	Page        int `json:"page"`
	PageSize    int `json:"pageSize"`
	Values      []T `json:"values"`
}

type NamedRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type VirtualService struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	GatewayRef          NamedRef `json:"gatewayRef"`
	LoadBalancerPoolRef NamedRef `json:"loadBalancerPoolRef"`
	VirtualIPAddress    string   `json:"virtualIpAddress"`
}

type Pool struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	GatewayRef NamedRef `json:"gatewayRef"`
}

type NATRule struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	RuleType          string `json:"ruleType"`
	ExternalAddresses string `json:"externalAddresses"`
	InternalAddresses string `json:"internalAddresses"`
	DnatExternalPort  string `json:"dnatExternalPort,omitempty"`
}

// ListVirtualServices returns every virtual service on the given edge gateway
// whose name starts with prefix. The flat /loadBalancer/virtualServices
// collection is not listable (GET returns 405); listing must go through the
// gateway-scoped summaries endpoint. Uses FIQL filter on name to keep the
// result small.
func (c *Client) ListVirtualServices(ctx context.Context, edgeGatewayID, namePrefix string) ([]VirtualService, error) {
	path := fmt.Sprintf("/cloudapi/1.0.0/edgeGateways/%s/loadBalancer/virtualServiceSummaries", url.PathEscape(edgeGatewayID))
	return paged[VirtualService](ctx, c, path, namePrefix)
}

func (c *Client) ListPools(ctx context.Context, edgeGatewayID, namePrefix string) ([]Pool, error) {
	path := fmt.Sprintf("/cloudapi/1.0.0/edgeGateways/%s/loadBalancer/poolSummaries", url.PathEscape(edgeGatewayID))
	return paged[Pool](ctx, c, path, namePrefix)
}

// ListNATRules lists NAT rules on a specific edge gateway. The NAT endpoint
// rejects server-side FIQL filtering on name, so we pull the full set and
// filter by prefix client-side.
func (c *Client) ListNATRules(ctx context.Context, edgeGatewayID, namePrefix string) ([]NATRule, error) {
	path := fmt.Sprintf("/cloudapi/1.0.0/edgeGateways/%s/nat/rules", url.PathEscape(edgeGatewayID))
	all, err := paged[NATRule](ctx, c, path, "")
	if err != nil {
		return nil, err
	}
	if namePrefix == "" {
		return all, nil
	}
	out := all[:0]
	for _, r := range all {
		if strings.HasPrefix(r.Name, namePrefix) {
			out = append(out, r)
		}
	}
	return out, nil
}

func paged[T any](ctx context.Context, c *Client, path, namePrefix string) ([]T, error) {
	var all []T
	page := 1
	for {
		q := url.Values{}
		q.Set("page", fmt.Sprint(page))
		q.Set("pageSize", "128")
		if namePrefix != "" {
			// FIQL: name starts with prefix
			q.Set("filter", fmt.Sprintf("name==%s*", namePrefix))
		}
		var pr pageResult[T]
		if err := c.do(ctx, http.MethodGet, path, q, nil, &pr); err != nil {
			return nil, err
		}
		all = append(all, pr.Values...)
		if pr.PageCount == 0 || page >= pr.PageCount {
			return all, nil
		}
		page++
	}
}

func (c *Client) DeleteVirtualService(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete,
		"/cloudapi/1.0.0/loadBalancer/virtualServices/"+url.PathEscape(id),
		nil, nil, nil)
}

func (c *Client) DeletePool(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete,
		"/cloudapi/1.0.0/loadBalancer/pools/"+url.PathEscape(id),
		nil, nil, nil)
}

func (c *Client) DeleteNATRule(ctx context.Context, edgeGatewayID, ruleID string) error {
	return c.do(ctx, http.MethodDelete,
		fmt.Sprintf("/cloudapi/1.0.0/edgeGateways/%s/nat/rules/%s",
			url.PathEscape(edgeGatewayID), url.PathEscape(ruleID)),
		nil, nil, nil)
}
