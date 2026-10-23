// Package home delegates named-model account choice to a Home control plane.
// The classifier is not part of this package.
package home

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/storm-software/mindctl/internal/accounts"
)

// Connect bootstraps mTLS from a Home JWT and returns the account selector.
// disableCluster skips the optional cluster lookup inside the token.
func Connect(ctx context.Context, rawJWT string, disableCluster bool, base *http.Client) (accounts.Selector, error) {
	ticket, err := decodeTicket(rawJWT)
	if err != nil {
		return nil, err
	}
	if base == nil {
		base = http.DefaultClient
	}
	endpoint := ticket.Endpoint
	if ticket.Cluster != "" && !disableCluster {
		discovered, discoverErr := clusterEndpoint(ctx, base, ticket.Cluster, rawJWT)
		if discoverErr != nil {
			return nil, discoverErr
		}
		if discovered != "" {
			endpoint = discovered
		}
	}
	if endpoint == "" {
		return nil, errors.New("home: bootstrap endpoint is missing")
	}
	material, err := bootstrap(ctx, base, endpoint, rawJWT)
	if err != nil {
		return nil, err
	}
	client, err := mtlsClient(base, material)
	if err != nil {
		return nil, err
	}
	return &Selector{endpoint: strings.TrimRight(endpoint, "/"), client: client}, nil
}

type ticket struct {
	Endpoint string `json:"endpoint"`
	Cluster  string `json:"cluster"`
}

type certificateMaterial struct {
	Cert, Key, CA string
}

// Selector asks Home which credential should serve a named model.
type Selector struct {
	endpoint string
	client   *http.Client
}

// Select implements accounts.Selector.
func (s *Selector) Select(ctx context.Context, req accounts.SelectRequest) (accounts.Selection, error) {
	var selection accounts.Selection
	if err := s.post(ctx, "/v1/accounts/select", req, &selection); err != nil {
		return accounts.Selection{}, err
	}
	if selection.Account.ID == "" {
		return accounts.Selection{}, accounts.ErrNone
	}
	return selection, nil
}

// ReportSuccess implements accounts.Selector.
func (s *Selector) ReportSuccess(ctx context.Context, sessionKey, accountID string) {
	_ = s.post(ctx, "/v1/accounts/report", map[string]string{
		"session_key": sessionKey, "account_id": accountID, "result": "success",
	}, nil)
}

// ReportFailure implements accounts.Selector.
func (s *Selector) ReportFailure(ctx context.Context, sessionKey, accountID string, class accounts.FailureClass) {
	_ = s.post(ctx, "/v1/accounts/report", map[string]string{
		"session_key": sessionKey, "account_id": accountID, "result": string(class),
	}, nil)
}

// HasEligible implements accounts.Selector.
func (s *Selector) HasEligible(req accounts.SelectRequest) bool {
	var decoded struct {
		Eligible bool `json:"eligible"`
	}
	if err := s.post(context.Background(), "/v1/accounts/eligible", req, &decoded); err != nil {
		return false
	}
	return decoded.Eligible
}

// ResetCooldown implements accounts.Selector.
func (s *Selector) ResetCooldown(id string) {
	_ = s.post(context.Background(), "/v1/accounts/cooldown/reset", map[string]string{"account_id": id}, nil)
}

func (s *Selector) post(ctx context.Context, path string, body any, dest any) error {
	if s == nil || s.client == nil {
		return errors.New("home: client is not connected")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint+path, strings.NewReader(string(encoded)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("home: %s returned %d", path, response.StatusCode)
	}
	if dest == nil || len(payload) == 0 {
		return nil
	}
	return json.Unmarshal(payload, dest)
}

func decodeTicket(raw string) (ticket, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[1] == "" {
		return ticket{}, errors.New("home: JWT payload is missing")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return ticket{}, errors.New("home: JWT payload is not base64")
	}
	var decoded ticket
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return ticket{}, errors.New("home: JWT payload is not a Home ticket")
	}
	return decoded, nil
}

func clusterEndpoint(ctx context.Context, client *http.Client, cluster, rawJWT string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, cluster, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+rawJWT)
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var decoded struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&decoded); err != nil {
		return "", err
	}
	if response.StatusCode >= 300 || decoded.Endpoint == "" {
		return "", errors.New("home: cluster discovery failed")
	}
	return decoded.Endpoint, nil
}

func bootstrap(ctx context.Context, client *http.Client, endpoint, rawJWT string) (certificateMaterial, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/v1/bootstrap", nil)
	if err != nil {
		return certificateMaterial{}, err
	}
	request.Header.Set("Authorization", "Bearer "+rawJWT)
	response, err := client.Do(request)
	if err != nil {
		return certificateMaterial{}, err
	}
	defer response.Body.Close()
	var decoded struct {
		Cert string `json:"cert"`
		Key  string `json:"key"`
		CA   string `json:"ca"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&decoded); err != nil {
		return certificateMaterial{}, err
	}
	if response.StatusCode >= 300 || decoded.Cert == "" || decoded.Key == "" || decoded.CA == "" {
		return certificateMaterial{}, errors.New("home: bootstrap failed")
	}
	return certificateMaterial{Cert: decoded.Cert, Key: decoded.Key, CA: decoded.CA}, nil
}

func mtlsClient(base *http.Client, material certificateMaterial) (*http.Client, error) {
	cert, err := tls.X509KeyPair([]byte(material.Cert), []byte(material.Key))
	if err != nil {
		return nil, errors.New("home: bootstrap certificate is invalid")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(material.CA)) {
		return nil, errors.New("home: bootstrap CA is invalid")
	}
	var transport *http.Transport
	if base != nil {
		if typed, ok := base.Transport.(*http.Transport); ok && typed != nil {
			transport = typed.Clone()
		}
	}
	if transport == nil {
		if typed, ok := http.DefaultTransport.(*http.Transport); ok {
			transport = typed.Clone()
		} else {
			transport = &http.Transport{}
		}
	}
	transport.TLSClientConfig = &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS12}
	timeout := 30 * time.Second
	if base != nil && base.Timeout > 0 {
		timeout = base.Timeout
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}
