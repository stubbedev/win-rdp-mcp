package main

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// tasks is the process-wide task manager: it enforces the per-category
// concurrency limits and backs the CancelTask / GetTaskStatus tools.
var tasks = newTaskManager()

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[win-rdp-mcp] "+format+"\n", args...)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		logf("%v", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	// Subcommands come before flag parsing so `win-rdp-mcp install` does not
	// have to look like a server invocation.
	if len(argv) > 0 {
		switch argv[0] {
		case "install":
			return installScheduledTask()
		case "uninstall":
			return uninstallScheduledTask()
		case "health":
			out, err := json.Marshal(map[string]string{"status": "ok", "version": Version})
			if err != nil {
				return err
			}
			fmt.Println(string(out))
			return nil
		}
	}

	opts, err := parseOptions(argv)
	if err != nil {
		return err
	}
	return serve(opts)
}

// ── Options ──────────────────────────────────────────────────────────────────

type options struct {
	transport string
	host      string
	port      int
	debug     bool

	authKey             string
	allowInsecureRemote bool
	sslCertFile         string
	sslKeyFile          string
	oauthClientID       string
	oauthClientSecret   string

	ipAllowlist []netip.Prefix
	enabled     map[string]bool

	configPath string
}

// useOAuth reports whether the OAuth endpoints should be mounted at all.
func (o options) useOAuth() bool { return o.oauthClientID != "" && o.oauthClientSecret != "" }

func (o options) useTLS() bool { return o.sslCertFile != "" && o.sslKeyFile != "" }

func (o options) authenticated() bool { return o.authKey != "" || o.useOAuth() }

const (
	envAuthKey           = "WIN_RDP_MCP_AUTH_KEY"
	envOAuthClientID     = "WIN_RDP_MCP_OAUTH_CLIENT_ID"
	envOAuthClientSecret = "WIN_RDP_MCP_OAUTH_CLIENT_SECRET"
)

// parseOptions resolves every setting through the same precedence chain:
// built-in default, then the config file, then the environment, then an
// explicitly passed flag.
func parseOptions(argv []string) (options, error) {
	fs := flag.NewFlagSet("win-rdp-mcp", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "win-rdp-mcp %s — control a Windows desktop over MCP\n\n"+
			"Usage:\n  win-rdp-mcp [flags]\n  win-rdp-mcp install | uninstall | health\n\nFlags:\n", Version)
		fs.PrintDefaults()
	}

	var (
		transport           = fs.String("transport", "streamable-http", "transport: stdio or streamable-http")
		host                = fs.String("host", "127.0.0.1", "bind address (0.0.0.0 exposes the server to the network)")
		port                = fs.Int("port", 8090, "bind port")
		debug               = fs.Bool("debug", false, "log every request")
		configPath          = fs.String("config", "", "path to "+configFileName)
		authKey             = fs.String("auth-key", "", "API key clients must send as `Authorization: Bearer <key>`")
		allowInsecureRemote = fs.Bool("allow-insecure-remote", false, "allow a non-loopback bind with no authentication (dangerous)")
		sslCertFile         = fs.String("ssl-certfile", "", "TLS certificate file; enables HTTPS together with -ssl-keyfile")
		sslKeyFile          = fs.String("ssl-keyfile", "", "TLS private key file")
		oauthClientID       = fs.String("oauth-client-id", "", "pre-provisioned OAuth client ID")
		oauthClientSecret   = fs.String("oauth-client-secret", "", "pre-provisioned OAuth client secret")
		enableAll           = fs.Bool("enable-all", false, "enable every tool, including the destructive tier 3")
		enableTier3         = fs.Bool("enable-tier3", false, "enable the destructive tier 3 tools")
		disableTier2        = fs.Bool("disable-tier2", false, "disable the interactive tier 2 tools")
		toolsFlag           = fs.String("tools", "", "comma-separated tools to enable (highest precedence)")
		excludeTools        = fs.String("exclude-tools", "", "comma-separated tools to disable")
		ipAllowlist         = fs.String("ip-allowlist", "", "comma-separated IPs/CIDRs allowed to connect")
	)
	if err := fs.Parse(argv); err != nil {
		return options{}, err
	}

	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	cfg, err := loadConfig(discoverConfigPath(*configPath))
	if err != nil {
		return options{}, err
	}

	opts := options{
		transport:  *transport,
		host:       *host,
		port:       *port,
		debug:      *debug,
		configPath: cfg.SourcePath,
	}

	// Strings and numbers: config first, then env, then flag.
	if cfg.defined("server", "host") && !explicit["host"] {
		opts.host = cfg.Server.Host
	}
	if cfg.defined("server", "port") && !explicit["port"] {
		opts.port = cfg.Server.Port
	}

	opts.authKey = pick(cfg.defined("server", "auth_key"), cfg.Server.AuthKey,
		os.Getenv(envAuthKey), explicit["auth-key"], *authKey)
	opts.oauthClientID = pick(cfg.defined("security", "oauth_client_id"), cfg.Security.OAuthClientID,
		os.Getenv(envOAuthClientID), explicit["oauth-client-id"], *oauthClientID)
	opts.oauthClientSecret = pick(cfg.defined("security", "oauth_client_secret"), cfg.Security.OAuthClientSecret,
		os.Getenv(envOAuthClientSecret), explicit["oauth-client-secret"], *oauthClientSecret)
	opts.sslCertFile = pick(cfg.defined("server", "ssl_certfile"), cfg.Server.SSLCertFile, "", explicit["ssl-certfile"], *sslCertFile)
	opts.sslKeyFile = pick(cfg.defined("server", "ssl_keyfile"), cfg.Server.SSLKeyFile, "", explicit["ssl-keyfile"], *sslKeyFile)

	opts.allowInsecureRemote = pickBool(cfg.defined("server", "allow_insecure_remote"),
		cfg.Server.AllowInsecureRemote, explicit["allow-insecure-remote"], *allowInsecureRemote)
	tier3On := pickBool(cfg.defined("security", "enable_tier3"), cfg.Security.EnableTier3, explicit["enable-tier3"], *enableTier3)
	tier2Off := pickBool(cfg.defined("security", "disable_tier2"), cfg.Security.DisableTier2, explicit["disable-tier2"], *disableTier2)

	selected := cfg.Tools.Enable
	if explicit["tools"] {
		selected = parseToolCSV(*toolsFlag)
	}
	excluded := cfg.Tools.Exclude
	if explicit["exclude-tools"] {
		excluded = parseToolCSV(*excludeTools)
	}
	allowlistEntries := cfg.Security.IPAllowlist
	if explicit["ip-allowlist"] {
		allowlistEntries = parseToolCSV(*ipAllowlist)
	}

	if opts.ipAllowlist, err = parseIPAllowlist(allowlistEntries); err != nil {
		return options{}, err
	}
	if opts.enabled, err = resolveEnabledTools(toolSelection{
		enableTier3:  tier3On,
		disableTier2: tier2Off,
		enableAll:    *enableAll,
		explicit:     selected,
		exclude:      excluded,
	}); err != nil {
		return options{}, err
	}

	return opts, validateOptions(opts)
}

// pick applies the precedence chain for one string setting.
func pick(configSet bool, configValue, envValue string, flagSet bool, flagValue string) string {
	value := ""
	if configSet {
		value = configValue
	}
	if envValue != "" {
		value = envValue
	}
	if flagSet {
		value = flagValue
	}
	return value
}

func pickBool(configSet, configValue, flagSet, flagValue bool) bool {
	value := false
	if configSet {
		value = configValue
	}
	if flagSet {
		value = flagValue
	}
	return value
}

// validateOptions refuses the configurations that would publish this machine's
// desktop — and, with tier 3, a remote shell — to anyone who can reach the port.
func validateOptions(opts options) error {
	switch opts.transport {
	case "stdio", "streamable-http":
	default:
		return fmt.Errorf("unknown transport %q (use stdio or streamable-http)", opts.transport)
	}

	if (opts.oauthClientID == "") != (opts.oauthClientSecret == "") {
		return errors.New("OAuth requires both -oauth-client-id and -oauth-client-secret")
	}
	if opts.useTLS() != (opts.sslCertFile != "" || opts.sslKeyFile != "") {
		return errors.New("HTTPS requires both -ssl-certfile and -ssl-keyfile")
	}

	// stdio has no listener, so none of the network gates below apply.
	if opts.transport == "stdio" || isLoopbackBindHost(opts.host) || opts.authenticated() {
		return nil
	}

	if tier3 := exposedTier3(opts.enabled); len(tier3) > 0 {
		// Tier 3 holds Shell, FileWrite and RegWrite. On an unauthenticated,
		// non-loopback bind that is published pre-auth remote code execution,
		// so -allow-insecure-remote does not cover it: that flag is for
		// read-and-interact access on a trusted LAN.
		return fmt.Errorf("refusing to expose tier 3 tools (%s) on a non-loopback bind without authentication. "+
			"Add -auth-key, configure OAuth, bind to 127.0.0.1, or drop -enable-tier3/-enable-all",
			strings.Join(tier3[:min(len(tier3), 5)], ", "))
	}
	if !opts.allowInsecureRemote {
		return errors.New("refusing to bind a non-loopback address without authentication. " +
			"Use -auth-key, bind to 127.0.0.1, or pass -allow-insecure-remote to accept the risk")
	}
	return nil
}

// ── Serving ──────────────────────────────────────────────────────────────────

func serve(opts options) error {
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "win-rdp-mcp", Version: Version},
		&mcp.ServerOptions{Instructions: instructions(opts)},
	)
	if err := registerTools(srv, opts.enabled); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if opts.transport == "stdio" {
		if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
			return err
		}
		return nil
	}
	return serveHTTP(ctx, srv, opts)
}

const sessionTTL = 30 * time.Minute

func serveHTTP(ctx context.Context, srv *mcp.Server, opts options) error {
	mux := http.NewServeMux()

	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{SessionTimeout: sessionTTL},
	)
	// Clients disagree about the trailing slash, and a 404 there looks like the
	// server is down rather than misaddressed.
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/mcp/", mcpHandler)

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": Version})
	})

	var validateOAuth func(string) bool
	if opts.useOAuth() {
		scheme := "http"
		if opts.useTLS() {
			scheme = "https"
		}
		store := newOAuthStore(fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(opts.host, strconv.Itoa(opts.port))),
			opts.oauthClientID, opts.oauthClientSecret)
		store.register(mux)
		validateOAuth = store.validateToken
	}

	// Order matters: the allowlist runs outermost so a blocked address never
	// reaches the token comparison at all.
	var handler http.Handler = mux
	if opts.debug {
		handler = logRequests(handler)
	}
	if opts.authenticated() {
		handler = authMiddleware(opts.authKey, validateOAuth, handler)
	}
	if len(opts.ipAllowlist) > 0 {
		handler = ipAllowlistMiddleware(opts.ipAllowlist, handler)
	}

	addr := net.JoinHostPort(opts.host, strconv.Itoa(opts.port))
	httpSrv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	printBanner(opts, addr)

	var err error
	if opts.useTLS() {
		err = httpSrv.ListenAndServeTLS(opts.sslCertFile, opts.sslKeyFile)
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logf("%s %s from %s (%s)", r.Method, r.URL.Path, r.RemoteAddr, time.Since(start).Round(time.Millisecond))
	})
}

// printBanner summarises the security posture on stderr, because "is this open
// to the network?" is the question an operator needs answered at a glance.
func printBanner(opts options, addr string) {
	scheme := "http"
	if opts.useTLS() {
		scheme = "https"
	}
	auth := "no auth"
	if opts.authKey != "" && opts.useOAuth() {
		auth = "auth key + oauth"
	} else if opts.authKey != "" {
		auth = "auth key"
	} else if opts.useOAuth() {
		auth = "oauth"
	}

	logf("win-rdp-mcp %s listening on %s://%s/mcp", Version, scheme, addr)
	logf("  %s | tiers %s | %d/%d tools", auth, strings.Join(tierNames(opts.enabled), ","), len(opts.enabled), len(allTools))
	if opts.configPath != "" {
		logf("  config: %s", opts.configPath)
	}
	if len(opts.ipAllowlist) > 0 {
		logf("  ip allowlist: %d network(s)", len(opts.ipAllowlist))
	}
	if !isLoopbackBindHost(opts.host) && !opts.authenticated() {
		logf("  WARNING: open to the network with no authentication — use -auth-key")
	}
}

// instructions is the server-level guidance the client shows the model.
func instructions(opts options) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }

	w("# win-rdp-mcp")
	w("")
	w("Control this Windows machine: screenshots, keyboard and mouse, PowerShell, files, services, registry, scheduled tasks, event log and network checks.")
	w("")
	w("## How to drive the desktop")
	w("- Start with `Snapshot` to see the screen, the window list and the foreground window's controls. `AnnotatedSnapshot` numbers those controls on the image, which is the easier target for a click.")
	w("- Coordinates are virtual-screen pixels, the same space the snapshot reports — do not rescale them yourself.")
	w("- `FocusWindow` before typing into an application; keystrokes go to whatever holds focus.")
	w("- A black or failed screenshot usually means the session is disconnected. `ReconnectSession` attaches it to the console; Snapshot already retries once behind it.")
	w("")
	w("## Concurrency")
	w("- Desktop tools hold an exclusive lock, so only one runs at a time. Results carry a `[task:id]` prefix; `GetTaskStatus`, `GetRunningTasks` and `CancelTask` take that id.")
	w("")

	if tier3 := exposedTier3(opts.enabled); len(tier3) > 0 {
		w("## Destructive tools are enabled")
		w("- " + strings.Join(tier3, ", "))
		w("- Do not run these without an explicit instruction from the user. `Shell` executes arbitrary PowerShell; `RegWrite`, `FileWrite`, `ServiceStop`, `KillProcess` and `TaskDelete` change the machine.")
	} else {
		w("## Destructive tools are disabled")
		w("- Shell, file writes, registry writes and service control are not available. Tell the user to start the server with -enable-tier3 if they want them.")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ── install / uninstall ──────────────────────────────────────────────────────

const scheduledTaskName = "WinRdpMCP"

// installScheduledTask registers the server to start at boot. It runs as the
// logged-on user, not SYSTEM: a SYSTEM session has no desktop to screenshot.
func installScheduledTask() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating this binary: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}

	user := os.Getenv("USERNAME")
	if user == "" {
		user = os.Getenv("USER")
	}
	args := []string{"/Create", "/SC", "ONSTART", "/TN", scheduledTaskName, "/TR", `"` + exe + `"`, "/F"}
	if user != "" {
		args = append(args, "/RU", user)
	}

	out, err := exec.Command("schtasks", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("creating scheduled task: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("Scheduled task %q created; win-rdp-mcp will start at boot.\n", scheduledTaskName)
	return nil
}

func uninstallScheduledTask() error {
	out, err := exec.Command("schtasks", "/Delete", "/TN", scheduledTaskName, "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("removing scheduled task: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("Scheduled task %q removed.\n", scheduledTaskName)
	return nil
}
