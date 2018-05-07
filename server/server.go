package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	argocd "github.com/argoproj/argo-cd"
	"github.com/argoproj/argo-cd/common"
	"github.com/argoproj/argo-cd/errors"
	"github.com/argoproj/argo-cd/pkg/apiclient"
	appclientset "github.com/argoproj/argo-cd/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-cd/reposerver"
	"github.com/argoproj/argo-cd/server/application"
	"github.com/argoproj/argo-cd/server/cluster"
	"github.com/argoproj/argo-cd/server/repository"
	"github.com/argoproj/argo-cd/server/session"
	"github.com/argoproj/argo-cd/server/settings"
	"github.com/argoproj/argo-cd/server/version"
	dexutil "github.com/argoproj/argo-cd/util/dex"
	grpc_util "github.com/argoproj/argo-cd/util/grpc"
	jsonutil "github.com/argoproj/argo-cd/util/json"
	util_session "github.com/argoproj/argo-cd/util/session"
	settings_util "github.com/argoproj/argo-cd/util/settings"
	tlsutil "github.com/argoproj/argo-cd/util/tls"
	"github.com/argoproj/argo-cd/util/webhook"
	golang_proto "github.com/golang/protobuf/proto"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_auth "github.com/grpc-ecosystem/go-grpc-middleware/auth"
	grpc_logrus "github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	"github.com/grpc-ecosystem/grpc-gateway/runtime"
	log "github.com/sirupsen/logrus"
	"github.com/soheilhy/cmux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	port = 8080
)

var (
	endpoint = fmt.Sprintf("localhost:%d", port)
	// ErrNoSession indicates no auth token was supplied as part of a request
	ErrNoSession = status.Errorf(codes.Unauthenticated, "no session information")
	// ErrInvalidSession indicates that specified token is invalid
	ErrInvalidSession = status.Errorf(codes.Unauthenticated, "invalid session information")
)

// ArgoCDServer is the API server for ArgoCD
type ArgoCDServer struct {
	ArgoCDServerOpts

	ssoClientApp *dexutil.ClientApp
	settings     settings_util.ArgoCDSettings
	log          *log.Entry
	sessionMgr   *util_session.SessionManager
	settingsMgr  *settings_util.SettingsManager
}

type ArgoCDServerOpts struct {
	DisableAuth     bool
	Insecure        bool
	Namespace       string
	StaticAssetsDir string
	KubeClientset   kubernetes.Interface
	AppClientset    appclientset.Interface
	RepoClientset   reposerver.Clientset
}

// NewServer returns a new instance of the ArgoCD API server
func NewServer(opts ArgoCDServerOpts) *ArgoCDServer {
	settingsMgr := settings_util.NewSettingsManager(opts.KubeClientset, opts.Namespace)
	settings, err := settingsMgr.GetSettings()
	if err != nil {
		log.Fatal(err)
	}
	sessionMgr := util_session.MakeSessionManager(settings.ServerSignature)
	return &ArgoCDServer{
		ArgoCDServerOpts: opts,
		log:              log.NewEntry(log.New()),
		settings:         *settings,
		sessionMgr:       &sessionMgr,
		settingsMgr:      settingsMgr,
	}
}

// Run runs the API Server
// We use k8s.io/code-generator/cmd/go-to-protobuf to generate the .proto files from the API types.
// k8s.io/ go-to-protobuf uses protoc-gen-gogo, which comes from gogo/protobuf (a fork of
// golang/protobuf).
func (a *ArgoCDServer) Run() {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	grpcS := a.newGRPCServer()
	var httpS *http.Server
	var httpsS *http.Server
	if a.useTLS() {
		httpS = newRedirectServer()
		httpsS = a.newHTTPServer(ctx)
	} else {
		httpS = a.newHTTPServer(ctx)
	}

	// Cmux is used to support servicing gRPC and HTTP1.1+JSON on the same port
	conn, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	errors.CheckError(err)

	tcpm := cmux.New(conn)
	var tlsm cmux.CMux
	var grpcL net.Listener
	var httpL net.Listener
	var httpsL net.Listener
	if !a.useTLS() {
		httpL = tcpm.Match(cmux.HTTP1Fast())
		grpcL = tcpm.Match(cmux.HTTP2HeaderField("content-type", "application/grpc"))
	} else {
		// We first match on HTTP 1.1 methods.
		httpL = tcpm.Match(cmux.HTTP1Fast())

		// If not matched, we assume that its TLS.
		tlsl := tcpm.Match(cmux.Any())
		tlsConfig := tls.Config{
			Certificates: []tls.Certificate{*a.settings.Certificate},
		}
		tlsl = tls.NewListener(tlsl, &tlsConfig)

		// Now, we build another mux recursively to match HTTPS and gRPC.
		tlsm = cmux.New(tlsl)
		httpsL = tlsm.Match(cmux.HTTP1Fast())
		grpcL = tlsm.Match(cmux.Any())
	}

	// Start the muxed listeners for our servers
	log.Infof("argocd %s serving on port %d (url: %s, tls: %v, namespace: %s, sso: %v)",
		argocd.GetVersion(), port, a.settings.URL, a.useTLS(), a.Namespace, a.settings.IsSSOConfigured())
	go func() { errors.CheckError(grpcS.Serve(grpcL)) }()
	go func() { errors.CheckError(httpS.Serve(httpL)) }()
	if a.useTLS() {
		go func() { errors.CheckError(httpsS.Serve(httpsL)) }()
		go func() { errors.CheckError(tlsm.Serve()) }()
	}
	go a.initializeOIDCClientApp()
	err = tcpm.Serve()
	errors.CheckError(err)
}

// initializeOIDCClientApp initializes the OIDC Client application, querying the well known oidc
// configuration path. Because ArgoCD is a OIDC client to itself, we have a chicken-and-egg problem
// of (1) serving dex over HTTP, and (2) querying the OIDC provider (ourselves) to initialize the
// app (HTTP GET http://example-argocd.com/api/dex/.well-known/openid-configuration)
// This method is expected to be invoked right after we start listening over HTTP
func (a *ArgoCDServer) initializeOIDCClientApp() {
	if !a.settings.IsSSOConfigured() {
		return
	}
	// wait for dex to become ready
	dexClient, err := dexutil.NewDexClient()
	errors.CheckError(err)
	dexClient.WaitUntilReady()
	var backoff = wait.Backoff{
		Steps:    5,
		Duration: 1 * time.Second,
		Factor:   1.0,
		Jitter:   0.1,
	}
	var realErr error
	_ = wait.ExponentialBackoff(backoff, func() (bool, error) {
		realErr = a.ssoClientApp.Initialize()
		if realErr != nil {
			a.log.Warnf("failed to initialize client app: %v", realErr)
			return false, nil
		}
		return true, nil
	})
	errors.CheckError(realErr)
}

func (a *ArgoCDServer) useTLS() bool {
	if a.Insecure || a.settings.Certificate == nil {
		return false
	}
	return true
}

func (a *ArgoCDServer) newGRPCServer() *grpc.Server {
	var sOpts []grpc.ServerOption
	// NOTE: notice we do not configure the gRPC server here with TLS (e.g. grpc.Creds(creds))
	// This is because TLS handshaking occurs in cmux handling
	sOpts = append(sOpts, grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(
		grpc_logrus.StreamServerInterceptor(a.log),
		grpc_auth.StreamServerInterceptor(a.authenticate),
		grpc_util.ErrorCodeStreamServerInterceptor(),
		grpc_util.PanicLoggerStreamServerInterceptor(a.log),
	)))
	sOpts = append(sOpts, grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
		grpc_logrus.UnaryServerInterceptor(a.log),
		grpc_auth.UnaryServerInterceptor(a.authenticate),
		grpc_util.ErrorCodeUnaryServerInterceptor(),
		grpc_util.PanicLoggerUnaryServerInterceptor(a.log),
	)))

	grpcS := grpc.NewServer(sOpts...)
	clusterService := cluster.NewServer(a.Namespace, a.KubeClientset, a.AppClientset)
	repoService := repository.NewServer(a.Namespace, a.KubeClientset, a.AppClientset)
	sessionService := session.NewServer(a.settings)
	applicationService := application.NewServer(a.Namespace, a.KubeClientset, a.AppClientset, a.RepoClientset, repoService, clusterService)
	settingsService := settings.NewServer(a.settingsMgr)
	version.RegisterVersionServiceServer(grpcS, &version.Server{})
	cluster.RegisterClusterServiceServer(grpcS, clusterService)
	application.RegisterApplicationServiceServer(grpcS, applicationService)
	repository.RegisterRepositoryServiceServer(grpcS, repoService)
	session.RegisterSessionServiceServer(grpcS, sessionService)
	settings.RegisterSettingsServiceServer(grpcS, settingsService)

	// Register reflection service on gRPC server.
	reflection.Register(grpcS)
	return grpcS
}

// TranslateGrpcCookieHeader conditionally sets a cookie on the response.
func (a *ArgoCDServer) translateGrpcCookieHeader(ctx context.Context, w http.ResponseWriter, resp golang_proto.Message) error {
	if sessionResp, ok := resp.(*session.SessionResponse); ok {
		flags := []string{"path=/"}
		if !a.Insecure {
			flags = append(flags, "Secure")
		}
		cookie := util_session.MakeCookieMetadata(common.AuthCookieName, sessionResp.Token, flags...)
		w.Header().Set("Set-Cookie", cookie)
	}
	return nil
}

// newHTTPServer returns the HTTP server to serve HTTP/HTTPS requests. This is implemented
// using grpc-gateway as a proxy to the gRPC server.
func (a *ArgoCDServer) newHTTPServer(ctx context.Context) *http.Server {
	mux := http.NewServeMux()
	httpS := http.Server{
		Addr:    endpoint,
		Handler: mux,
	}
	var dOpts []grpc.DialOption
	if a.useTLS() {
		// The following sets up the dial Options for grpc-gateway to talk to gRPC server over TLS.
		// grpc-gateway is just translating HTTP/HTTPS requests as gRPC requests over localhost,
		// so we need to supply the same certificates to establish the connections that a normal,
		// external gRPC client would need.
		tlsConfig := a.selfTLSConfig()
		tlsConfig.InsecureSkipVerify = true
		dCreds := credentials.NewTLS(tlsConfig)
		dOpts = append(dOpts, grpc.WithTransportCredentials(dCreds))
	} else {
		dOpts = append(dOpts, grpc.WithInsecure())
	}

	// HTTP 1.1+JSON Server
	// grpc-ecosystem/grpc-gateway is used to proxy HTTP requests to the corresponding gRPC call
	// NOTE: if a marshaller option is not supplied, grpc-gateway will default to the jsonpb from
	// golang/protobuf. Which does not support types such as time.Time. gogo/protobuf does support
	// time.Time, but does not support custom UnmarshalJSON() and MarshalJSON() methods. Therefore
	// we use our own Marshaler
	gwMuxOpts := runtime.WithMarshalerOption(runtime.MIMEWildcard, new(jsonutil.JSONMarshaler))
	gwCookieOpts := runtime.WithForwardResponseOption(a.translateGrpcCookieHeader)
	gwmux := runtime.NewServeMux(gwMuxOpts, gwCookieOpts)
	mux.Handle("/api/", gwmux)
	mustRegisterGWHandler(version.RegisterVersionServiceHandlerFromEndpoint, ctx, gwmux, endpoint, dOpts)
	mustRegisterGWHandler(cluster.RegisterClusterServiceHandlerFromEndpoint, ctx, gwmux, endpoint, dOpts)
	mustRegisterGWHandler(application.RegisterApplicationServiceHandlerFromEndpoint, ctx, gwmux, endpoint, dOpts)
	mustRegisterGWHandler(repository.RegisterRepositoryServiceHandlerFromEndpoint, ctx, gwmux, endpoint, dOpts)
	mustRegisterGWHandler(session.RegisterSessionServiceHandlerFromEndpoint, ctx, gwmux, endpoint, dOpts)
	mustRegisterGWHandler(settings.RegisterSettingsServiceHandlerFromEndpoint, ctx, gwmux, endpoint, dOpts)

	// Dex reverse proxy and client app and OAuth2 login/callback
	a.registerDexHandlers(mux)

	// Webhook handler for git events
	acdWebhookHandler := webhook.NewHandler(a.Namespace, a.AppClientset, a.settings)
	mux.HandleFunc("/api/webhook", acdWebhookHandler.Handler)

	if a.StaticAssetsDir != "" {
		mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
			acceptHTML := false
			for _, acceptType := range strings.Split(request.Header.Get("Accept"), ",") {
				if acceptType == "text/html" || acceptType == "html" {
					acceptHTML = true
					break
				}
			}
			fileRequest := request.URL.Path != "/index.html" && strings.Contains(request.URL.Path, ".")

			// serve index.html for non file requests to support HTML5 History API
			if acceptHTML && !fileRequest && (request.Method == "GET" || request.Method == "HEAD") {
				http.ServeFile(writer, request, a.StaticAssetsDir+"/index.html")
			} else {
				http.ServeFile(writer, request, a.StaticAssetsDir+request.URL.Path)
			}
		})
	}
	return &httpS
}

// selfTLSConfig returns a tls.Config with the configured certificates
func (a *ArgoCDServer) selfTLSConfig() *tls.Config {
	certPool := x509.NewCertPool()
	pemCertBytes, _ := tlsutil.EncodeX509KeyPair(*a.settings.Certificate)
	ok := certPool.AppendCertsFromPEM(pemCertBytes)
	if !ok {
		panic("bad certs")
	}
	return &tls.Config{
		RootCAs: certPool,
	}
}

// registerDexHandlers will register dex HTTP handlers, creating the the OAuth client app
func (a *ArgoCDServer) registerDexHandlers(mux *http.ServeMux) {
	if !a.settings.IsSSOConfigured() {
		return
	}
	// Run dex OpenID Connect Identity Provider behind a reverse proxy (served at /api/dex)
	var err error
	mux.HandleFunc(dexutil.DexAPIEndpoint+"/", dexutil.NewDexHTTPReverseProxy())
	tlsConfig := a.selfTLSConfig()
	tlsConfig.InsecureSkipVerify = true
	a.ssoClientApp, err = dexutil.NewClientApp(a.settings.URL, a.settings.ServerSignature, tlsConfig)
	errors.CheckError(err)
	mux.HandleFunc(dexutil.LoginEndpoint, a.ssoClientApp.HandleLogin)
	mux.HandleFunc(dexutil.CallbackEndpoint, a.ssoClientApp.HandleCallback)
}

// newRedirectServer returns an HTTP server which does a 307 redirect to the HTTPS server
func newRedirectServer() *http.Server {
	return &http.Server{
		Addr: endpoint,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			target := "https://" + req.Host + req.URL.Path
			if len(req.URL.RawQuery) > 0 {
				target += "?" + req.URL.RawQuery
			}
			http.Redirect(w, req, target, http.StatusTemporaryRedirect)
		}),
	}
}

type registerFunc func(ctx context.Context, mux *runtime.ServeMux, endpoint string, opts []grpc.DialOption) error

// mustRegisterGWHandler is a convenience function to register a gateway handler
func mustRegisterGWHandler(register registerFunc, ctx context.Context, mux *runtime.ServeMux, endpoint string, opts []grpc.DialOption) {
	err := register(ctx, mux, endpoint, opts)
	if err != nil {
		panic(err)
	}
}

// Authenticate checks for the presence of a valid token when accessing server-side resources.
func (a *ArgoCDServer) authenticate(ctx context.Context) (context.Context, error) {
	if a.DisableAuth {
		return ctx, nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx, ErrNoSession
	}
	token := getToken(md)
	if token == "" {
		return ctx, ErrNoSession
	}
	_, err := a.sessionMgr.Parse(token)
	if err != nil {
		return ctx, ErrInvalidSession
	}
	// TODO: when we care about user groups, we will want to put the claims into the context
	return ctx, nil
}

// getToken extracts the token from gRPC metadata or cookie headers
func getToken(md metadata.MD) string {
	// check the "token" metadata
	tokens, ok := md[apiclient.MetaDataTokenKey]
	if ok && len(tokens) > 0 {
		return tokens[0]
	}
	// check the legacy key (v0.3.2 and below). 'tokens' was renamed to 'token'
	tokens, ok = md["tokens"]
	if ok && len(tokens) > 0 {
		return tokens[0]
	}
	// check the HTTP cookie
	for _, cookieToken := range md["grpcgateway-cookie"] {
		tokenPair := strings.SplitN(cookieToken, "=", 2)
		if len(tokenPair) == 2 && tokenPair[0] == common.AuthCookieName {
			return tokenPair[1]
		}
	}
	return ""
}