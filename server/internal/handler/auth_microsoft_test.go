package handler

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
)

const (
	msTestClient = "ms-client"
	msTestSecret = "ms-secret"
)

type msFixture struct {
	t      *testing.T
	tenant string
	kid    string
	key    *rsa.PrivateKey
}

func newMSFixture(t *testing.T) *msFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &msFixture{t: t, tenant: "tenant-" + strings.ReplaceAll(strings.ToLower(t.Name()), "/", "-"), kid: "kid-" + t.Name(), key: key}
	t.Setenv("MICROSOFT_CLIENT_ID", msTestClient)
	t.Setenv("MICROSOFT_CLIENT_SECRET", msTestSecret)
	t.Setenv("MICROSOFT_TENANT_ID", f.tenant)
	return f
}

func (f *msFixture) claims(mut func(jwt.MapClaims)) jwt.MapClaims {
	c := jwt.MapClaims{
		"iss":   "https://login.microsoftonline.com/" + f.tenant + "/v2.0",
		"tid":   f.tenant,
		"aud":   msTestClient,
		"exp":   time.Now().Add(time.Hour).Unix(),
		"email": "  Ada.Entra@Example.com ",
		"name":  "Ada Entra",
	}
	if mut != nil {
		mut(c)
	}
	return c
}

func (f *msFixture) sign(c jwt.MapClaims, signer *rsa.PrivateKey) string {
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = f.kid
	s, err := tok.SignedString(signer)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *msFixture) call(tokenStatus int, tokenBody string) *testutil.Response {
	f.t.Helper()
	pub := f.key.PublicKey
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": f.kid,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
	h := *testHandler
	h.cfg = Config{AllowSignup: true}
	h.googleOAuthHTTPClient = &http.Client{Transport: googleRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/"+f.tenant+"/oauth2/v2.0/token":
			req.ParseForm()
			if req.PostForm.Get("code") != "test-code" || req.PostForm.Get("client_secret") != msTestSecret ||
				req.PostForm.Get("grant_type") != "authorization_code" || req.PostForm.Get("scope") != "openid profile email" {
				f.t.Errorf("unexpected token form: %v", req.PostForm)
			}
			return googleResponse(req, tokenStatus, tokenBody), nil
		case req.URL.Path == "/"+f.tenant+"/discovery/v2.0/keys":
			return googleResponse(req, http.StatusOK, string(jwks)), nil
		}
		f.t.Fatalf("unexpected request: %s", req.URL)
		return nil, nil
	})}
	req := httptest.NewRequest(http.MethodPost, "/auth/microsoft", strings.NewReader(`{"code":"test-code","redirect_uri":"http://localhost/cb"}`))
	return testutil.Call(f.t, h.MicrosoftLogin, req)
}

func idTokenBody(tok string) string { return fmt.Sprintf(`{"id_token":%q}`, tok) }

func TestMicrosoftLogin(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		f := newMSFixture(t)
		dbfx.Cleanup(t, `DELETE FROM "user" WHERE email = $1`, "ada.entra@example.com")
		var got LoginResponse
		resp := f.call(200, idTokenBody(f.sign(f.claims(nil), f.key))).Want(200)
		resp.JSON(&got)
		if got.Token == "" || got.User.Email != "ada.entra@example.com" || got.User.Name != "Ada Entra" {
			t.Fatalf("unexpected login response: %+v", got)
		}
		if len(resp.Result().Cookies()) == 0 {
			t.Fatal("expected auth cookies")
		}
	})

	t.Run("preferred_username fallback", func(t *testing.T) {
		f := newMSFixture(t)
		dbfx.Cleanup(t, `DELETE FROM "user" WHERE email = $1`, "upn.user@example.com")
		c := f.claims(func(c jwt.MapClaims) {
			delete(c, "email")
			c["preferred_username"] = "UPN.User@example.com"
		})
		var got LoginResponse
		f.call(200, idTokenBody(f.sign(c, f.key))).Want(200).JSON(&got)
		if got.User.Email != "upn.user@example.com" {
			t.Fatalf("email = %q", got.User.Email)
		}
	})

	t.Run("missing email", func(t *testing.T) {
		f := newMSFixture(t)
		c := f.claims(func(c jwt.MapClaims) {
			delete(c, "email")
			c["preferred_username"] = "no-at-sign"
		})
		f.call(200, idTokenBody(f.sign(c, f.key))).Want(400)
	})

	rejected := map[string]func(*msFixture) string{
		"wrong tid": func(f *msFixture) string {
			return f.sign(f.claims(func(c jwt.MapClaims) { c["tid"] = "other-tenant" }), f.key)
		},
		"wrong aud": func(f *msFixture) string {
			return f.sign(f.claims(func(c jwt.MapClaims) { c["aud"] = "someone-else" }), f.key)
		},
		"bad signature": func(f *msFixture) string {
			other, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatal(err)
			}
			return f.sign(f.claims(nil), other)
		},
	}
	for name, mk := range rejected {
		t.Run(name, func(t *testing.T) {
			f := newMSFixture(t)
			f.call(200, idTokenBody(mk(f))).Want(401)
		})
	}

	t.Run("invalid_grant", func(t *testing.T) {
		f := newMSFixture(t)
		var got struct{ Code, Error string }
		f.call(400, `{"error":"invalid_grant"}`).Want(400).JSON(&got)
		if got.Code != "" || !strings.Contains(got.Error, "Microsoft") {
			t.Fatalf("got code=%q error=%q", got.Code, got.Error)
		}
	})

	t.Run("not configured", func(t *testing.T) {
		f := newMSFixture(t)
		t.Setenv("MICROSOFT_TENANT_ID", "")
		var got struct{ Code string }
		f.call(200, "{}").Want(http.StatusForbidden).JSON(&got)
		if got.Code != "microsoft_login_not_configured" {
			t.Fatalf("code = %q", got.Code)
		}
	})
}
