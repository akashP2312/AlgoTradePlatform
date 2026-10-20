package kite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"pmt/internal/config"
)

// DefaultRoot is Kite Connect's REST base URL.
const DefaultRoot = "https://api.kite.trade"

// REST is the guarded REST client. Use Kite for gokiteconnect calls; never call
// order or logout methods on it (the guard refuses them anyway).
type REST struct {
	Kite   *kiteconnect.Client
	HTTP   *http.Client
	Guard  *Guard
	root   string
	apiKey string
	token  string
}

// NewREST builds a client on the guarded transport. accessToken may be empty for
// the login exchange.
func NewREST(k config.Kite, apiKey, accessToken string) *REST {
	root := strings.TrimRight(k.RootURL, "/")
	if root == "" {
		root = DefaultRoot
	}
	g := NewGuard(nil, root, Limits{Quote: k.QuotePerSec, Historical: k.HistoricalPerSec, Other: k.OtherPerSec})
	hc := &http.Client{Transport: g, Timeout: 30 * time.Second}
	kc := kiteconnect.New(apiKey)
	kc.SetHTTPClient(hc)
	kc.SetBaseURI(root)
	kc.SetAppName("pmt")
	if accessToken != "" {
		kc.SetAccessToken(accessToken)
	}
	return &REST{Kite: kc, HTTP: hc, Guard: g, root: root, apiKey: apiKey, token: accessToken}
}

// SetAccessToken swaps in a new token after the morning login.
func (r *REST) SetAccessToken(tok string) {
	r.token = tok
	r.Kite.SetAccessToken(tok)
}

// get performs a raw authenticated GET, for responses gokiteconnect parses in ways
// we do not want (the instrument CSV, which we archive byte for byte).
func (r *REST) get(ctx context.Context, p string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.root+p, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Kite-Version", "3")
	req.Header.Set("User-Agent", "gokiteconnect/4 pmt")
	req.Header.Set("Authorization", "token "+r.apiKey+":"+r.token)
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, apiError(resp.StatusCode, body)
	}
	return body, nil
}

// apiError mirrors gokiteconnect's error envelope so IsTokenError works on raw calls.
func apiError(code int, body []byte) error {
	e := kiteconnect.Error{Code: code, ErrorType: kiteconnect.GeneralError, Message: http.StatusText(code)}
	var env struct {
		Message   string `json:"message"`
		ErrorType string `json:"error_type"`
	}
	if json.Unmarshal(body, &env) == nil {
		if env.ErrorType != "" {
			e.ErrorType = env.ErrorType
		}
		if env.Message != "" {
			e.Message = env.Message
		}
	}
	if code == http.StatusForbidden && env.ErrorType == "" {
		e.ErrorType = kiteconnect.TokenError
	}
	return e
}

// IsTokenError reports whether err means the access token is missing, expired or
// invalidated, in which case callers wait for a new token instead of retrying.
func IsTokenError(err error) bool {
	var ke kiteconnect.Error
	if errors.As(err, &ke) {
		return ke.ErrorType == kiteconnect.TokenError
	}
	var kp *kiteconnect.Error
	if errors.As(err, &kp) && kp != nil {
		return kp.ErrorType == kiteconnect.TokenError
	}
	return false
}

// IsThrottled reports a 429 that survived the guard's retries.
func IsThrottled(err error) bool {
	var ke kiteconnect.Error
	return errors.As(err, &ke) && ke.Code == http.StatusTooManyRequests
}

// Describe renders an error without request URLs or headers.
func Describe(err error) string {
	var ke kiteconnect.Error
	if errors.As(err, &ke) {
		return fmt.Sprintf("%s (%d): %s", ke.ErrorType, ke.Code, ke.Message)
	}
	return err.Error()
}
