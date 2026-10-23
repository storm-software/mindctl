package home

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/accounts"
)

func TestConnectDelegatesNamedSelection(t *testing.T) {
	certPEM, keyPEM, caPEM := testCertificate(t)
	var selected bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/bootstrap":
			_ = json.NewEncoder(w).Encode(map[string]string{"cert": certPEM, "key": keyPEM, "ca": caPEM})
		case "/v1/accounts/select":
			selected = true
			_ = json.NewEncoder(w).Encode(accounts.Selection{Account: accounts.Account{ID: "home-account", Provider: "openai", Enabled: true}})
		default:
			http.NotFound(w, r)
		}
	}))
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	server.StartTLS()
	defer server.Close()
	payload, _ := json.Marshal(map[string]string{"endpoint": server.URL})
	token := "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	selector, err := Connect(context.Background(), token, true, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	selection, err := selector.Select(context.Background(), accounts.SelectRequest{ModelID: "gpt-test", Provider: "openai"})
	if err != nil || !selected || selection.Account.ID != "home-account" {
		t.Fatalf("selection=%+v selected=%v err=%v", selection, selected, err)
	}
}

func testCertificate(t *testing.T) (string, string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "home"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM, certPEM
}
