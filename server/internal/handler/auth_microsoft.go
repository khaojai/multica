package handler

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/logger"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	microsoftLoginBase   = "https://login.microsoftonline.com/"
	microsoftJWKSTTL     = time.Hour
	microsoftTokenLeeway = time.Minute
)

type microsoftTokenResponse struct {
	IDToken string `json:"id_token"`
}

type microsoftClaims struct {
	jwt.RegisteredClaims
	TenantID          string `json:"tid"`
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`
}

var errMicrosoftJWKSFetch = errors.New("microsoft jwks fetch failed")

type microsoftJWKS struct {
	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

var microsoftKeyCache = struct {
	mu       sync.Mutex
	byTenant map[string]*microsoftJWKS
}{byTenant: map[string]*microsoftJWKS{}}

func microsoftKeysFor(tenant string) *microsoftJWKS {
	microsoftKeyCache.mu.Lock()
	defer microsoftKeyCache.mu.Unlock()
	c := microsoftKeyCache.byTenant[tenant]
	if c == nil {
		c = &microsoftJWKS{}
		microsoftKeyCache.byTenant[tenant] = c
	}
	return c
}

func (h *Handler) fetchMicrosoftJWKS(ctx context.Context, tenant string) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, microsoftLoginBase+url.PathEscape(tenant)+"/discovery/v2.0/keys", nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.googleHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		exp := 0
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
	}
	return keys, nil
}

func (h *Handler) microsoftSigningKey(ctx context.Context, tenant, kid string) (*rsa.PublicKey, error) {
	c := microsoftKeysFor(tenant)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keys != nil && time.Since(c.fetchedAt) < microsoftJWKSTTL {
		if key, ok := c.keys[kid]; ok {
			return key, nil
		}
	}
	keys, err := h.fetchMicrosoftJWKS(ctx, tenant)
	if err != nil {
		slog.Error("microsoft jwks fetch failed", "error", err)
		return nil, errMicrosoftJWKSFetch
	}
	c.keys, c.fetchedAt = keys, time.Now()
	key, ok := keys[kid]
	if !ok {
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	return key, nil
}

func (h *Handler) parseMicrosoftIDToken(ctx context.Context, idToken, tenant, clientID string) (*microsoftClaims, error) {
	claims := &microsoftClaims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(microsoftLoginBase+tenant+"/v2.0"),
		jwt.WithAudience(clientID),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(microsoftTokenLeeway),
	)
	_, err := parser.ParseWithClaims(idToken, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("id_token has no kid")
		}
		return h.microsoftSigningKey(ctx, tenant, kid)
	})
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(claims.TenantID, tenant) {
		return nil, errors.New("id_token tid does not match configured tenant")
	}
	return claims, nil
}

func (h *Handler) MicrosoftLogin(w http.ResponseWriter, r *http.Request) {
	var req GoogleLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}

	clientID := os.Getenv("MICROSOFT_CLIENT_ID")
	clientSecret := os.Getenv("MICROSOFT_CLIENT_SECRET")
	tenant := strings.TrimSpace(os.Getenv("MICROSOFT_TENANT_ID"))
	if clientID == "" || clientSecret == "" || tenant == "" {
		writeFeatureDisabled(w, "microsoft_login_not_configured", "Microsoft login is not configured")
		return
	}
	switch strings.ToLower(tenant) {
	case "common", "organizations", "consumers":
		slog.Error("microsoft login requires a single-tenant MICROSOFT_TENANT_ID", "tenant", tenant)
		writeFeatureDisabled(w, "microsoft_login_not_configured", "Microsoft login is not configured")
		return
	}

	redirectURI := req.RedirectURI
	if redirectURI == "" {
		redirectURI = os.Getenv("MICROSOFT_REDIRECT_URI")
	}

	tokenResp, err := h.googleHTTPClient().PostForm(microsoftLoginBase+url.PathEscape(tenant)+"/oauth2/v2.0/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {req.Code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
		"scope":         {"openid profile email"},
	})
	if err != nil {
		slog.Error("microsoft oauth token exchange failed", "error", err)
		writeError(w, http.StatusBadGateway, "failed to exchange code with Microsoft")
		return
	}
	defer tokenResp.Body.Close()

	tokenBody, err := io.ReadAll(tokenResp.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to read Microsoft token response")
		return
	}
	if tokenResp.StatusCode != http.StatusOK {
		slog.Error("microsoft oauth token exchange returned error", "status", tokenResp.StatusCode, "body", string(tokenBody))
		var tokenErr struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(tokenBody, &tokenErr); err == nil &&
			tokenResp.StatusCode == http.StatusBadRequest && tokenErr.Error == "invalid_grant" {
			writeError(w, http.StatusBadRequest, "The Microsoft sign-in code is invalid or expired. Sign in again.")
			return
		}
		writeError(w, http.StatusBadGateway, "failed to exchange code with Microsoft")
		return
	}

	var mToken microsoftTokenResponse
	if err := json.Unmarshal(tokenBody, &mToken); err != nil {
		writeError(w, http.StatusBadGateway, "failed to parse Microsoft token response")
		return
	}
	if strings.TrimSpace(mToken.IDToken) == "" {
		slog.Error("microsoft oauth token response has no id_token")
		writeError(w, http.StatusBadGateway, "invalid Microsoft token response")
		return
	}

	claims, err := h.parseMicrosoftIDToken(r.Context(), mToken.IDToken, tenant, clientID)
	if err != nil {
		if errors.Is(err, errMicrosoftJWKSFetch) {
			writeError(w, http.StatusBadGateway, "failed to fetch Microsoft signing keys")
			return
		}
		slog.Warn("microsoft id_token rejected", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusUnauthorized, "invalid Microsoft id_token")
		return
	}

	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if email == "" && strings.Contains(claims.PreferredUsername, "@") {
		email = strings.ToLower(strings.TrimSpace(claims.PreferredUsername))
	}
	if email == "" {
		writeError(w, http.StatusBadRequest, "Microsoft did not provide an email address for this sign-in")
		return
	}

	if auth.IsTemporarilyDisabledUserEmail(email) {
		writeErrorCode(w, http.StatusForbidden, googleLoginCodeAccountDisabled, auth.TemporarilyDisabledUserError)
		return
	}

	user, isNew, err := h.findOrCreateUser(r.Context(), email)
	if err != nil {
		if writeGoogleLoginActionableError(w, err) {
			return
		}
		slog.Warn("login user lookup or creation failed", append(logger.RequestAttrs(r), "error", err, "email", email)...)
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}
	if isNew {
		evt := analytics.Signup(uuidToString(user.ID), user.Email, signupSourceFromRequest(r))
		evt.Properties["auth_method"] = "microsoft"
		obsmetrics.RecordEvent(h.Analytics, h.Metrics, evt)
	}

	if claims.Name != "" && user.Name == strings.Split(email, "@")[0] {
		updated, err := h.Queries.UpdateUser(r.Context(), db.UpdateUserParams{
			ID:        user.ID,
			Name:      claims.Name,
			AvatarUrl: user.AvatarUrl,
		})
		if err == nil {
			user = updated
		}
	}

	tokenString, err := h.issueJWT(user)
	if err != nil {
		if writeGoogleLoginActionableError(w, err) {
			return
		}
		slog.Warn("microsoft login failed", append(logger.RequestAttrs(r), "error", err, "email", email)...)
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	if err := auth.SetAuthCookies(w, tokenString); err != nil {
		slog.Warn("failed to set auth cookies", "error", err)
	}
	if h.CFSigner != nil {
		for _, cookie := range h.CFSigner.SignedCookies(time.Now().Add(72 * time.Hour)) {
			http.SetCookie(w, cookie)
		}
	}

	slog.Info("user logged in via microsoft", append(logger.RequestAttrs(r), "user_id", uuidToString(user.ID), "email", user.Email)...)
	writeJSON(w, http.StatusOK, LoginResponse{
		Token: tokenString,
		User:  h.userToResponse(user),
	})
}
