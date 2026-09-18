//go:build integration

package testsupport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// KeycloakImage matches the one Compose runs.
const KeycloakImage = "quay.io/keycloak/keycloak:26.4"

// Realm, and the clients keycloak/realm.json provisions.
const (
	KeycloakRealm = "wagering"
	APIAudience   = "wagering-api"

	InternalClient  = "internal-service"
	ProviderAClient = "provider-a"
	ProviderBClient = "provider-b"
	// ExpiringClient issues a token that lives one second.
	ExpiringClient = "provider-expiring"
	// OutsiderClient issues a valid token for another audience.
	OutsiderClient = "outsider"
)

// ClientSecret is the convention the realm follows; no real secret is involved.
func ClientSecret(clientID string) string { return clientID + "-secret" }

// Keycloak starts once per test binary and is left to Ryuk: the import takes
// long enough that a container per test would dominate the suite, and the realm
// is read-only.
var keycloak = sync.OnceValues(startKeycloak)

func KeycloakIssuer(t *testing.T) string {
	t.Helper()

	issuer, err := keycloak()
	if err != nil {
		t.Fatalf("start keycloak container: %v", err)
	}
	return issuer
}

func startKeycloak() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	_, thisFile, _, _ := runtime.Caller(0)
	realm := filepath.Join(filepath.Dir(thisFile), "..", "..", "keycloak", "realm.json")

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Image: KeycloakImage,
			Cmd:   []string{"start-dev", "--import-realm"},
			Env: map[string]string{
				"KC_BOOTSTRAP_ADMIN_USERNAME": "admin",
				"KC_BOOTSTRAP_ADMIN_PASSWORD": "admin",
			},
			Files: []testcontainers.ContainerFile{{
				HostFilePath:      realm,
				ContainerFilePath: "/opt/keycloak/data/import/realm.json",
				FileMode:          0o644,
			}},
			ExposedPorts: []string{"8080/tcp"},
			// The realm endpoint, not the process: the import must have finished.
			WaitingFor: wait.ForHTTP("/realms/" + KeycloakRealm + "/.well-known/openid-configuration").
				WithPort("8080/tcp").
				WithStartupTimeout(4 * time.Minute),
		},
	})
	if err != nil {
		return "", err
	}

	endpoint, err := container.PortEndpoint(ctx, "8080/tcp", "http")
	if err != nil {
		return "", err
	}
	// No KC_HOSTNAME here: Keycloak derives the issuer from the request, and the
	// tests reach it under this one address.
	return endpoint + "/realms/" + KeycloakRealm, nil
}

// Token runs the client_credentials grant the brief prescribes.
func Token(t *testing.T, issuer, clientID string) string {
	t.Helper()

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {ClientSecret(clientID)},
	}

	resp, err := http.Post(issuer+"/protocol/openid-connect/token",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("token request for %s: %v", clientID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token request for %s: status %d", clientID, resp.StatusCode)
	}

	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode token response for %s: %v", clientID, err)
	}
	if body.AccessToken == "" {
		t.Fatalf("token response for %s carried no access_token", clientID)
	}
	return body.AccessToken
}

// KeycloakEnv points the process under test at the shared container and returns
// its issuer.
func KeycloakEnv(t *testing.T) string {
	t.Helper()

	issuer := KeycloakIssuer(t)
	t.Setenv("OIDC_ISSUER_URL", issuer)
	t.Setenv("OIDC_AUDIENCE", APIAudience)
	return issuer
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// BearerClient signs every request it makes as clientID.
func BearerClient(t *testing.T, issuer, clientID string) *http.Client {
	t.Helper()
	return &http.Client{Transport: bearer{Token(t, issuer, clientID)}}
}
