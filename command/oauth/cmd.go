package oauth

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/smallstep/cli/command"
	"github.com/smallstep/cli/crypto/randutil"
	"github.com/smallstep/cli/errs"
	"github.com/smallstep/cli/exec"
	"github.com/smallstep/cli/flags"
	"github.com/smallstep/cli/jose"
	"github.com/smallstep/cli/utils"
	"github.com/urfave/cli"
)

// These are the OAuth2.0 client IDs from the Step CLI. This application is
// using the OAuth2.0 flow for installed applications described on
// https://developers.google.com/identity/protocols/OAuth2InstalledApp
//
// The Step CLI app and these client IDs do not have any APIs or services are
// enabled and it should be only used for OAuth 2.0 authorization.
//
// Due to the fact that the app cannot keep the client_secret confidential,
// incremental authorization with installed apps are not supported by Google.
//
// Google is also distributing the client ID and secret on the cloud SDK
// available here https://cloud.google.com/sdk/docs/quickstarts
const (
	defaultClientID          = "1087160488420-8qt7bavg3qesdhs6it824mhnfgcfe8il.apps.googleusercontent.com"
	defaultClientNotSoSecret = "udTrOT3gzrO7W9fDPgZQLfYJ"

	// The URN for getting verification token offline
	oobCallbackUrn = "urn:ietf:wg:oauth:2.0:oob"
	// The URN for token request grant type jwt-bearer
	jwtBearerUrn = "urn:ietf:params:oauth:grant-type:jwt-bearer"
)

type token struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Err          string `json:"error,omitempty"`
	ErrDesc      string `json:"error_description,omitempty"`
}

func init() {
	cmd := cli.Command{
		Name:  "oauth",
		Usage: "authorization and single sign-on using OAuth & OIDC",
		UsageText: `**step oauth**
[**--provider**=<provider>] [**--client-id**=<client-id> **--client-secret**=<client-secret>]
[**--scope**=<scope> ...] [**--bare** [**--oidc**]] [**--header** [**--oidc**]] [**--prompt**=<prompt>]

**step oauth**
**--authorization-endpoint**=<authorization-endpoint>
**--token-endpoint**=<token-endpoint>
**--client-id**=<client-id> **--client-secret**=<client-secret>
[**--scope**=<scope> ...] [**--bare** [**--oidc**]] [**--header** [**--oidc**]] [**--prompt**=<prompt>]

**step oauth** [**--account**=<account>]
[**--authorization-endpoint**=<authorization-endpoint>]
[**--token-endpoint**=<token-endpoint>]
[**--scope**=<scope> ...] [**--bare** [**--oidc**]] [**--header** [**--oidc**]] [**--prompt**=<prompt>]

**step oauth** **--account**=<account> **--jwt**
[**--scope**=<scope> ...] [**--header**] [**-bare**] [**--prompt**=<prompt>]`,
		Description: `**step oauth** command implements the OAuth 2.0 authorization flow.

OAuth is an open standard for access delegation, commonly used as a way for
Internet users to grant websites or applications access to their information on
other websites but without giving them the passwords. This mechanism is used by
companies such as Amazon, Google, Facebook, Microsoft and Twitter to permit the
users to share information about their accounts with third party applications or
websites. Learn more at https://en.wikipedia.org/wiki/OAuth.

This command by default performs he authorization flow with a preconfigured
Google application, but a custom one can be set combining the flags
**--client-id**, **--client-secret**, and **--provider**. The provider value
must be set to the OIDC discovery document (.well-known/openid-configuration)
endpoint. If Google is used this flag is not necessary, but the appropriate
value would be be https://accounts.google.com or
https://accounts.google.com/.well-known/openid-configuration

## EXAMPLES

Do the OAuth 2.0 flow using the default client:
'''
$ step oauth
'''

Redirect to localhost instead of 127.0.0.1:
'''
$ step oauth --listen localhost:0
'''

Redirect to a fixed port instead of random one:
'''
$ step oauth --listen :10000
'''

Redirect to a fixed url but listen on all the interfaces:
'''
$ step oauth --listen 0.0.0.0:10000 --listen-url http://127.0.0.1:10000
'''

Get just the access token:
'''
$ step oauth --bare
'''

Get just the OIDC token:
'''
$ step oauth --oidc --bare
'''

Use a custom OAuth2.0 server:
'''
$ step oauth --client-id my-client-id --client-secret my-client-secret \
  --provider https://example.org
'''`,
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "provider, idp",
				Usage: "OAuth provider for authentication",
				Value: "google",
			},
			cli.StringFlag{
				Name:  "email, e",
				Usage: "Email to authenticate",
			},
			cli.BoolFlag{
				Name:  "console, c",
				Usage: "Complete the flow while remaining only inside the terminal",
			},
			cli.StringFlag{
				Name:  "client-id",
				Usage: "OAuth Client ID",
			},
			cli.StringFlag{
				Name:  "client-secret",
				Usage: "OAuth Client Secret",
			},
			cli.StringFlag{
				Name:  "account",
				Usage: "JSON file containing account details",
			},
			cli.StringFlag{
				Name:  "authorization-endpoint",
				Usage: "OAuth Authorization Endpoint",
			},
			cli.StringFlag{
				Name:  "token-endpoint",
				Usage: "OAuth Token Endpoint",
			},
			cli.BoolFlag{
				Name:  "header",
				Usage: "Output HTTP Authorization Header (suitable for use with curl)",
			},
			cli.BoolFlag{
				Name:  "oidc",
				Usage: "Output OIDC Token instead of OAuth Access Token",
			},
			cli.BoolFlag{
				Name:  "bare",
				Usage: "Only output the token",
			},
			cli.StringSliceFlag{
				Name:  "scope",
				Usage: "OAuth scopes",
			},
			cli.StringFlag{
				Name: "prompt",
				Usage: `Whether the Authorization Server prompts the End-User for reauthentication and consent.
OpenID standard defines the following values, but your provider may support some or none of them:

    **none**
    :   The Authorization Server MUST NOT display any authentication or consent user interface pages.
        An error is returned if an End-User is not already authenticated or the Client does not have
        pre-configured consent for the requested Claims or does not fulfill other conditions for
        processing the request.

    **login**
    :   The Authorization Server SHOULD prompt the End-User for reauthentication. If it cannot
        reauthenticate the End-User, it MUST return an error, typically login_required.

    **consent**
    :   The Authorization Server SHOULD prompt the End-User for consent before returning information
        to the Client. If it cannot obtain consent, it MUST return an error, typically consent_required.

    **select_account**
    :   The Authorization Server SHOULD prompt the End-User to select a user account. This enables an
        End-User who has multiple accounts at the Authorization Server to select amongst the multiple
        accounts that they might have current sessions for. If it cannot obtain an account selection
        choice made by the End-User, it MUST return an error, typically account_selection_required.
`,
			},
			cli.BoolFlag{
				Name:  "jwt",
				Usage: "Generate a JWT Auth token instead of an OAuth Token (only works with service accounts)",
			},
			cli.StringFlag{
				Name:  "listen",
				Usage: "Callback listener <address> (e.g. \":10000\")",
			},
			cli.StringFlag{
				Name:  "listen-url",
				Usage: "The redirect_uri <url> in the authorize request (e.g. \"http://127.0.0.1:10000\")",
			},
			cli.BoolFlag{
				Name:   "implicit",
				Usage:  "Uses the implicit flow to authenticate the user. Requires **--insecure** and **--client-id** flags.",
				Hidden: true,
			},
			cli.BoolFlag{
				Name:   "insecure",
				Usage:  "Allows the use of insecure flows.",
				Hidden: true,
			},
			cli.StringFlag{
				Name:   "browser",
				Usage:  "Path to browser for OAuth flow (macOS only).",
				Hidden: true,
			},
			flags.RedirectURL,
		},
		Action: oauthCmd,
	}

	command.Register(cmd)
}

func oauthCmd(c *cli.Context) error {
	opts := &options{
		Provider:            c.String("provider"),
		Email:               c.String("email"),
		Console:             c.Bool("console"),
		Implicit:            c.Bool("implicit"),
		CallbackListener:    c.String("listen"),
		CallbackListenerURL: c.String("listen-url"),
		CallbackPath:        "/",
		TerminalRedirect:    c.String("redirect-url"),
		Browser:             c.String("browser"),
	}
	if err := opts.Validate(); err != nil {
		return err
	}
	if (opts.Provider != "google" || c.IsSet("authorization-endpoint")) && !c.IsSet("client-id") {
		return errors.New("flag '--client-id' required with '--provider'")
	}

	var clientID, clientSecret string
	if opts.Implicit {
		if !c.Bool("insecure") {
			return errs.RequiredInsecureFlag(c, "implicit")
		}
		if !c.IsSet("client-id") {
			return errs.RequiredWithFlag(c, "implicit", "client-id")
		}
	} else {
		clientID = defaultClientID
		clientSecret = defaultClientNotSoSecret
	}
	if c.IsSet("client-id") {
		clientID = c.String("client-id")
		clientSecret = c.String("client-secret")
	}

	authzEp := ""
	tokenEp := ""
	if c.IsSet("authorization-endpoint") {
		if !c.IsSet("token-endpoint") {
			return errors.New("flag '--authorization-endpoint' requires flag '--token-endpoint'")
		}
		opts.Provider = ""
		authzEp = c.String("authorization-endpoint")
		tokenEp = c.String("token-endpoint")
	}

	do2lo := false
	issuer := ""
	// This code supports Google service accounts. Probably maybe also support JWKs?
	if c.IsSet("account") {
		opts.Provider = ""
		filename := c.String("account")
		b, err := ioutil.ReadFile(filename)
		if err != nil {
			return errors.Wrapf(err, "error reading account from %s", filename)
		}
		account := make(map[string]interface{})
		if err = json.Unmarshal(b, &account); err != nil {
			return errors.Wrapf(err, "error reading %s: unsupported format", filename)
		}

		if _, ok := account["installed"]; ok {
			details := account["installed"].(map[string]interface{})
			authzEp = details["auth_uri"].(string)
			tokenEp = details["token_uri"].(string)
			clientID = details["client_id"].(string)
			clientSecret = details["client_secret"].(string)
		} else if accountType, ok := account["type"]; ok && accountType == "service_account" {
			authzEp = account["auth_uri"].(string)
			tokenEp = account["token_uri"].(string)
			clientID = account["private_key_id"].(string)
			clientSecret = account["private_key"].(string)
			issuer = account["client_email"].(string)
			do2lo = true
		} else {
			return errors.Wrapf(err, "error reading %s: unsupported account type", filename)
		}
	}

	scope := "openid email"
	if c.IsSet("scope") {
		scope = strings.Join(c.StringSlice("scope"), " ")
	}
	prompt := ""
	if c.IsSet("prompt") {
		prompt = c.String("prompt")
	}

	o, err := newOauth(opts.Provider, clientID, clientSecret, authzEp, tokenEp, scope, prompt, opts)
	if err != nil {
		return err
	}

	var tok *token
	switch {
	case do2lo:
		if c.Bool("jwt") {
			tok, err = o.DoJWTAuthorization(issuer, scope)
		} else {
			tok, err = o.DoTwoLeggedAuthorization(issuer)
		}
	case opts.Console:
		tok, err = o.DoManualAuthorization()
	default:
		tok, err = o.DoLoopbackAuthorization()
	}

	if err != nil {
		return err
	}

	if c.Bool("header") {
		if c.Bool("oidc") {
			fmt.Println("Authorization: Bearer", tok.IDToken)
		} else {
			fmt.Println("Authorization: Bearer", tok.AccessToken)
		}
	} else {
		if c.Bool("bare") {
			if c.Bool("oidc") {
				fmt.Println(tok.IDToken)
			} else {
				fmt.Println(tok.AccessToken)
			}
		} else {
			b, err := json.MarshalIndent(tok, "", "  ")
			if err != nil {
				return errors.Wrapf(err, "error marshaling token data")
			}
			fmt.Println(string(b))
		}
	}

	return nil
}

type options struct {
	Provider            string
	Email               string
	Console             bool
	Implicit            bool
	CallbackListener    string
	CallbackListenerURL string
	CallbackPath        string
	TerminalRedirect    string
	Browser             string
}

// Validate validates the options.
func (o *options) Validate() error {
	if o.Provider != "google" && !strings.HasPrefix(o.Provider, "https://") {
		return errors.New("use a valid provider: google")
	}
	if o.CallbackListener != "" {
		if _, _, err := net.SplitHostPort(o.CallbackListener); err != nil {
			return errors.Wrapf(err, "invalid value '%s' for flag '--listen'", o.CallbackListener)
		}
	}
	if o.CallbackListenerURL != "" {
		u, err := url.Parse(o.CallbackListenerURL)
		if err != nil || u.Scheme == "" {
			return errors.Wrapf(err, "invalid value '%s' for flag '--listen-url'", o.CallbackListenerURL)
		}
		if u.Path != "" {
			o.CallbackPath = u.Path
		}
	}
	return nil
}

type oauth struct {
	provider            string
	clientID            string
	clientSecret        string
	scope               string
	prompt              string
	loginHint           string
	redirectURI         string
	tokenEndpoint       string
	authzEndpoint       string
	userInfoEndpoint    string // For testing
	state               string
	codeChallenge       string
	nonce               string
	implicit            bool
	CallbackListener    string
	CallbackListenerURL string
	CallbackPath        string
	terminalRedirect    string
	browser             string
	errCh               chan error
	tokCh               chan *token
}

func newOauth(provider, clientID, clientSecret, authzEp, tokenEp, scope, prompt string, opts *options) (*oauth, error) {
	state, err := randutil.Alphanumeric(32)
	if err != nil {
		return nil, err
	}

	challenge, err := randutil.Alphanumeric(64)
	if err != nil {
		return nil, err
	}

	nonce, err := randutil.Hex(64) // 256 bits
	if err != nil {
		return nil, err
	}

	switch provider {
	case "google":
		return &oauth{
			provider:            provider,
			clientID:            clientID,
			clientSecret:        clientSecret,
			scope:               scope,
			prompt:              prompt,
			authzEndpoint:       "https://accounts.google.com/o/oauth2/v2/auth",
			tokenEndpoint:       "https://www.googleapis.com/oauth2/v4/token",
			userInfoEndpoint:    "https://www.googleapis.com/oauth2/v3/userinfo",
			loginHint:           opts.Email,
			state:               state,
			codeChallenge:       challenge,
			nonce:               nonce,
			implicit:            opts.Implicit,
			CallbackListener:    opts.CallbackListener,
			CallbackListenerURL: opts.CallbackListenerURL,
			CallbackPath:        opts.CallbackPath,
			terminalRedirect:    opts.TerminalRedirect,
			browser:             opts.Browser,
			errCh:               make(chan error),
			tokCh:               make(chan *token),
		}, nil
	default:
		userinfoEp := ""
		if authzEp == "" && tokenEp == "" {
			d, err := disco(provider)
			if err != nil {
				return nil, err
			}

			if _, ok := d["authorization_endpoint"]; !ok {
				return nil, errors.New("missing 'authorization_endpoint' in provider metadata")
			}
			if _, ok := d["token_endpoint"]; !ok {
				return nil, errors.New("missing 'token_endpoint' in provider metadata")
			}
			authzEp = d["authorization_endpoint"].(string)
			tokenEp = d["token_endpoint"].(string)
			userinfoEp = d["token_endpoint"].(string)
		}
		return &oauth{
			provider:            provider,
			clientID:            clientID,
			clientSecret:        clientSecret,
			scope:               scope,
			prompt:              prompt,
			authzEndpoint:       authzEp,
			tokenEndpoint:       tokenEp,
			userInfoEndpoint:    userinfoEp,
			loginHint:           opts.Email,
			state:               state,
			codeChallenge:       challenge,
			nonce:               nonce,
			implicit:            opts.Implicit,
			CallbackListener:    opts.CallbackListener,
			CallbackListenerURL: opts.CallbackListenerURL,
			CallbackPath:        opts.CallbackPath,
			terminalRedirect:    opts.TerminalRedirect,
			browser:             opts.Browser,
			errCh:               make(chan error),
			tokCh:               make(chan *token),
		}, nil
	}
}

func disco(provider string) (map[string]interface{}, error) {
	u, err := url.Parse(provider)
	if err != nil {
		return nil, err
	}
	// TODO: OIDC and OAuth specify two different ways of constructing this
	// URL. This is the OIDC way. Probably want to try both. See
	// https://tools.ietf.org/html/rfc8414#section-5
	if !strings.Contains(u.Path, "/.well-known/openid-configuration") {
		u.Path = path.Join(u.Path, "/.well-known/openid-configuration")
	}
	resp, err := http.Get(u.String())
	if err != nil {
		return nil, errors.Wrapf(err, "error retrieving %s", u.String())
	}
	defer resp.Body.Close()
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrapf(err, "error retrieving %s", u.String())
	}
	details := make(map[string]interface{})
	if err = json.Unmarshal(b, &details); err != nil {
		return nil, errors.Wrapf(err, "error reading %s: unsupported format", u.String())
	}
	return details, err
}

// NewServer creates http server
func (o *oauth) NewServer() (*httptest.Server, error) {
	if o.CallbackListener == "" {
		return httptest.NewServer(o), nil
	}
	host, port, err := net.SplitHostPort(o.CallbackListener)
	if err != nil {
		return nil, err
	}
	if host == "" {
		host = "127.0.0.1"
	}
	l, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, errors.Wrapf(err, "error listening on %s", o.CallbackListener)
	}
	srv := &httptest.Server{
		Listener: l,
		Config:   &http.Server{Handler: o},
	}
	srv.Start()

	// Update server url to use for example http://localhost:port
	if host != "127.0.0.1" {
		_, p, err := net.SplitHostPort(l.Addr().String())
		if err != nil {
			return nil, errors.Wrapf(err, "error parsing %s", l.Addr().String())
		}
		srv.URL = "http://" + host + ":" + p
	}

	return srv, nil
}

// DoLoopbackAuthorization performs the log in into the identity provider
// opening a browser and using a redirect_uri in a loopback IP address
// (http://127.0.0.1:port or http://[::1]:port).
func (o *oauth) DoLoopbackAuthorization() (*token, error) {
	srv, err := o.NewServer()
	if err != nil {
		return nil, err
	}
	// Update server url if --listen-url is set
	if o.CallbackListenerURL != "" {
		o.redirectURI = o.CallbackListenerURL
	} else {
		o.redirectURI = srv.URL
	}
	defer srv.Close()

	// Get auth url and open it in a browser
	authURL, err := o.Auth()
	if err != nil {
		return nil, err
	}

	if err := exec.OpenInBrowser(authURL, o.browser); err != nil {
		fmt.Fprintln(os.Stderr, "Cannot open a web browser on your platform.")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Open a local web browser and visit:")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, authURL)
		fmt.Fprintln(os.Stderr)
	} else {
		fmt.Fprintln(os.Stderr, "Your default web browser has been opened to visit:")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, authURL)
		fmt.Fprintln(os.Stderr)
	}

	// Wait for response and return the token
	select {
	case tok := <-o.tokCh:
		return tok, nil
	case err := <-o.errCh:
		return nil, err
	case <-time.After(2 * time.Minute):
		return nil, errors.New("oauth command timed out, please try again")
	}
}

// DoManualAuthorization performs the log in into the identity provider
// allowing the user to open a browser on a different system and then entering
// the authorization code on the Step CLI.
func (o *oauth) DoManualAuthorization() (*token, error) {
	o.redirectURI = oobCallbackUrn
	authURL, err := o.Auth()
	if err != nil {
		return nil, err
	}

	fmt.Fprintln(os.Stderr, "Open a local web browser and visit:")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, authURL)
	fmt.Fprintln(os.Stderr)

	// Read from the command line
	fmt.Fprint(os.Stderr, "Enter verification code: ")
	code, err := utils.ReadString(os.Stdin)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	tok, err := o.Exchange(o.tokenEndpoint, code)
	if err != nil {
		return nil, err
	}
	if tok.Err != "" || tok.ErrDesc != "" {
		return nil, errors.Errorf("Error exchanging authorization code: %s. %s", tok.Err, tok.ErrDesc)
	}
	return tok, nil
}

// DoTwoLeggedAuthorization performs two-legged OAuth using the jwt-bearer
// grant type.
func (o *oauth) DoTwoLeggedAuthorization(issuer string) (*token, error) {
	pemBytes := []byte(o.clientSecret)
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("failed to read private key pem block")
	}
	priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.Wrap(err, "error parsing private key")
	}

	// Add claims
	now := int(time.Now().Unix())
	c := map[string]interface{}{
		"aud":   o.tokenEndpoint,
		"nbf":   now,
		"iat":   now,
		"exp":   now + 3600,
		"iss":   issuer,
		"scope": o.scope,
	}

	so := new(jose.SignerOptions)
	so.WithType("JWT")
	so.WithHeader("kid", o.clientID)

	// Sign JWT
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: "RS256",
		Key:       priv,
	}, so)
	if err != nil {
		return nil, errors.Wrapf(err, "error creating JWT signer")
	}

	raw, err := jose.Signed(signer).Claims(c).CompactSerialize()
	if err != nil {
		return nil, errors.Wrapf(err, "error serializing JWT")
	}

	// Construct the POST request to fetch the OAuth token.
	params := url.Values{
		"assertion":  []string{string(raw)},
		"grant_type": []string{jwtBearerUrn},
	}

	// Send the POST request and return token.
	resp, err := http.PostForm(o.tokenEndpoint, params)
	if err != nil {
		return nil, errors.Wrapf(err, "error from token endpoint")
	}
	defer resp.Body.Close()

	var tok token
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, errors.WithStack(err)
	}

	return &tok, nil
}

// DoJWTAuthorization generates a JWT instead of an OAuth token. Only works for
// certain APIs. See https://developers.google.com/identity/protocols/OAuth2ServiceAccount#jwt-auth.
func (o *oauth) DoJWTAuthorization(issuer, aud string) (*token, error) {
	pemBytes := []byte(o.clientSecret)
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("failed to read private key pem block")
	}
	priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.Wrap(err, "error parsing private key")
	}

	// Add claims
	now := int(time.Now().Unix())
	c := map[string]interface{}{
		"aud": aud,
		"nbf": now,
		"iat": now,
		"exp": now + 3600,
		"iss": issuer,
		"sub": issuer,
	}

	so := new(jose.SignerOptions)
	so.WithType("JWT")
	so.WithHeader("kid", o.clientID)

	// Sign JWT
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: "RS256",
		Key:       priv,
	}, so)
	if err != nil {
		return nil, errors.Wrapf(err, "error creating JWT signer")
	}

	raw, err := jose.Signed(signer).Claims(c).CompactSerialize()
	if err != nil {
		return nil, errors.Wrapf(err, "error serializing JWT")
	}

	tok := &token{string(raw), "", "", 3600, "Bearer", "", ""}
	return tok, nil
}

// ServeHTTP is the handler that performs the OAuth 2.0 dance and returns the
// tokens using channels.
func (o *oauth) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != o.CallbackPath {
		http.NotFound(w, req)
		return
	}

	q := req.URL.Query()
	errStr := q.Get("error")
	if errStr != "" {
		o.badRequest(w, "Failed to authenticate: "+errStr)
		return
	}

	if o.implicit {
		o.implicitHandler(w, req)
		return
	}

	code, state := q.Get("code"), q.Get("state")
	if code == "" || state == "" {
		fmt.Fprintf(os.Stderr, "Invalid request received: http://%s%s\n", req.RemoteAddr, req.URL.String())
		fmt.Fprintf(os.Stderr, "You may have an app or browser plugin that needs to be turned off\n")
		http.Error(w, "400 bad request", http.StatusBadRequest)
		return
	}

	if code == "" {
		o.badRequest(w, "Failed to authenticate: missing or invalid code")
		return
	}

	if state == "" || state != o.state {
		o.badRequest(w, "Failed to authenticate: missing or invalid state")
		return
	}

	tok, err := o.Exchange(o.tokenEndpoint, code)
	if err != nil {
		o.badRequest(w, "Failed exchanging authorization code: "+err.Error())
		return
	}
	if tok.Err != "" || tok.ErrDesc != "" {
		o.badRequest(w, fmt.Sprintf("Failed exchanging authorization code: %s. %s", tok.Err, tok.ErrDesc))
		return
	}

	if o.terminalRedirect != "" {
		http.Redirect(w, req, o.terminalRedirect, 302)
	} else {
		o.success(w)
	}
	o.tokCh <- tok
}

func (o *oauth) implicitHandler(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	if hash := q.Get("urlhash"); hash == "true" {
		state := q.Get("state")
		if state == "" || state != o.state {
			o.badRequest(w, "Failed to authenticate: missing or invalid state")
			return
		}
		accessToken := q.Get("access_token")
		if accessToken == "" {
			o.badRequest(w, "Failed to authenticate: missing access token")
			return
		}

		if o.terminalRedirect != "" {
			http.Redirect(w, req, o.terminalRedirect, 302)
		} else {
			o.success(w)
		}

		expiresIn, _ := strconv.Atoi(q.Get("expires_in"))
		o.tokCh <- &token{
			AccessToken:  accessToken,
			IDToken:      q.Get("id_token"),
			RefreshToken: q.Get("refresh_token"),
			ExpiresIn:    expiresIn,
			TokenType:    q.Get("token_type"),
		}
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Header().Add("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<html><head><title>Processing OAuth Request</title>`))
	w.Write([]byte(`</head>`))
	w.Write([]byte(`<script type="text/javascript">`))
	w.Write([]byte(fmt.Sprintf(`function redirect(){var hash = window.location.hash.substr(1); document.location.href = "%s?urlhash=true&"+hash;}`, o.redirectURI)))
	w.Write([]byte(`if (window.addEventListener) window.addEventListener("load", redirect, false); else if (window.attachEvent) window.attachEvent("onload", redirect); else window.onload = redirect;`))
	w.Write([]byte("</script>"))
	w.Write([]byte(`<body><p style='font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif, "Apple Color Emoji", "Segoe UI Emoji", "Segoe UI Symbol"; font-size: 22px; color: #333; width: 400px; margin: 0 auto; text-align: center; line-height: 1.7; padding: 20px;'>`))
	w.Write([]byte(`<strong style='font-size: 28px; color: #000;'>Success</strong><br />`))
	w.Write([]byte(`Click <a href="javascript:redirect();">here</a> if your browser does not automatically redirect you`))
	w.Write([]byte(`</p></body></html>`))
}

// Auth returns the OAuth 2.0 authentication url.
func (o *oauth) Auth() (string, error) {
	u, err := url.Parse(o.authzEndpoint)
	if err != nil {
		return "", errors.WithStack(err)
	}

	q := u.Query()
	q.Add("client_id", o.clientID)
	q.Add("redirect_uri", o.redirectURI)
	if o.implicit {
		q.Add("response_type", "id_token token")
	} else {
		q.Add("response_type", "code")
		q.Add("code_challenge_method", "S256")
		s256 := sha256.Sum256([]byte(o.codeChallenge))
		q.Add("code_challenge", base64.RawURLEncoding.EncodeToString(s256[:]))
	}
	q.Add("scope", o.scope)
	if o.prompt != "" {
		q.Add("prompt", o.prompt)
	}
	q.Add("state", o.state)
	q.Add("nonce", o.nonce)
	if o.loginHint != "" {
		q.Add("login_hint", o.loginHint)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Exchange exchanges the authorization code for refresh and access tokens.
func (o *oauth) Exchange(tokenEndpoint, code string) (*token, error) {
	data := url.Values{}
	data.Set("code", code)
	data.Set("client_id", o.clientID)
	data.Set("client_secret", o.clientSecret)
	data.Set("redirect_uri", o.redirectURI)
	data.Set("grant_type", "authorization_code")
	data.Set("code_verifier", o.codeChallenge)

	resp, err := http.PostForm(tokenEndpoint, data)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer resp.Body.Close()

	var tok token
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, errors.WithStack(err)
	}

	return &tok, nil
}

func (o *oauth) success(w http.ResponseWriter) {
	w.WriteHeader(http.StatusOK)
	w.Header().Add("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(`<html><head><title>OAuth Request Successful</title>`))
	w.Write([]byte(`</head><body><p style='font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif, "Apple Color Emoji", "Segoe UI Emoji", "Segoe UI Symbol"; font-size: 22px; color: #333; width: 400px; margin: 0 auto; text-align: center; line-height: 1.7; padding: 20px;'>`))
	w.Write([]byte(`<strong style='font-size: 28px; color: #000;'>Success</strong><br />Look for the token on the command line`))
	w.Write([]byte(`</p></body></html>`))
}

func (o *oauth) badRequest(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusBadRequest)
	w.Header().Add("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(`<html><head><title>OAuth Request Unsuccessful</title>`))
	w.Write([]byte(`</head><body><p style='font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif, "Apple Color Emoji", "Segoe UI Emoji", "Segoe UI Symbol"; font-size: 22px; color: #333; width: 400px; margin: 0 auto; text-align: center; line-height: 1.7; padding: 20px;'>`))
	w.Write([]byte(`<strong style='font-size: 28px; color: red;'>Failure</strong><br />`))
	w.Write([]byte(msg))
	w.Write([]byte(`</p></body></html>`))
	o.errCh <- errors.New(msg)
}
