package server

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/didip/tollbooth/v6"
	"github.com/didip/tollbooth_chi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/go-pkgz/auth"
	"github.com/go-pkgz/auth/token"
	"github.com/gorilla/handlers"
	"github.com/pkg/errors"
	"github.com/zebox/registry-admin/app/registry"
	"github.com/zebox/registry-admin/app/store"
	"github.com/zebox/registry-admin/app/store/engine"
	"github.com/zebox/registry-admin/app/store/service"

	log "github.com/go-pkgz/lgr"
	R "github.com/go-pkgz/rest"
)

// Server the main service instance
type Server struct {
	Hostname                 string
	Listen                   string // listen on host:port scope
	Port                     int    // main service port, default 80 on
	SSLConfig                SSLConfig
	Authenticator            *auth.Service     // portal access authenticator
	AccessLog                io.Writer         // access logger
	L                        log.L             // system logger
	Storage                  engine.Interface  // main storage instance interface
	RegistryService          registryInterface // main instance for connection to registry service
	GarbageCollectorInterval int64
	WebContentFS             *embed.FS

	ctx         context.Context
	httpsServer *http.Server
	httpServer  *http.Server
	lock        sync.Mutex
}

// endpointsHandler contain main endpoints properties for used inside handlers
type endpointsHandler struct {
	dataStore     engine.Interface
	authenticator *auth.Service
	l             log.L
	ctx           context.Context // pass global context
}

// registryInterface implement method for access data of a registry instance
type registryInterface interface {

	// Login is initials login step when docker login command call
	Login(user store.User) (string, error)

	// Token will create jwt for make a request to registry service when auth token is using
	Token(authRequest registry.TokenRequest) (string, error)

	// ParseAuthenticateHeaderRequest will parse 'Www-Authenticate' header for extract token authorization data.
	ParseAuthenticateHeaderRequest(headerValue string) (authRequest registry.TokenRequest, err error)

	// UpdateHtpasswd update user access list in .htpasswd file every time when users entries add/update/delete
	UpdateHtpasswd(usersFn registry.FetchUsers) error

	// APIVersionCheck a minimal endpoint, mounted at /v2/ will provide version support information
	// based on its response statuses.
	APIVersionCheck(ctx context.Context) error

	// Catalog return list a set of available repositories in the local registry cluster.
	Catalog(ctx context.Context, n, last string) (registry.Repositories, error)

	// ListingImageTags retrieve information about tags.
	ListingImageTags(ctx context.Context, repoName, n, last string) (registry.ImageTags, error)

	// Manifest will fetch the manifest identified by 'name' and 'reference' where 'reference' can be a tag or digest.
	Manifest(ctx context.Context, repoName, tag string) (registry.ManifestSchemaV2, error)

	// GetBlob retrieve information about image from config blob
	GetBlob(ctx context.Context, name, digest string) (blob []byte, err error)

	// DeleteTag will deleteDigest the manifest identified by name and reference. Note that a manifest can only be deleted
	// by digest.
	DeleteTag(ctx context.Context, repoName, digest string) error
}

// responseMessage is the uniform response message pattern for various frontend framework like react-admin and other
type responseMessage struct {
	Error   bool        `json:"error"`
	Message string      `json:"message"`
	ID      int64       `json:"id"`
	Data    interface{} `json:"data"`
}

// Run start main server instance and apply it parameters
func (s *Server) Run(ctx context.Context) error {

	s.ctx = ctx

	if s.Listen == "*" {
		s.Listen = ""
	}

	if s.RegistryService == nil {
		return errors.New("a registry service define required ")
	}

	switch s.SSLConfig.SSLMode {
	case SSLNone:
		log.Printf("[INFO] activate http rest server on %s:%d", s.Listen, s.Port)

		s.lock.Lock()
		s.httpServer = s.makeHTTPServer(fmt.Sprintf("%s:%d", s.Listen, s.Port), s.routes())
		s.httpServer.ErrorLog = log.ToStdLogger(log.Default(), "WARN")
		s.lock.Unlock()

		return s.httpServer.ListenAndServe()

	case SSLStatic:
		log.Printf("[INFO] activate https server in 'static' mode on %s:%d", s.Listen, s.SSLConfig.Port)

		s.lock.Lock()
		s.httpsServer = s.makeHTTPSServer(fmt.Sprintf("%s:%d", s.Listen, s.SSLConfig.Port), s.routes())
		s.httpsServer.ErrorLog = log.ToStdLogger(log.Default(), "WARN")

		// define redirection from http -> https
		s.httpServer = s.makeHTTPServer(fmt.Sprintf("%s:%d", s.Listen, s.Port), s.httpToHTTPSRouter())
		s.httpServer.ErrorLog = log.ToStdLogger(log.Default(), "WARN")
		s.lock.Unlock()

		go func() {
			log.Printf("[INFO] activate http redirect server on %s:%d", s.Listen, s.Port)
			err := s.httpServer.ListenAndServe()
			log.Printf("[WARN] http redirect server terminated, %s", err)
		}()

		return s.httpsServer.ListenAndServeTLS(s.SSLConfig.Cert, s.SSLConfig.Key)

	case SSLAuto:
		log.Printf("[INFO] activate https server in 'auto' mode on %s:%d", s.Listen, s.SSLConfig.Port)

		m := s.makeAutocertManager()
		s.lock.Lock()
		s.httpsServer = s.makeHTTPSAutocertServer(fmt.Sprintf("%s:%d", s.Listen, s.SSLConfig.Port), s.routes(), m)
		s.httpsServer.ErrorLog = log.ToStdLogger(log.Default(), "WARN")

		// define redirection handler for ACME challenge verification
		s.httpServer = s.makeHTTPServer(fmt.Sprintf("%s:%d", s.Listen, s.Port), s.httpChallengeRouter(m))
		s.httpServer.ErrorLog = log.ToStdLogger(log.Default(), "WARN")

		s.lock.Unlock()

		go func() {
			log.Printf("[INFO] activate http challenge server on port %d", s.Port)

			err := s.httpServer.ListenAndServe()
			log.Printf("[WARN] http challenge server terminated, %s", err)
		}()

		return s.httpsServer.ListenAndServeTLS("", "")
	}

	return nil
}

// Shutdown http server instance
func (s *Server) Shutdown() {
	log.Print("[WARN] shutdown rest server")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.lock.Lock()
	if s.httpServer != nil {
		if err := s.httpServer.Shutdown(ctx); err != nil {
			log.Printf("[DEBUG] http shutdown error, %s", err)
		}
		log.Print("[DEBUG] shutdown http server completed")
	}

	if s.httpsServer != nil {
		log.Print("[WARN] shutdown https server")
		if err := s.httpsServer.Shutdown(ctx); err != nil {
			log.Printf("[DEBUG] https shutdown error, %s", err)
		}
		log.Print("[DEBUG] shutdown https server completed")
	}

	if err := s.Storage.Close(ctx); err != nil {
		log.Print("[ERROR] failed to close storage connection")
	}
	s.lock.Unlock()
}

func (s *Server) routes() chi.Router {
	router := chi.NewRouter()

	router.Use(middleware.Throttle(1000), middleware.RealIP, R.Recoverer(log.Default()))
	router.Use(middleware.Timeout(30 * time.Second))
	router.Use(R.Ping)

	corsMiddleware := cors.New(cors.Options{
		AllowedOrigins:   []string{s.Hostname, os.Getenv("RA_DEV_HOST")},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Origin", "Accept", "Authorization", "Content-Type", "X-XSRF-Token", "X-JWT"},
		ExposedHeaders:   []string{"Authorization"},
		AllowCredentials: true,
		MaxAge:           300,
	})
	router.Use(corsMiddleware.Handler)
	router.Use(accessLogHandler(s.AccessLog))

	authHandler, _ := s.Authenticator.Handlers()
	authMiddleware := s.Authenticator.Middleware()

	router.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(5 * time.Second))
		r.Use(tollbooth_chi.LimitHandler(tollbooth.NewLimiter(10, nil)), middleware.NoCache)
		r.Mount("/auth", authHandler)
	})

	// initialing main endpoints properties for use in handlers
	eh := endpointsHandler{
		dataStore:     s.Storage,
		authenticator: s.Authenticator,
		l:             s.L,
		ctx:           s.ctx,
	}

	// main endpoints routes
	router.Route("/api/v1", func(rootApi chi.Router) {
		rootApi.Group(func(rootRoute chi.Router) {
			rootRoute.Use(tollbooth_chi.LimitHandler(tollbooth.NewLimiter(10, nil)))
			rootRoute.Use(authMiddleware.Trace, middleware.NoCache)

			uh := userHandlers{
				endpointsHandler: eh,
				registryService:  s.RegistryService,
				userAdapter:      newUsersRegistryAdapter(s.ctx, engine.QueryFilter{}, s.Storage.FindUsers),
			}

			// try to update users list in htpasswd from store if htpasswd is defined
			if err := uh.registryService.UpdateHtpasswd(uh.userAdapter); err != nil {
				panic(fmt.Errorf("failed to update htpasswd: %v", err))
			}

			// this route expose api for manipulation with User entries
			rootRoute.Route("/users", func(routeUser chi.Router) {
				routeUser.Use(authMiddleware.Auth, middleware.NoCache)
				routeUser.Use(authMiddleware.RBAC("admin", "manager"))

				routeUser.Get("/{id}", uh.userInfoCtrl)
				routeUser.Get("/", uh.userFindCtrl)

				// operation create/update/deleteDigest with User items allow for admin only
				routeUser.Group(func(routeAdminUser chi.Router) {
					routeAdminUser.Use(authMiddleware.RBAC("admin"))

					routeAdminUser.Post("/", uh.userCreateCtrl)
					routeAdminUser.Put("/{id}", uh.userUpdateCtrl)
					routeAdminUser.Delete("/{id}", uh.userDeleteCtrl)
				})
			})

			// this route expose api for manipulation with Group items
			gh := groupHandlers{eh}
			rootRoute.Route("/groups", func(routeGroup chi.Router) {
				routeGroup.Use(authMiddleware.Auth, middleware.NoCache)
				routeGroup.Use(authMiddleware.RBAC("admin", "manager"))

				routeGroup.Get("/{id}", gh.groupInfoCtrl)
				routeGroup.Get("/", gh.groupFindCtrl)

				// operation create/update/deleteDigest with Group items allow for admins only
				routeGroup.Group(func(routeAdminGroup chi.Router) {
					routeAdminGroup.Use(authMiddleware.RBAC("admin"))

					routeAdminGroup.Post("/", gh.groupCreateCtrl)
					routeAdminGroup.Put("/{id}", gh.groupUpdateCtrl)
					routeAdminGroup.Delete("/{id}", gh.groupDeleteCtrl)
				})
			})

			// this route expose api for manipulation with Access items
			ah := accessHandlers{eh}
			rootRoute.Route("/access", func(routeAccess chi.Router) {
				routeAccess.Use(authMiddleware.Auth, middleware.NoCache)
				routeAccess.Use(authMiddleware.RBAC("admin", "manager"))

				routeAccess.Get("/{id}", ah.accessInfoCtrl)
				routeAccess.Get("/", ah.accessFindCtrl)

				// operation create/update/deleteDigest with Access items allow for admins only
				routeAccess.Group(func(routeAdminAccess chi.Router) {
					routeAdminAccess.Use(authMiddleware.RBAC("admin"))
					routeAdminAccess.Post("/", ah.accessAddCtrl)
					routeAdminAccess.Put("/{id}", ah.accessUpdateCtrl)
					routeAdminAccess.Delete("/{id}", ah.accessDeleteCtrl)
				})
			})

			// this route expose api for manipulation with Registry service entries
			rh := registryHandlers{
				endpointsHandler: eh,
				registryService:  s.RegistryService,
				dataService: &service.DataService{
					Registry: s.RegistryService,
					Storage:  s.Storage,
				},
			}

			// starting Data Service maintenance tasks such as garbage collector and repositories auto sync
			rh.dataService.RepositoriesMaintenance(s.ctx, s.GarbageCollectorInterval)

			// route API for manipulations registry entries (catalog/tags/manifest/deleteDigest)
			rootRoute.Route("/registry", func(routeRegistry chi.Router) {
				routeRegistry.Get("/auth", rh.tokenAuth)

				routeRegistry.Group(func(registryApiEventsRegistry chi.Router) {
					registryApiEventsRegistry.Use(authMiddleware.Auth, middleware.NoCache)
					registryApiEventsRegistry.Post("/events", rh.events)
					registryApiEventsRegistry.Get("/health", rh.health)
				})

				routeRegistry.Group(func(routeApiRegistry chi.Router) {

					// allows any users list repositories, but user role can see allowed repositories only
					routeApiRegistry.Use(authMiddleware.Auth, middleware.NoCache)
					routeApiRegistry.Get("/catalog", rh.catalogList)
				})

				routeRegistry.Group(func(routeApiManagerRegistry chi.Router) {
					routeApiManagerRegistry.Use(authMiddleware.Auth, middleware.NoCache)
					routeApiManagerRegistry.Use(authMiddleware.RBAC("admin", "manager"))
					routeApiManagerRegistry.Get("/catalog/blobs", rh.imageConfig)
				})

				routeRegistry.Group(func(routeApiAdminRegistry chi.Router) {
					routeApiAdminRegistry.Use(authMiddleware.RBAC("admin"))
					routeApiAdminRegistry.Get("/sync", rh.syncRepositories)
					routeApiAdminRegistry.Delete("/catalog/*", rh.deleteDigest)
				})
			})
		})
	})

	// serving static content with web UI (html,js,css etc.)
	s.serveStaticWeb(router, "/", "web")

	return router
}

// accessLogHandler the handler will log all request for access to the server
func accessLogHandler(wr io.Writer) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return handlers.CombinedLoggingHandler(wr, next)
	}
}

func (s *Server) makeHTTPServer(addr string, router http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
}

// serves static files from /web or embedded by static
func (s *Server) serveStaticWeb(r chi.Router, path, rootWebDir string) {
	var webFS http.Handler
	webDirFS, err := fs.Sub(s.WebContentFS, rootWebDir)
	if err != nil {
		panic(fmt.Errorf("%v: failed to read web directory from embed FS", err))
	}

	webFS = http.StripPrefix(path, http.FileServer(http.FS(webDirFS)))

	if path != "/" && path[len(path)-1] != '/' {
		r.Get(path, http.RedirectHandler(path+"/", http.StatusMovedPermanently).ServeHTTP)
		path += "/"
	}
	path += "*"

	r.Get(path, func(w http.ResponseWriter, req *http.Request) {
		contentPath := rootWebDir + req.URL.Path

		// This required for support with SPA routing (e.g. react-router), otherwise server return page with 404 error
		// If a file is found, it will be served. If not, the file located at the index path
		// on the SPA handler will be served.
		// This is suitable behavior for serving an SPA (single page application).
		if _, err = s.WebContentFS.Open(contentPath); err != nil && req.URL.Path != "/" {
			tpl, errTpl := template.ParseFS(s.WebContentFS, rootWebDir+"/index.html")
			if errTpl != nil {
				s.L.Logf("[ERROR] page %s not found in pages cache...", req.RequestURI)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			if err = tpl.Execute(w, nil); err != nil {
				s.L.Logf("[ERROR] failed to execute template for SPA router redirect %v", err)
				return
			}
			return
		}
		webFS.ServeHTTP(w, req)
	})

}

// ClaimUpdateFn will either add or update token extra data with token claims it call when new token or refresh
func (s *Server) ClaimUpdateFn(claims token.Claims) token.Claims {

	u, err := s.Storage.GetUser(s.ctx, claims.User.Name)
	if err != nil {
		log.Printf("[ERROR] can't get user info from store %v", err)
		return claims
	}

	claims.User.SetRole(u.Role)
	if claims.User.Attributes == nil {
		claims.User.Attributes = make(map[string]interface{})
	}

	claims.User.SetBoolAttr("disabled", u.Disabled)
	claims.User.Attributes["uid"] = u.ID
	return claims
}

// BasicAuthCheckerFn will be checking credentials with basic authenticate method
func (s *Server) BasicAuthCheckerFn(user, password string) (bool, token.User, error) {
	claim := token.User{}

	u, err := s.Storage.GetUser(s.ctx, user)
	if err != nil {
		log.Printf("[WARN] failed to check login credentials for user [%s]  err: %v", user, err)
		return false, claim, nil
	}

	if u.Disabled {
		return false, claim, errors.Errorf("User with login '%s' disabled", user)
	}

	ok := store.ComparePassword(u.Password, password)

	if !ok {
		return false, claim, nil

	}

	claim.Name = u.Name
	claim.SetRole(u.Role)

	if claim.Attributes == nil {
		claim.Attributes = make(map[string]interface{})
	}
	claim.Attributes["uid"] = u.ID
	claim.ID = strconv.FormatInt(u.ID, 10)
	return true, claim, nil
}

// Check will be checking user credentials with OAuth method
// It's method pass when add auth local provider
func (s *Server) Check(user, password string) (ok bool, err error) {
	ok, _, err = s.BasicAuthCheckerFn(user, password)
	return ok, err
}

// Validate will validate token claims for OAuth provider
func (s *Server) Validate(_ string, claims token.Claims) bool {
	if claims.User == nil {
		return false
	}
	return !claims.User.BoolAttr("disabled")
}
