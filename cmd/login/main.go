// Command login is the daily human step: it trades the Kite request token for an
// access token and writes the session to NATS KV (for the ticker and jobs) and to
// the token file (for the backup recorder). It prints the user and expiry only;
// the token itself is never printed or logged.
//
//	login                       # prints the login URL, then reads the redirect URL from stdin
//	login --listen 127.0.0.1:8765   # waits for the redirect on a localhost callback
//	login --request-token XYZ
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"pmt/internal/app"
	"pmt/internal/bus"
	"pmt/internal/kite"
)

func main() {
	reqTok := flag.String("request-token", "", "request_token from the login redirect")
	listen := flag.String("listen", "", "localhost address to receive the redirect on, e.g. 127.0.0.1:8765 (the Kite app's redirect URL must point here)")
	tokenFile := flag.String("token-file", "", "token file to write (default backuprec.token_file)")
	noKV := flag.Bool("no-kv", false, "write the token file only")
	flag.Parse()

	ctx, cancel, cfg := app.Start("login")
	defer cancel()
	if cfg.Kite.APIKey == "" || cfg.Kite.APISecret == "" {
		app.Fatal("login", errors.New("KITE_API_KEY and KITE_API_SECRET must be set in the environment"))
	}
	path := *tokenFile
	if path == "" {
		path = cfg.Abs(cfg.BackupRec.TokenFile)
	}
	if path == "" {
		app.Fatal("login", errors.New("no token file: set PMT_TOKEN_FILE or pass --token-file"))
	}

	rt := *reqTok
	var err error
	if rt == "" {
		loginURL := "https://kite.zerodha.com/connect/login?v=3&api_key=" + url.QueryEscape(cfg.Kite.APIKey)
		fmt.Fprintln(os.Stderr, "Open this URL, log in with 2FA, and come back:")
		fmt.Fprintln(os.Stderr, "  "+loginURL)
		if *listen != "" {
			rt, err = callback(ctx, *listen)
		} else {
			fmt.Fprint(os.Stderr, "Paste the redirect URL (or the request_token): ")
			rt, err = readStdin()
		}
		if err != nil {
			app.Fatal("login", err)
		}
	}

	rest := kite.NewREST(cfg.Kite, cfg.Kite.APIKey, "")
	ectx, ecancel := context.WithTimeout(ctx, 30*time.Second)
	s, err := rest.Exchange(ectx, rt, cfg.Kite.APISecret)
	ecancel()
	if err != nil {
		app.Fatal("token exchange", errors.New(kite.Describe(err)))
	}

	if err := kite.WriteTokenFile(path, s); err != nil {
		app.Fatal("token file", err)
	}
	if !*noKV {
		if err := saveKV(ctx, cfg.NATSURL, s); err != nil {
			app.Fatal("nats kv", err)
		}
	}
	fmt.Println("logged in:", s.String())
}

func saveKV(ctx context.Context, natsURL string, s bus.Session) error {
	c, err := bus.Connect(natsURL, "pmt-login")
	if err != nil {
		return err
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := c.Ensure(ctx); err != nil {
		return err
	}
	kv, err := c.KV(ctx, bus.KVSession)
	if err != nil {
		return err
	}
	return kite.SaveSession(ctx, kv, s)
}

// extract accepts a full redirect URL, a query string, or a bare token.
func extract(in string) (string, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return "", errors.New("nothing pasted")
	}
	if strings.Contains(in, "request_token=") {
		q := in
		if i := strings.IndexByte(in, '?'); i >= 0 {
			q = in[i+1:]
		}
		v, err := url.ParseQuery(q)
		if err != nil {
			return "", err
		}
		if st := v.Get("status"); st != "" && st != "success" {
			return "", fmt.Errorf("login status %q", st)
		}
		if t := v.Get("request_token"); t != "" {
			return t, nil
		}
		return "", errors.New("request_token is empty")
	}
	if strings.ContainsAny(in, " /?&=") {
		return "", errors.New("not a redirect URL or request token")
	}
	return in, nil
}

func readStdin() (string, error) {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return extract(line)
}

// callback serves one redirect on a loopback address and returns its token.
func callback(ctx context.Context, addr string) (string, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", fmt.Errorf("--listen must be a loopback address, got %s", host)
	}
	got := make(chan string, 1)
	srv := &http.Server{Addr: addr, ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, err := extract("?" + r.URL.RawQuery)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "Logged in. You can close this tab.")
		select {
		case got <- t:
		default:
		}
	})}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	go srv.Serve(ln)
	defer srv.Close()
	fmt.Fprintln(os.Stderr, "waiting for the redirect on http://"+addr+" ...")
	select {
	case t := <-got:
		return t, nil
	case <-time.After(10 * time.Minute):
		return "", errors.New("no redirect within 10 minutes")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
