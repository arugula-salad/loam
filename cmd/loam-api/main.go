// loam-api is loam's public API (docs/API.md): records in Postgres, sandboxes
// on the nodelets (docs/NODE.md), port URLs under --domain and the projects'
// own --domains, and with --e2b-domain the E2B adapter (docs/E2B.md).
//
// Phase 1 runs exactly one replica: the lifecycle loop and the in-flight
// counts that keep sandboxes awake live in its memory (internal/apiserver).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/arugula-salad/loam/internal/apiserver"
	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/internal/e2b"
	"github.com/arugula-salad/loam/internal/nodeapi"
)

// nodeletFlags is --nodelet name=host:port, repeatable.
type nodeletFlags map[string]string

func (f nodeletFlags) String() string { return fmt.Sprint(map[string]string(f)) }
func (f nodeletFlags) Set(v string) error {
	name, addr, ok := strings.Cut(v, "=")
	if !ok || name == "" || addr == "" {
		return errors.New("want name=host:port")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = nodeapi.Addr(addr)
	}
	f[name] = addr
	return nil
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// secret is the value of envName, or the trimmed contents of file.
func secret(envName, file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return os.Getenv(envName), nil
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("loam-api failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	nodelets := nodeletFlags{}
	listen := flag.String("listen", env("LOAM_LISTEN", ":8080"), "address to serve on (LOAM_LISTEN)")
	metricsListen := flag.String("metrics-listen", env("LOAM_METRICS_LISTEN", ":9090"), "address to serve Prometheus metrics on, /metrics (LOAM_METRICS_LISTEN); never the public listener, since the page names every project's sandboxes; empty serves none")
	accessLog := flag.Bool("access-log", true, "log a line per request to a sandbox's URL")
	domain := flag.String("domain", env("LOAM_DOMAIN", ""), "parent domain of the API (api.<domain>) and the port URLs, <name>--<project>.<domain> (LOAM_DOMAIN); required")
	templatesPath := flag.String("templates", env("LOAM_TEMPLATES", ""), "the templates YAML file (LOAM_TEMPLATES); required")
	domainsPath := flag.String("domains", env("LOAM_DOMAINS", ""), "the projects' URL domains, a YAML or JSON file (LOAM_DOMAINS); none without one")
	registries := flag.String("allowed-registries", env("LOAM_ALLOWED_REGISTRIES", "ghcr.io/arugula-salad"), "comma-separated repository prefixes a sandbox's image may come from (LOAM_ALLOWED_REGISTRIES); empty refuses images")
	dbURL := flag.String("database-url", env("LOAM_DATABASE_URL", ""), "Postgres URL (LOAM_DATABASE_URL); \"memory\" keeps records in memory, for local development only")
	adminFile := flag.String("admin-token-file", "", "file holding the admin token (else LOAM_ADMIN_TOKEN)")
	nodeFile := flag.String("node-token-file", "", "file holding the node token (else LOAM_NODE_TOKEN)")
	secretKeyFile := flag.String("secret-key-file", "", "file holding the key that seals project secrets, 32 bytes in base64 (else LOAM_SECRET_KEY); without one, secrets are unsupported")
	service := flag.String("nodelet-service", env("LOAM_NODELET_SERVICE", "loam-nodelet.loam.svc.cluster.local"), "headless Service of the nodelets, resolved on every lookup (LOAM_NODELET_SERVICE)")
	publicURL := flag.String("public-url", env("LOAM_PUBLIC_URL", ""), "the API's base URL as clients reach it, the identity tokens' issuer (LOAM_PUBLIC_URL); default https://api.<domain>")
	identityKeys := flag.String("identity-keys", env("LOAM_IDENTITY_KEYS", ""), "comma-separated PEM files or directories of RSA keys that sign sandbox identity tokens; the first signs, all are published (LOAM_IDENTITY_KEYS); none leaves identity unsupported")
	e2bDomain := flag.String("e2b-domain", env("LOAM_E2B_DOMAIN", ""), "the E2B adapter's domain, the SDKs' E2B_DOMAIN: api.<it> and <port>-<id>.<it> (LOAM_E2B_DOMAIN; docs/E2B.md); empty serves no E2B")
	e2bTemplate := flag.String("e2b-template", env("LOAM_E2B_TEMPLATE", "e2b"), "the template E2B's base is made from: one whose image runs envd (LOAM_E2B_TEMPLATE)")
	e2bTemplates := flag.String("e2b-templates", env("LOAM_E2B_TEMPLATES", ""), "E2B templates beyond base, name=loam-template[:user],... each made from a template whose image runs envd, user being envd's default (LOAM_E2B_TEMPLATES)")
	e2bMaxTimeout := flag.Duration("e2b-max-timeout", 24*time.Hour, "the longest timeout an E2B sandbox may have")
	flag.Var(nodelets, "nodelet", "a nodelet by hand, name=host:port (repeatable; replaces --nodelet-service; local development)")
	flag.Parse()

	adminToken, err := secret("LOAM_ADMIN_TOKEN", *adminFile)
	if err != nil {
		return err
	}
	nodeToken, err := secret("LOAM_NODE_TOKEN", *nodeFile)
	if err != nil {
		return err
	}
	switch {
	case adminToken == "":
		return errors.New("an admin token is required (LOAM_ADMIN_TOKEN or --admin-token-file)")
	case nodeToken == "":
		return errors.New("a node token is required (LOAM_NODE_TOKEN or --node-token-file)")
	case *domain == "":
		return errors.New("--domain is required (LOAM_DOMAIN): the API is https://api.<domain> and sandboxes are under *.<domain>")
	case *templatesPath == "":
		return errors.New("--templates is required")
	case *dbURL == "":
		return errors.New("a database is required (LOAM_DATABASE_URL)")
	}
	templates, err := apiserver.LoadTemplates(*templatesPath)
	if err != nil {
		return err
	}
	var domains apiserver.Domains
	if *domainsPath != "" {
		if domains, err = apiserver.LoadDomains(*domainsPath, *domain); err != nil {
			return err
		}
	}
	allowed, err := apiserver.ParseAllowedRegistries(*registries)
	if err != nil {
		return fmt.Errorf("--allowed-registries: %w", err)
	}
	if len(allowed) == 0 {
		log.Warn("no allowed registries (--allowed-registries): sandboxes are made from templates only")
	}
	secretKeyText, err := secret("LOAM_SECRET_KEY", *secretKeyFile)
	if err != nil {
		return err
	}
	var secretKey []byte
	if secretKeyText == "" {
		log.Warn("no secret key (LOAM_SECRET_KEY): project secrets are unsupported")
	} else if secretKey, err = apiserver.ParseSecretKey(secretKeyText); err != nil {
		return err
	}
	if *e2bDomain != "" {
		if _, ok := templates[*e2bTemplate]; !ok {
			return fmt.Errorf("--e2b-template %q is not in --templates", *e2bTemplate)
		}
	}
	e2bExtra, err := parseE2BTemplates(*e2bTemplates, templates)
	if err != nil {
		return err
	}
	if *publicURL == "" {
		*publicURL = "https://api." + *domain
	}
	if u, err := url.Parse(*publicURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("--public-url %q: want an http(s) URL with a host and nothing after its path", *publicURL)
	}
	var keyPaths []string
	for _, p := range strings.Split(*identityKeys, ",") {
		if p = strings.TrimSpace(p); p != "" {
			keyPaths = append(keyPaths, p)
		}
	}
	idKeys, err := apiserver.LoadIdentityKeys(keyPaths)
	if err != nil {
		return fmt.Errorf("identity keys: %w", err)
	}
	if len(idKeys) == 0 {
		log.Warn("no identity keys: sandboxes can't get identity tokens (--identity-keys)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var store db.Store
	if *dbURL == "memory" {
		log.Warn("records are kept in memory and lost at exit")
		store = db.NewMemory()
	} else {
		pg, err := db.Open(ctx, *dbURL, nil)
		if err != nil {
			return fmt.Errorf("database: %w", err)
		}
		store = pg
	}
	defer store.Close()

	nodes := &apiserver.Nodes{Token: nodeToken, Service: *service}
	if len(nodelets) > 0 {
		nodes.Static = nodelets
	}
	srv := apiserver.New(apiserver.Options{Store: store, Nodes: nodes, Templates: templates, Domain: *domain,
		URLDomains: domains, AllowedRegistries: allowed,
		AdminToken: adminToken, NodeToken: nodeToken, SecretKey: secretKey, PublicURL: *publicURL, IdentityKeys: idKeys, Log: log,
		NoAccessLog: !*accessLog})
	go srv.Run(ctx)

	var handler http.Handler = srv
	if *e2bDomain != "" {
		// The adapter takes its own hosts and hands every other request on.
		handler = e2b.New(e2b.Options{Domain: *e2bDomain, Template: *e2bTemplate, Templates: e2bExtra, MaxTimeout: *e2bMaxTimeout, Log: log},
			srv, store).Handler(srv)
	}
	hs := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 30 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	var ms *http.Server
	if *metricsListen != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", srv.Metrics())
		ms = &http.Server{Addr: *metricsListen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- ms.ListenAndServe() }()
	}
	log.Info("loam-api serving", "listen", *listen, "domain", *domain, "public_url", *publicURL,
		"templates", len(templates), "url_domains", len(domains), "allowed_registries", allowed, "identity_keys", len(idKeys), "e2b_domain", *e2bDomain, "metrics_listen", *metricsListen)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shut, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = hs.Shutdown(shut)
	// The last requests' traffic, which the flush loop (stopped with ctx) won't write.
	if ferr := srv.FlushTraffic(shut); ferr != nil {
		log.Warn("flushing traffic at shutdown failed", "err", ferr)
	}
	if ms != nil {
		ms.Close()
	}
	return err
}

// parseE2BTemplates reads --e2b-templates, name=loam-template[:user] items
// separated by commas, refusing a name the SDKs couldn't send (base is
// --e2b-template's, and a colon starts a tag) and a loam template that isn't
// in --templates. The user is envd's default for the template; the image
// must have it, which nothing here can check.
func parseE2BTemplates(spec string, templates apiserver.Templates) (map[string]e2b.Template, error) {
	out := map[string]e2b.Template{}
	for _, pair := range strings.Split(spec, ",") {
		if pair = strings.TrimSpace(pair); pair == "" {
			continue
		}
		name, tmpl, ok := strings.Cut(pair, "=")
		tmpl, user, _ := strings.Cut(tmpl, ":")
		switch {
		case !ok || name == "" || tmpl == "":
			return nil, fmt.Errorf("--e2b-templates: %q is not name=template", pair)
		case name == "base" || strings.Contains(name, ":"):
			return nil, fmt.Errorf("--e2b-templates: %q can't be an E2B template's name", name)
		}
		if _, ok := templates[tmpl]; !ok {
			return nil, fmt.Errorf("--e2b-templates: template %q is not in --templates", tmpl)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("--e2b-templates: %q is given twice", name)
		}
		out[name] = e2b.Template{Loam: tmpl, User: user}
	}
	return out, nil
}
