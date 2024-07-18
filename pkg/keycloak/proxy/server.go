/*
Copyright 2015 All rights reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"

	"go.uber.org/zap/zapcore"

	"golang.org/x/crypto/acme/autocert"

	httplog "log"

	"github.com/Nerzal/gocloak/v12"
	proxyproto "github.com/armon/go-proxyproto"
	backoff "github.com/cenkalti/backoff/v4"
	oidc3 "github.com/coreos/go-oidc/v3/oidc"
	"github.com/elazarl/goproxy"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gogatekeeper/gatekeeper/pkg/apperrors"
	"github.com/gogatekeeper/gatekeeper/pkg/authorization"
	"github.com/gogatekeeper/gatekeeper/pkg/constant"
	"github.com/gogatekeeper/gatekeeper/pkg/encryption"
	"github.com/gogatekeeper/gatekeeper/pkg/keycloak/config"
	"github.com/gogatekeeper/gatekeeper/pkg/proxy/cookie"
	"github.com/gogatekeeper/gatekeeper/pkg/proxy/core"
	"github.com/gogatekeeper/gatekeeper/pkg/proxy/handlers"
	"github.com/gogatekeeper/gatekeeper/pkg/proxy/metrics"
	gmiddleware "github.com/gogatekeeper/gatekeeper/pkg/proxy/middleware"
	"github.com/gogatekeeper/gatekeeper/pkg/proxy/session"
	"github.com/gogatekeeper/gatekeeper/pkg/storage"
	"github.com/gogatekeeper/gatekeeper/pkg/utils"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/cors"

	_ "go.uber.org/automaxprocs" // fixes golang cgroup issue
	"go.uber.org/zap"
)

func init() {
	_, _ = time.LoadLocation("UTC")      // ensure all time is in UTC [NOTE(fredbi): no this does just nothing]
	runtime.GOMAXPROCS(runtime.NumCPU()) // set the core
	prometheus.MustRegister(metrics.CertificateRotationMetric)
	prometheus.MustRegister(metrics.LatencyMetric)
	prometheus.MustRegister(metrics.OauthLatencyMetric)
	prometheus.MustRegister(metrics.OauthTokensMetric)
	prometheus.MustRegister(metrics.StatusMetric)
}

// NewProxy create's a new proxy from configuration
//
//nolint:cyclop
func NewProxy(config *config.Config, log *zap.Logger, upstream core.ReverseProxy) (*OauthProxy, error) {
	var err error
	// create the service logger
	if log == nil {
		log, err = createLogger(config)
		if err != nil {
			return nil, err
		}
	}

	err = config.Update()
	if err != nil {
		return nil, err
	}

	log.Info(
		"starting the service",
		zap.String("prog", constant.Prog),
		zap.String("author", constant.Author),
		zap.String("version", core.Version),
	)

	svc := &OauthProxy{
		Config:         config,
		Log:            log,
		metricsHandler: promhttp.Handler(),
	}

	// parse the upstream endpoint
	if svc.Endpoint, err = url.Parse(config.Upstream); err != nil {
		return nil, err
	}

	// initialize the store if any
	if config.StoreURL != "" {
		if svc.Store, err = storage.CreateStorage(config.StoreURL); err != nil {
			return nil, err
		}
	}

	svc.Log.Info(
		"attempting to retrieve configuration discovery url",
		zap.String("url", svc.Config.DiscoveryURL),
		zap.String("timeout", svc.Config.OpenIDProviderTimeout.String()),
	)

	// initialize the openid client
	if svc.Provider, svc.IdpClient, err = svc.NewOpenIDProvider(); err != nil {
		svc.Log.Error(
			"failed to get provider configuration from discovery",
			zap.Error(err),
		)
		return nil, err
	}

	svc.Log.Info("successfully retrieved openid configuration from the discovery")

	if config.EnableUma || config.EnableForwarding {
		patDone := make(chan bool)
		svc.pat = &PAT{}
		go getPAT(
			log,
			svc.pat,
			config.ClientID,
			config.ClientSecret,
			config.Realm,
			config.OpenIDProviderTimeout,
			config.PatRetryCount,
			config.PatRetryInterval,
			config.EnableForwarding,
			config.ForwardingGrantType,
			svc.IdpClient,
			config.ForwardingUsername,
			config.ForwardingPassword,
			patDone,
		)
		<-patDone
	}

	if config.SkipTokenVerification {
		log.Warn(
			"TESTING ONLY CONFIG - access token verification has been disabled",
		)
	}

	if config.ClientID == "" && config.ClientSecret == "" {
		log.Warn(
			"client credentials are not set, depending on " +
				"provider (confidential|public) you might be unable to auth",
		)
	}

	if upstream != nil {
		svc.Upstream = upstream
	}

	// are we running in forwarding mode?
	if config.EnableForwarding {
		if err := svc.createForwardingProxy(); err != nil {
			return nil, err
		}
	} else {
		if err := svc.CreateReverseProxy(); err != nil {
			return nil, err
		}
	}

	return svc, nil
}

// createLogger is responsible for creating the service logger
func createLogger(config *config.Config) (*zap.Logger, error) {
	httplog.SetOutput(io.Discard) // disable the http logger

	if config.DisableAllLogging {
		return zap.NewNop(), nil
	}

	cfg := zap.NewProductionConfig()
	cfg.DisableStacktrace = true
	cfg.DisableCaller = true

	// Use human-readable timestamps in the logs until KEYCLOAK-12100 is fixed
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	// are we enabling json logging?
	if !config.EnableJSONLogging {
		cfg.Encoding = "console"
	}

	// are we running verbose mode?
	if config.Verbose {
		httplog.SetOutput(os.Stderr)
		cfg.DisableCaller = false
		cfg.Development = true
		cfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	}

	return cfg.Build()
}

// useDefaultStack sets the default middleware stack for router
func (r *OauthProxy) useDefaultStack(engine chi.Router) {
	engine.NotFound(handlers.EmptyHandler)

	if r.Config.EnableDefaultDeny || r.Config.EnableDefaultDenyStrict {
		engine.Use(gmiddleware.MethodCheckMiddleware(r.Log))
	} else {
		engine.MethodNotAllowed(handlers.EmptyHandler)
	}

	engine.Use(middleware.Recoverer)

	// @check if the request tracking id middleware is enabled
	if r.Config.EnableRequestID {
		r.Log.Info("enabled the correlation request id middleware")
		engine.Use(gmiddleware.RequestIDMiddleware(r.Config.RequestIDHeader))
	}

	if r.Config.EnableCompression {
		engine.Use(middleware.Compress(5))
	}

	// @step: enable the entrypoint middleware
	engine.Use(gmiddleware.EntrypointMiddleware(r.Log, r.Config.ForwardAuthMode))

	if r.Config.EnableLogging {
		engine.Use(gmiddleware.LoggingMiddleware(r.Log, r.Config.Verbose))
	}

	// step: load the templates if any
	tmpl := createTemplates(
		r.Log,
		r.Config.SignInPage,
		r.Config.ForbiddenPage,
		r.Config.ErrorPage,
	)

	r.accessForbidden = core.AccessForbidden(
		r.Log,
		http.StatusForbidden,
		r.Config.ForbiddenPage,
		r.Config.Tags,
		tmpl,
	)

	r.accessError = core.AccessForbidden(
		r.Log,
		http.StatusBadRequest,
		r.Config.ErrorPage,
		r.Config.Tags,
		tmpl,
	)

	r.customSignInPage = core.CustomSignInPage(
		r.Log,
		r.Config.SignInPage,
		r.Config.Tags,
		tmpl,
	)

	if r.Config.EnableSecurityFilter {
		engine.Use(
			gmiddleware.SecurityMiddleware(
				r.Log,
				r.Config.Hostnames,
				r.Config.EnableBrowserXSSFilter,
				r.Config.ContentSecurityPolicy,
				r.Config.EnableContentNoSniff,
				r.Config.EnableFrameDeny,
				r.Config.EnableHTTPSRedirect,
				r.accessForbidden,
			),
		)
	}
}

// createReverseProxy creates a reverse proxy
//
//nolint:cyclop,funlen
func (r *OauthProxy) CreateReverseProxy() error {
	r.Log.Info(
		"enabled reverse proxy mode, upstream url",
		zap.String("url", r.Config.Upstream),
	)

	if r.Upstream == nil {
		if err := r.createUpstreamProxy(r.Endpoint); err != nil {
			return err
		}
	}

	engine := chi.NewRouter()
	r.useDefaultStack(engine)

	r.WithOAuthURI = utils.WithOAuthURI(r.Config.BaseURI, r.Config.OAuthURI)
	r.Cm = &cookie.Manager{
		CookieDomain:         r.Config.CookieDomain,
		BaseURI:              r.Config.BaseURI,
		HTTPOnlyCookie:       r.Config.HTTPOnlyCookie,
		SecureCookie:         r.Config.SecureCookie,
		EnableSessionCookies: r.Config.EnableSessionCookies,
		SameSiteCookie:       r.Config.SameSiteCookie,
		CookieAccessName:     r.Config.CookieAccessName,
		CookieRefreshName:    r.Config.CookieRefreshName,
		CookieIDTokenName:    r.Config.CookieIDTokenName,
		CookiePKCEName:       r.Config.CookiePKCEName,
		CookieUMAName:        r.Config.CookieUMAName,
		CookieRequestURIName: r.Config.CookieRequestURIName,
		CookieOAuthStateName: r.Config.CookieOAuthStateName,
		NoProxy:              r.Config.NoProxy,
		NoRedirects:          r.Config.NoRedirects,
	}

	r.newOAuth2Config = utils.NewOAuth2Config(
		r.Config.ClientID,
		r.Config.ClientSecret,
		r.Provider.Endpoint().AuthURL,
		r.Provider.Endpoint().TokenURL,
		r.Config.Scopes,
	)

	r.GetIdentity = session.GetIdentity(
		r.Log,
		r.Config.SkipAuthorizationHeaderIdentity,
		r.Config.EnableEncryptedToken,
		r.Config.ForceEncryptedCookie,
		r.Config.EncryptionKey,
	)

	r.getRedirectionURL = handlers.GetRedirectionURL(
		r.Log,
		r.Config.RedirectionURL,
		r.Config.NoProxy,
		r.Config.NoRedirects,
		r.Config.SecureCookie,
		r.Config.CookieOAuthStateName,
		r.WithOAuthURI,
	)

	redToAuth := core.RedirectToAuthorization(
		r.Log,
		r.Config.NoRedirects,
		r.Cm,
		r.Config.SkipTokenVerification,
		r.Config.NoProxy,
		r.Config.BaseURI,
		r.Config.OAuthURI,
		r.Config.AllowedQueryParams,
		r.Config.DefaultAllowedQueryParams,
	)

	if r.Config.EnableHmac {
		engine.Use(gmiddleware.HmacMiddleware(r.Log, r.Config.EncryptionKey))
	}

	// @step: configure CORS middleware
	if len(r.Config.CorsOrigins) > 0 {
		corsHandler := cors.New(cors.Options{
			AllowedOrigins:   r.Config.CorsOrigins,
			AllowedMethods:   r.Config.CorsMethods,
			AllowedHeaders:   r.Config.CorsHeaders,
			AllowCredentials: r.Config.CorsCredentials,
			ExposedHeaders:   r.Config.CorsExposedHeaders,
			MaxAge:           int(r.Config.CorsMaxAge.Seconds()),
			Debug:            r.Config.Verbose,
		})

		engine.Use(corsHandler.Handler)
	}

	proxyMiddle := gmiddleware.ProxyMiddleware(
		r.Log,
		r.Config.CorsOrigins,
		r.Config.Headers,
		r.Endpoint,
		r.Config.PreserveHost,
		r.Upstream,
	)
	if !r.Config.NoProxy {
		engine.Use(proxyMiddle)
	}

	r.Router = engine

	if len(r.Config.ResponseHeaders) > 0 {
		engine.Use(gmiddleware.ResponseHeaderMiddleware(r.Config.ResponseHeaders))
	}

	// step: define admin subrouter: health and metrics
	adminEngine := chi.NewRouter()

	r.Log.Info(
		"enabled health service",
		zap.String("path", path.Clean(r.WithOAuthURI(constant.HealthURL))),
	)

	adminEngine.Get(constant.HealthURL, handlers.HealthHandler)

	if r.Config.EnableMetrics {
		r.Log.Info(
			"enabled the service metrics middleware",
			zap.String("path", path.Clean(r.WithOAuthURI(constant.MetricsURL))),
		)
		adminEngine.Get(
			constant.MetricsURL,
			handlers.ProxyMetricsHandler(
				r.Config.LocalhostMetrics,
				r.accessForbidden,
				r.metricsHandler,
			),
		)
	}

	authMid := gmiddleware.AuthenticationMiddleware(
		r.Log,
		r.Config.CookieAccessName,
		r.Config.CookieRefreshName,
		r.GetIdentity,
		r.IdpClient.RestyClient().GetClient(),
		r.Config.EnableIDPSessionCheck,
		r.Provider,
		r.Config.SkipTokenVerification,
		r.Config.ClientID,
		r.Config.SkipAccessTokenClientIDCheck,
		r.Config.SkipAccessTokenIssuerCheck,
		r.accessForbidden,
		r.Config.EnableRefreshTokens,
		r.Config.RedirectionURL,
		r.Cm,
		r.Config.EnableEncryptedToken,
		r.Config.ForceEncryptedCookie,
		r.Config.EncryptionKey,
		redToAuth,
		r.newOAuth2Config,
		r.Store,
		r.Config.AccessTokenDuration,
	)

	loginHand := loginHandler(
		r.Log,
		r.Config.OpenIDProviderTimeout,
		r.IdpClient.RestyClient().GetClient(),
		r.Config.EnableLoginHandler,
		r.newOAuth2Config,
		r.getRedirectionURL,
		r.Config.EnableEncryptedToken,
		r.Config.ForceEncryptedCookie,
		r.Config.EncryptionKey,
		r.Config.EnableRefreshTokens,
		r.Config.EnableIDTokenCookie,
		r.Cm,
		r.Config.AccessTokenDuration,
		r.Store,
	)

	logoutHand := logoutHandler(
		r.Log,
		r.Config.PostLogoutRedirectURI,
		r.Config.RedirectionURL,
		r.Config.DiscoveryURL,
		r.Config.RevocationEndpoint,
		r.Config.CookieAccessName,
		r.Config.CookieIDTokenName,
		r.Config.CookieRefreshName,
		r.Config.ClientID,
		r.Config.ClientSecret,
		r.Config.EncryptionKey,
		r.Config.EnableEncryptedToken,
		r.Config.ForceEncryptedCookie,
		r.Config.EnableLogoutRedirect,
		r.Store,
		r.Cm,
		r.IdpClient.RestyClient().GetClient(),
		r.accessError,
		r.GetIdentity,
	)

	oauthCallbackHand := oauthCallbackHandler(
		r.Log,
		r.Config.ClientID,
		r.Config.Realm,
		r.Config.CookiePKCEName,
		r.Config.CookieRequestURIName,
		r.Config.PostLoginRedirectPath,
		r.Config.EncryptionKey,
		r.Config.SkipTokenVerification,
		r.Config.SkipAccessTokenClientIDCheck,
		r.Config.SkipAccessTokenIssuerCheck,
		r.Config.EnableRefreshTokens,
		r.Config.EnableUma,
		r.Config.EnableUmaMethodScope,
		r.Config.EnableIDTokenCookie,
		r.Config.EnableEncryptedToken,
		r.Config.ForceEncryptedCookie,
		r.Config.EnablePKCE,
		r.Provider,
		r.Cm,
		r.pat,
		r.IdpClient,
		r.Store,
		r.newOAuth2Config,
		r.getRedirectionURL,
		r.accessForbidden,
		r.accessError,
	)

	oauthAuthorizationHand := oauthAuthorizationHandler(
		r.Log,
		r.Config.SkipTokenVerification,
		r.Config.Scopes,
		r.Config.EnablePKCE,
		r.Config.SignInPage,
		r.Cm,
		r.newOAuth2Config,
		r.getRedirectionURL,
		r.customSignInPage,
		r.Config.AllowedQueryParams,
		r.Config.DefaultAllowedQueryParams,
	)

	// step: add the routing for oauth
	engine.With(gmiddleware.ProxyDenyMiddleware(r.Log)).Route(r.Config.BaseURI+r.Config.OAuthURI, func(eng chi.Router) {
		eng.MethodNotAllowed(handlers.MethodNotAllowHandlder)
		eng.HandleFunc(constant.AuthorizationURL, oauthAuthorizationHand)
		eng.Get(constant.CallbackURL, oauthCallbackHand)
		eng.Get(constant.ExpiredURL, handlers.ExpirationHandler(r.GetIdentity, r.Config.CookieAccessName))
		eng.With(authMid).Get(constant.LogoutURL, logoutHand)
		eng.With(authMid).Get(
			constant.TokenURL,
			handlers.TokenHandler(r.GetIdentity, r.Config.CookieAccessName, r.accessError),
		)
		eng.Post(constant.LoginURL, loginHand)
		eng.Get(constant.DiscoveryURL, handlers.DiscoveryHandler(r.Log, r.WithOAuthURI))

		if r.Config.ListenAdmin == "" {
			eng.Mount("/", adminEngine)
		}

		eng.NotFound(http.NotFound)
	})

	// step: define profiling subrouter
	var debugEngine chi.Router

	if r.Config.EnableProfiling {
		r.Log.Warn("enabling the debug profiling on " + constant.DebugURL)

		debugEngine = chi.NewRouter()
		debugEngine.Get("/{name}", handlers.DebugHandler)
		debugEngine.Post("/{name}", handlers.DebugHandler)

		// @check if the server write-timeout is still set and throw a warning
		if r.Config.ServerWriteTimeout > 0 {
			r.Log.Warn(
				"you should disable the server write timeout ( " +
					"--server-write-timeout) when using pprof profiling",
			)
		}

		if r.Config.ListenAdmin == "" {
			engine.With(gmiddleware.ProxyDenyMiddleware(r.Log)).Mount(constant.DebugURL, debugEngine)
		}
	}

	if r.Config.ListenAdmin != "" {
		// mount admin and debug engines separately
		r.Log.Info("mounting admin endpoints on separate listener")

		admin := chi.NewRouter()
		admin.MethodNotAllowed(handlers.EmptyHandler)
		admin.NotFound(handlers.EmptyHandler)
		admin.Use(middleware.Recoverer)
		admin.Use(gmiddleware.ProxyDenyMiddleware(r.Log))
		admin.Route("/", func(e chi.Router) {
			e.Mount(r.Config.OAuthURI, adminEngine)
			if debugEngine != nil {
				e.Mount(constant.DebugURL, debugEngine)
			}
		})

		r.adminRouter = admin
	}

	if r.Config.NoProxy && !r.Config.NoRedirects {
		r.Log.Warn("using noproxy=true and noredirects=false " +
			", enabling use of X-FORWARDED-* headers, please " +
			"use only behind trusted proxy!")
	}

	if r.Config.EnableSessionCookies {
		r.Log.Info("using session cookies only for access and refresh tokens")
	}

	// step: add custom http methods
	if r.Config.CustomHTTPMethods != nil {
		for _, customHTTPMethod := range r.Config.CustomHTTPMethods {
			chi.RegisterMethod(customHTTPMethod)
			utils.AllHTTPMethods = append(utils.AllHTTPMethods, customHTTPMethod)
		}
	}

	// step: provision in the protected resources
	enableDefaultDeny := r.Config.EnableDefaultDeny
	enableDefaultDenyStrict := r.Config.EnableDefaultDenyStrict

	for _, res := range r.Config.Resources {
		if res.URL == "/" {
			r.Log.Warn("please be aware that '/' is only referring to site-root " +
				", to specify all path underneath use '/*'")
		}

		if res.URL[len(res.URL)-1:] == "/" && res.URL != "/" {
			r.Log.Warn("the resource url is not a prefix",
				zap.String("resource", res.URL),
				zap.String("change", res.URL),
				zap.String("amended", strings.TrimRight(res.URL, "/")))
		}
	}

	if enableDefaultDeny || enableDefaultDenyStrict {
		r.Log.Info("adding a default denial into the protected resources")

		r.Config.Resources = append(
			r.Config.Resources,
			&authorization.Resource{URL: constant.AllPath, Methods: utils.AllHTTPMethods},
		)
	}

	for _, res := range r.Config.Resources {
		r.Log.Info(
			"protecting resource",
			zap.String("resource", res.String()),
		)

		middlewares := []func(http.Handler) http.Handler{
			authMid,
			gmiddleware.AdmissionMiddleware(
				r.Log,
				res,
				r.Config.MatchClaims,
				r.accessForbidden,
			),
			gmiddleware.IdentityHeadersMiddleware(
				r.Log,
				r.Config.AddClaims,
				r.Config.CookieAccessName,
				r.Config.CookieRefreshName,
				r.Config.NoProxy,
				r.Config.EnableTokenHeader,
				r.Config.EnableAuthorizationHeader,
				r.Config.EnableAuthorizationCookies,
			),
		}

		if res.URL == constant.AllPath && !res.WhiteListed && enableDefaultDenyStrict {
			middlewares = []func(http.Handler) http.Handler{
				gmiddleware.DenyMiddleware(r.Log, r.accessForbidden),
				gmiddleware.ProxyDenyMiddleware(r.Log),
			}
		}

		if r.Config.EnableUma || r.Config.EnableOpa {
			middlewares = []func(http.Handler) http.Handler{
				authMid,
				authorizationMiddleware(
					r.Log,
					r.Config.EnableUma,
					r.Config.EnableUmaMethodScope,
					r.Config.CookieUMAName,
					r.Config.NoProxy,
					r.pat,
					r.Provider,
					r.IdpClient,
					r.Config.OpenIDProviderTimeout,
					r.Config.Realm,
					r.Config.EnableEncryptedToken,
					r.Config.ForceEncryptedCookie,
					r.Config.EncryptionKey,
					r.Cm,
					r.Config.EnableOpa,
					r.Config.OpaTimeout,
					r.Config.OpaAuthzURL,
					r.Config.DiscoveryURI,
					r.Config.ClientID,
					r.Config.SkipAccessTokenClientIDCheck,
					r.Config.SkipAccessTokenIssuerCheck,
					r.GetIdentity,
					r.accessForbidden,
				),
				gmiddleware.AdmissionMiddleware(
					r.Log,
					res,
					r.Config.MatchClaims,
					r.accessForbidden,
				),
				gmiddleware.IdentityHeadersMiddleware(
					r.Log,
					r.Config.AddClaims,
					r.Config.CookieAccessName,
					r.Config.CookieRefreshName,
					r.Config.NoProxy,
					r.Config.EnableTokenHeader,
					r.Config.EnableAuthorizationHeader,
					r.Config.EnableAuthorizationCookies,
				),
			}
		}

		e := engine.With(middlewares...)

		for _, method := range res.Methods {
			if !res.WhiteListed {
				e.MethodFunc(method, res.URL, handlers.EmptyHandler)
				continue
			}

			engine.MethodFunc(method, res.URL, handlers.EmptyHandler)
		}
	}

	for name, value := range r.Config.MatchClaims {
		r.Log.Info(
			"token must contain",
			zap.String("claim", name),
			zap.String("value", value),
		)
	}

	if r.Config.RedirectionURL == "" && !r.Config.NoRedirects {
		r.Log.Warn("no redirection url has been set, will use host headers")
	}

	if r.Config.EnableEncryptedToken {
		r.Log.Info("session access tokens will be encrypted")
	}

	return nil
}

// createForwardingProxy creates a forwarding proxy
//
//nolint:cyclop
func (r *OauthProxy) createForwardingProxy() error {
	r.Log.Info(
		"enabling forward signing mode, listening on",
		zap.String("interface", r.Config.Listen),
	)

	if r.Config.SkipUpstreamTLSVerify {
		r.Log.Warn(
			"tls verification switched off. In forward signing mode it's " +
				"recommended you verify! (--skip-upstream-tls-verify=false)",
		)
	}

	r.rpt = &RPT{}

	if err := r.createUpstreamProxy(nil); err != nil {
		return err
	}
	//nolint:bodyclose
	forwardingHandler := forwardProxyHandler(
		r.Log,
		r.pat,
		r.rpt,
		r.Config.EnableUma,
		r.Config.ForwardingDomains,
		r.Config.EnableHmac,
		r.Config.EncryptionKey,
	)

	// set the http handler
	proxy, assertOk := r.Upstream.(*goproxy.ProxyHttpServer)

	if !assertOk {
		return apperrors.ErrAssertionFailed
	}

	r.Router = proxy

	// setup the tls configuration
	if r.Config.TLSCaCertificate != "" && r.Config.TLSCaPrivateKey != "" {
		cAuthority, err := encryption.LoadCA(r.Config.TLSCaCertificate, r.Config.TLSCaPrivateKey)

		if err != nil {
			return fmt.Errorf("unable to load certificate authority, error: %s", err)
		}

		// implement the goproxy connect method
		proxy.OnRequest().HandleConnectFunc(
			func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
				return &goproxy.ConnectAction{
					Action:    goproxy.ConnectMitm,
					TLSConfig: goproxy.TLSConfigFromCA(cAuthority),
				}, host
			},
		)
	} else {
		// use the default certificate provided by goproxy
		proxy.OnRequest().HandleConnect(goproxy.AlwaysMitm)
	}

	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		// @NOTES, somewhat annoying but goproxy hands back a nil response on proxy client errors
		if resp != nil {
			if r.Config.EnableLogging {
				start, assertOk := ctx.UserData.(time.Time)
				if !assertOk {
					r.Log.Error(apperrors.ErrAssertionFailed.Error())
					return nil
				}

				latency := time.Since(start)
				metrics.LatencyMetric.Observe(latency.Seconds())

				r.Log.Info("client request",
					zap.String("method", resp.Request.Method),
					zap.String("path", resp.Request.URL.Path),
					zap.Int("status", resp.StatusCode),
					zap.Int64("bytes", resp.ContentLength),
					zap.String("host", resp.Request.Host),
					zap.String("path", resp.Request.URL.Path),
					zap.String("latency", latency.String()))
			}

			if r.Config.EnableUma {
				umaToken := resp.Header.Get(constant.UMAHeader)
				if umaToken != "" {
					r.rpt.m.Lock()
					r.rpt.Token = umaToken
					r.rpt.m.Unlock()
				}
			}
		}

		return resp
	})
	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		ctx.UserData = time.Now()
		forwardingHandler(req, ctx.Resp)
		return req, ctx.Resp
	})

	return nil
}

// Run starts the proxy service
//
//nolint:cyclop
func (r *OauthProxy) Run() error {
	listener, err := r.createHTTPListener(makeListenerConfig(r.Config))

	if err != nil {
		return err
	}

	// step: create the main http(s) server
	server := &http.Server{
		Addr:         r.Config.Listen,
		Handler:      r.Router,
		ReadTimeout:  r.Config.ServerReadTimeout,
		WriteTimeout: r.Config.ServerWriteTimeout,
		IdleTimeout:  r.Config.ServerIdleTimeout,
	}

	r.Server = server
	r.Listener = listener

	go func() {
		r.Log.Info(
			"Gatekeeper proxy service starting",
			zap.String("interface", r.Config.Listen),
		)

		if err = server.Serve(listener); err != nil {
			if err != http.ErrServerClosed {
				r.Log.Fatal("failed to start the http service", zap.Error(err))
			}
		}
	}()

	// step: are we running http service as well?
	if r.Config.ListenHTTP != "" {
		r.Log.Info(
			"Gatekeeper proxy http service starting",
			zap.String("interface", r.Config.ListenHTTP),
		)

		httpListener, err := r.createHTTPListener(listenerConfig{
			listen:        r.Config.ListenHTTP,
			proxyProtocol: r.Config.EnableProxyProtocol,
		})

		if err != nil {
			return err
		}

		httpsvc := &http.Server{
			Addr:         r.Config.ListenHTTP,
			Handler:      r.Router,
			ReadTimeout:  r.Config.ServerReadTimeout,
			WriteTimeout: r.Config.ServerWriteTimeout,
			IdleTimeout:  r.Config.ServerIdleTimeout,
		}

		go func() {
			if err := httpsvc.Serve(httpListener); err != nil {
				r.Log.Fatal("failed to start the http redirect service", zap.Error(err))
			}
		}()
	}

	// step: are we running specific admin service as well?
	// if not, admin endpoints are added as routes in the main service
	if r.Config.ListenAdmin != "" {
		r.Log.Info(
			"keycloak proxy admin service starting",
			zap.String("interface", r.Config.ListenAdmin),
		)

		var (
			adminListener net.Listener
			err           error
		)

		if r.Config.ListenAdminScheme == constant.UnsecureScheme {
			// run the admin endpoint (metrics, health) with http
			adminListener, err = r.createHTTPListener(listenerConfig{
				listen:        r.Config.ListenAdmin,
				proxyProtocol: r.Config.EnableProxyProtocol,
			})

			if err != nil {
				return err
			}
		} else {
			adminListenerConfig := makeListenerConfig(r.Config)
			// admin specific overides
			adminListenerConfig.listen = r.Config.ListenAdmin

			// TLS configuration defaults to the one for the main service,
			// and may be overidden
			if r.Config.TLSAdminPrivateKey != "" && r.Config.TLSAdminCertificate != "" {
				adminListenerConfig.useFileTLS = true
				adminListenerConfig.certificate = r.Config.TLSAdminCertificate
				adminListenerConfig.privateKey = r.Config.TLSAdminPrivateKey
			}

			if r.Config.TLSAdminCaCertificate != "" {
				adminListenerConfig.ca = r.Config.TLSAdminCaCertificate
			}

			if r.Config.TLSAdminClientCertificate != "" {
				adminListenerConfig.clientCert = r.Config.TLSAdminClientCertificate
			}

			adminListener, err = r.createHTTPListener(adminListenerConfig)
			if err != nil {
				return err
			}
		}

		adminsvc := &http.Server{
			Addr:         r.Config.ListenAdmin,
			Handler:      r.adminRouter,
			ReadTimeout:  r.Config.ServerReadTimeout,
			WriteTimeout: r.Config.ServerWriteTimeout,
			IdleTimeout:  r.Config.ServerIdleTimeout,
		}

		go func() {
			if ers := adminsvc.Serve(adminListener); err != nil {
				r.Log.Fatal("failed to start the admin service", zap.Error(ers))
			}
		}()
	}

	return nil
}

// listenerConfig encapsulate listener options
type listenerConfig struct {
	ca                  string   // the path to a certificate authority
	certificate         string   // the path to the certificate if any
	clientCert          string   // the path to a client certificate to use for mutual tls
	hostnames           []string // list of hostnames the service will respond to
	letsEncryptCacheDir string   // the path to cache letsencrypt certificates
	listen              string   // the interface to bind the listener to
	privateKey          string   // the path to the private key if any
	proxyProtocol       bool     // whether to enable proxy protocol on the listen
	redirectionURL      string   // url to redirect to
	useFileTLS          bool     // indicates we are using certificates from files
	useLetsEncryptTLS   bool     // indicates we are using letsencrypt
	useSelfSignedTLS    bool     // indicates we are using the self-signed tls
	minTLSVersion       uint16   // server minimal TLS version
}

// makeListenerConfig extracts a listener configuration from a proxy Config
func makeListenerConfig(config *config.Config) listenerConfig {
	var minTLSVersion uint16
	switch strings.ToLower(config.TLSMinVersion) {
	case "":
		minTLSVersion = 0 // zero means default value
	case "tlsv1.0":
		minTLSVersion = tls.VersionTLS10
	case "tlsv1.1":
		minTLSVersion = tls.VersionTLS11
	case "tlsv1.2":
		minTLSVersion = tls.VersionTLS12
	case "tlsv1.3":
		minTLSVersion = tls.VersionTLS13
	}

	return listenerConfig{
		hostnames:           config.Hostnames,
		letsEncryptCacheDir: config.LetsEncryptCacheDir,
		listen:              config.Listen,
		proxyProtocol:       config.EnableProxyProtocol,
		redirectionURL:      config.RedirectionURL,

		// TLS settings
		useFileTLS:        config.TLSPrivateKey != "" && config.TLSCertificate != "",
		privateKey:        config.TLSPrivateKey,
		ca:                config.TLSCaCertificate,
		certificate:       config.TLSCertificate,
		clientCert:        config.TLSClientCertificate,
		useLetsEncryptTLS: config.UseLetsEncrypt,
		useSelfSignedTLS:  config.EnabledSelfSignedTLS,
		minTLSVersion:     minTLSVersion,
	}
}

// ErrHostNotConfigured indicates the hostname was not configured
var ErrHostNotConfigured = errors.New("acme/autocert: host not configured")

// createHTTPListener is responsible for creating a listening socket
//
//nolint:cyclop
func (r *OauthProxy) createHTTPListener(config listenerConfig) (net.Listener, error) {
	var listener net.Listener
	var err error

	// are we create a unix socket or tcp listener?
	if strings.HasPrefix(config.listen, "unix://") {
		socket := config.listen[7:]

		if exists := utils.FileExists(socket); exists {
			if err = os.Remove(socket); err != nil {
				return nil, err
			}
		}

		r.Log.Info(
			"listening on unix socket",
			zap.String("interface", config.listen),
		)

		if listener, err = net.Listen("unix", socket); err != nil {
			return nil, err
		}
	} else { //nolint:gocritic
		if listener, err = net.Listen("tcp", config.listen); err != nil {
			return nil, err
		}
	}

	// does it require proxy protocol?
	if config.proxyProtocol {
		r.Log.Info(
			"enabling the proxy protocol on listener",
			zap.String("interface", config.listen),
		)
		listener = &proxyproto.Listener{Listener: listener}
	}

	// @check if the socket requires TLS
	if config.useSelfSignedTLS || config.useLetsEncryptTLS || config.useFileTLS {
		getCertificate := func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return nil, errors.New("not configured")
		}

		if config.useLetsEncryptTLS {
			r.Log.Info("enabling letsencrypt tls support")

			manager := autocert.Manager{
				Prompt: autocert.AcceptTOS,
				Cache:  autocert.DirCache(config.letsEncryptCacheDir),
				HostPolicy: func(_ context.Context, host string) error {
					if len(config.hostnames) > 0 {
						found := false

						for _, h := range config.hostnames {
							found = found || (h == host)
						}

						if !found {
							return ErrHostNotConfigured
						}
					} else if config.redirectionURL != "" {
						if u, err := url.Parse(config.redirectionURL); err != nil {
							return err
						} else if u.Host != host {
							return ErrHostNotConfigured
						}
					}

					return nil
				},
			}

			getCertificate = manager.GetCertificate
		}

		if config.useSelfSignedTLS {
			r.Log.Info(
				"enabling self-signed tls support",
				zap.Duration("expiration", r.Config.SelfSignedTLSExpiration),
			)

			rotate, err := encryption.NewSelfSignedCertificate(
				r.Config.SelfSignedTLSHostnames,
				r.Config.SelfSignedTLSExpiration,
				r.Log,
			)

			if err != nil {
				return nil, err
			}

			getCertificate = rotate.GetCertificate
		}

		if config.useFileTLS {
			r.Log.Info(
				"tls support enabled",
				zap.String("certificate", config.certificate),
				zap.String("private_key", config.privateKey),
			)

			rotate, err := encryption.NewCertificateRotator(
				config.certificate,
				config.privateKey,
				r.Log,
				&metrics.CertificateRotationMetric,
			)

			if err != nil {
				return nil, err
			}

			// start watching the files for changes
			if err := rotate.Watch(); err != nil {
				return nil, err
			}

			getCertificate = rotate.GetCertificate
		}

		tlsConfig := &tls.Config{
			GetCertificate: getCertificate,
			// Causes servers to use Go's default ciphersuite preferences,
			// which are tuned to avoid attacks. Does nothing on clients.
			PreferServerCipherSuites: true,
			NextProtos:               []string{"h2", "http/1.1"},
			MinVersion:               config.minTLSVersion,
		}

		listener = tls.NewListener(listener, tlsConfig)

		// @check if we doing mutual tls
		if config.clientCert != "" {
			caCert, err := os.ReadFile(config.clientCert)

			if err != nil {
				return nil, err
			}

			caCertPool := x509.NewCertPool()
			caCertPool.AppendCertsFromPEM(caCert)
			tlsConfig.ClientCAs = caCertPool
			tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		}
	}

	return listener, nil
}

// createUpstreamProxy create a reverse http proxy from the upstream
func (r *OauthProxy) createUpstreamProxy(upstream *url.URL) error {
	dialer := (&net.Dialer{
		KeepAlive: r.Config.UpstreamKeepaliveTimeout,
		Timeout:   r.Config.UpstreamTimeout,
	}).Dial

	// are we using a unix socket?
	if upstream != nil && upstream.Scheme == "unix" {
		r.Log.Info(
			"using unix socket for upstream",
			zap.String("socket", fmt.Sprintf("%s%s", upstream.Host, upstream.Path)),
		)

		socketPath := fmt.Sprintf("%s%s", upstream.Host, upstream.Path)
		dialer = func(network, address string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		}

		upstream.Path = ""
		upstream.Host = "domain-sock"
		upstream.Scheme = constant.UnsecureScheme
	}
	// create the upstream tls configure
	//nolint:gas
	tlsConfig := &tls.Config{InsecureSkipVerify: r.Config.SkipUpstreamTLSVerify}

	// are we using a client certificate
	// @TODO provide a means of reload on the client certificate when it expires. I'm not sure if it's just a
	// case of update the http transport settings - Also we to place this go-routine?
	if r.Config.TLSClientCertificate != "" {
		cert, err := os.ReadFile(r.Config.TLSClientCertificate)

		if err != nil {
			r.Log.Error(
				"unable to read client certificate",
				zap.String("path", r.Config.TLSClientCertificate),
				zap.Error(err),
			)
			return err
		}

		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(cert)
		tlsConfig.ClientCAs = pool
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}

	{
		// @check if we have a upstream ca to verify the upstream
		if r.Config.UpstreamCA != "" {
			r.Log.Info(
				"loading the upstream ca",
				zap.String("path", r.Config.UpstreamCA),
			)

			cAuthority, err := os.ReadFile(r.Config.UpstreamCA)

			if err != nil {
				return err
			}

			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(cAuthority)
			tlsConfig.RootCAs = pool
		}
	}

	// create the forwarding proxy
	proxy := goproxy.NewProxyHttpServer()

	// headers formed by middleware before proxying to upstream shall be
	// kept in response. This is true for CORS headers ([KEYCOAK-9045])
	// and for refreshed cookies (htts://github.com/louketo/louketo-proxy/pulls/456])
	proxy.KeepDestinationHeaders = true
	proxy.Logger = httplog.New(io.Discard, "", 0)
	r.Upstream = proxy

	// update the tls configuration of the reverse proxy
	upstreamProxy, assertOk := r.Upstream.(*goproxy.ProxyHttpServer)

	if !assertOk {
		return apperrors.ErrAssertionFailed
	}

	var upstreamProxyFunc func(*http.Request) (*url.URL, error)
	if r.Config.UpstreamProxy != "" {
		prConfig := httpproxy.Config{
			HTTPProxy:  r.Config.UpstreamProxy,
			HTTPSProxy: r.Config.UpstreamProxy,
		}
		if r.Config.UpstreamNoProxy != "" {
			prConfig.NoProxy = r.Config.UpstreamNoProxy
		}
		upstreamProxyFunc = func(req *http.Request) (*url.URL, error) {
			return prConfig.ProxyFunc()(req.URL)
		}
	}
	upstreamProxy.Tr = &http.Transport{
		Dial:                  dialer,
		Proxy:                 upstreamProxyFunc,
		DisableKeepAlives:     !r.Config.UpstreamKeepalives,
		ExpectContinueTimeout: r.Config.UpstreamExpectContinueTimeout,
		ResponseHeaderTimeout: r.Config.UpstreamResponseHeaderTimeout,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   r.Config.UpstreamTLSHandshakeTimeout,
		MaxIdleConns:          r.Config.MaxIdleConns,
		MaxIdleConnsPerHost:   r.Config.MaxIdleConnsPerHost,
	}

	return nil
}

// createTemplates loads the custom template
func createTemplates(
	logger *zap.Logger,
	signInPage string,
	forbiddenPage string,
	errorPage string,
) *template.Template {
	var list []string
	if signInPage != "" {
		logger.Debug(
			"loading the custom sign in page",
			zap.String("page", signInPage),
		)
		list = append(list, signInPage)
	}

	if forbiddenPage != "" {
		logger.Debug(
			"loading the custom sign forbidden page",
			zap.String("page", forbiddenPage),
		)
		list = append(list, forbiddenPage)
	}

	if errorPage != "" {
		logger.Debug(
			"loading the custom error page",
			zap.String("page", errorPage),
		)
		list = append(list, errorPage)
	}

	if len(list) > 0 {
		logger.Info(
			"loading the custom templates",
			zap.String("templates", strings.Join(list, ",")),
		)

		return template.Must(template.ParseFiles(list...))
	}

	return nil
}

type OpenIDRoundTripper struct {
	http.Header
	rt http.RoundTripper
}

var _ http.RoundTripper = OpenIDRoundTripper{}

func NewOpenIDRoundTripper(rt http.RoundTripper) OpenIDRoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return OpenIDRoundTripper{Header: make(http.Header), rt: rt}
}

func (r OpenIDRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(r.Header) == 0 {
		return r.rt.RoundTrip(req)
	}

	req = req.Clone(req.Context())
	for k, v := range r.Header {
		req.Header[k] = v
	}

	return r.rt.RoundTrip(req)
}

// newOpenIDProvider initializes the openID configuration, note: the redirection url is deliberately left blank
// in order to retrieve it from the host header on request
func (r *OauthProxy) NewOpenIDProvider() (*oidc3.Provider, *gocloak.GoCloak, error) {
	host := fmt.Sprintf(
		"%s://%s",
		r.Config.DiscoveryURI.Scheme,
		r.Config.DiscoveryURI.Host,
	)

	client := gocloak.NewClient(host)

	if r.Config.IsDiscoverURILegacy {
		gocloak.SetLegacyWildFlySupport()(client)
	}

	restyClient := client.RestyClient()
	restyClient.SetTimeout(r.Config.OpenIDProviderTimeout)
	restyClient.SetTLSClientConfig(
		&tls.Config{
			InsecureSkipVerify: r.Config.SkipOpenIDProviderTLSVerify,
		},
	)

	if r.Config.OpenIDProviderProxy != "" {
		restyClient.SetProxy(r.Config.OpenIDProviderProxy)
	}

	httpCl := restyClient.GetClient()
	// This is not nice but currently go-oidc package doesnt provide way to set custom headers
	// https://github.com/coreos/go-oidc/issues/382
	openIDRt := NewOpenIDRoundTripper(httpCl.Transport)
	for k, v := range r.Config.OpenIDProviderHeaders {
		openIDRt.Set(k, v)
	}
	httpCl.Transport = openIDRt

	// see https://github.com/coreos/go-oidc/issues/214
	// see https://github.com/coreos/go-oidc/pull/260
	ctx := oidc3.ClientContext(context.Background(), restyClient.GetClient())
	var provider *oidc3.Provider
	var err error

	operation := func() error {
		provider, err = oidc3.NewProvider(ctx, r.Config.DiscoveryURL)
		if err != nil {
			return err
		}
		return nil
	}

	notify := func(err error, delay time.Duration) {
		r.Log.Warn(
			"problem retrieving oidc config",
			zap.Error(err),
			zap.Duration("retry after", delay),
		)
	}

	bo := backoff.WithMaxRetries(
		backoff.NewExponentialBackOff(),
		uint64(r.Config.OpenIDProviderRetryCount),
	)
	err = backoff.RetryNotify(operation, bo, notify)

	if err != nil {
		return nil,
			nil,
			fmt.Errorf(
				"failed to retrieve the provider configuration from discovery url: %w",
				err,
			)
	}

	return provider, client, nil
}
