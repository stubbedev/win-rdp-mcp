package main

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/pflag"
)

var desktopTools = map[string]bool{
	"Click": true, "Type": true, "Move": true, "Scroll": true,
	"Shortcut": true, "Wait": true, "Snapshot": true, "ReconnectSession": true,
}

type controlOptions struct {
	host, user, domain, pass string
	size                     sessionSize
	agentExe                 string
	noAgent                  bool
	debug                    bool
	enabled                  map[string]bool
}

func parseControlOptions(argv []string) (controlOptions, error) {
	fs := pflag.NewFlagSet("win-rdp-mcp control", pflag.ContinueOnError)
	fs.SortFlags = false
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "win-rdp-mcp control %s — drive a Windows target over RDP, expose it as MCP\n\n"+
			"Usage:\n  win-rdp-mcp control --target HOST --user USER [flags]\n\n"+
			"Every setting can instead live in %s (~/.config/win-rdp-mcp/ or the current\n"+
			"directory, or --config); a flag wins over the file. The password comes from --pass-file, $%s,\n"+
			"or password / pass_file under [target] in the file. Flags:\n", Version, configFileName, envTargetPass)
		fmt.Fprint(fs.Output(), fs.FlagUsages())
	}

	var (
		target      = fs.StringP("target", "t", "", "target host or host:port (RDP)")
		user        = fs.StringP("user", "u", "", "RDP username")
		domain      = fs.StringP("domain", "D", "", "RDP domain (blank for a local account)")
		passFile    = fs.StringP("pass-file", "p", "", "file holding the RDP password (else $"+envTargetPass+", else the config file)")
		width       = fs.IntP("width", "W", 1280, "session width in pixels")
		height      = fs.IntP("height", "H", 800, "session height in pixels")
		agentExe    = fs.String("agent-exe", "", "path to the Windows agent binary to push (default: win-rdp-mcp.exe next to this binary, else ./win-rdp-mcp.exe)")
		noAgent     = fs.Bool("no-agent", false, "desktop tools only; do not push the in-session agent")
		configPath  = fs.StringP("config", "c", "", "path to "+configFileName)
		debug       = fs.BoolP("debug", "d", false, "log RDP and bootstrap detail to stderr")
		enableAll   = fs.BoolP("enable-all", "a", false, "enable every tool, including destructive tier 3")
		enableTier3 = fs.BoolP("enable-tier3", "3", false, "enable the destructive tier 3 tools")
		disableT2   = fs.BoolP("disable-tier2", "2", false, "disable the interactive tier 2 tools")
	)
	if err := fs.Parse(argv); err != nil {
		return controlOptions{}, err
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *pflag.Flag) { explicit[f.Name] = true })

	cfg, err := loadConfig(discoverConfigPath(*configPath))
	if err != nil {
		return controlOptions{}, err
	}
	t := cfg.Target
	inFile := func(key string) bool { return cfg.defined("target", key) }

	opts := controlOptions{
		host:     pick(inFile("host"), t.Host, "", explicit["target"], *target),
		user:     pick(inFile("user"), t.User, "", explicit["user"], *user),
		domain:   pick(inFile("domain"), t.Domain, "", explicit["domain"], *domain),
		agentExe: pick(inFile("agent_exe"), expandUser(t.AgentExe), "", explicit["agent-exe"], *agentExe),
		noAgent:  pickBool(inFile("no_agent"), t.NoAgent, explicit["no-agent"], *noAgent),
		debug:    *debug,
	}
	if opts.host == "" || opts.user == "" {
		return controlOptions{}, fmt.Errorf("no target: pass --target and --user, or set host and user under [target] in ~/.config/win-rdp-mcp/%s", configFileName)
	}
	if opts.pass, err = resolvePassword(*passFile, t); err != nil {
		return controlOptions{}, err
	}

	w, h := *width, *height
	if inFile("width") && !explicit["width"] {
		w = t.Width
	}
	if inFile("height") && !explicit["height"] {
		h = t.Height
	}
	if opts.size, err = newSessionSize(w, h); err != nil {
		return controlOptions{}, err
	}

	opts.enabled, err = resolveEnabledTools(toolSelection{
		enableTier3:  pickBool(cfg.defined("security", "enable_tier3"), cfg.Security.EnableTier3, explicit["enable-tier3"], *enableTier3),
		disableTier2: pickBool(cfg.defined("security", "disable_tier2"), cfg.Security.DisableTier2, explicit["disable-tier2"], *disableT2),
		enableAll:    *enableAll,
		explicit:     cfg.Tools.Enable,
		exclude:      cfg.Tools.Exclude,
	})
	return opts, err
}

func controlMain(argv []string) error {
	opts, err := parseControlOptions(argv)
	if errors.Is(err, pflag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: rdpLogLevel(opts.debug)})))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sess := &rdpSession{
		host: withDefaultPort(opts.host), user: opts.user, domain: opts.domain, pass: opts.pass,
		size: opts.size, debug: opts.debug, life: ctx,
	}
	defer sess.stop()

	var proxy *agentProxy
	if !opts.noAgent {
		exe := opts.agentExe
		if exe == "" {
			exe = defaultAgentExe()
		}
		if err := checkAgentExe(exe); err != nil {
			return err
		}
		proxy, err = newAgentProxy(sess, exe, opts.enabled)
		if err != nil {
			return err
		}
		sess.agentChannel = proxy.link
		sess.onDisconnect = proxy.link.dropAll
		sess.onFirstConnect = func(ctx context.Context) {
			if _, err := proxy.connection(ctx); err != nil {
				logf("agent start failed (desktop tools still work): %v", err)
			}
		}
	}

	srv := mcp.NewServer(
		&mcp.Implementation{Name: "win-rdp-mcp-control", Version: Version},
		&mcp.ServerOptions{Instructions: controllerInstructions(opts.noAgent)},
	)
	if err := registerControllerTools(srv, opts.enabled, sess, proxy); err != nil {
		return err
	}

	logf("controller ready — %d tools (%d desktop-local, rest via agent); RDP connects to %s on the first tool call",
		len(opts.enabled), countLocal(opts.enabled), sess.host)
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func rdpLogLevel(debug bool) slog.Level {
	if debug {
		return slog.LevelInfo
	}
	return slog.LevelError
}

const envTargetPass = "WIN_RDP_TARGET_PASS"

func resolvePassword(flagFile string, t TargetConfig) (string, error) {
	switch {
	case flagFile != "":
		return readPasswordFile(flagFile)
	case os.Getenv(envTargetPass) != "":
		return os.Getenv(envTargetPass), nil
	case t.Password != "":
		return t.Password, nil
	case t.PassFile != "":
		return readPasswordFile(expandUser(t.PassFile))
	}
	return "", fmt.Errorf("no password: pass --pass-file, set $%s, or set password or pass_file under [target] in %s", envTargetPass, configFileName)
}

func readPasswordFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the RDP password file: %w", err)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

func defaultAgentExe() string {
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), "win-rdp-mcp.exe")
		if fileExists(cand) {
			return cand
		}
	}
	return "win-rdp-mcp.exe"
}

func checkAgentExe(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("the Windows agent to push was not found at %s: %w (build it with `just build-windows`, pass --agent-exe, or run with --no-agent for desktop tools only)", path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("the Windows agent path %s is not a regular file", path)
	}
	return nil
}

func countLocal(enabled map[string]bool) int {
	n := 0
	for name := range enabled {
		if desktopTools[name] {
			n++
		}
	}
	return n
}

func registerControllerTools(srv *mcp.Server, enabled map[string]bool, sess *rdpSession, proxy *agentProxy) error {
	specs, err := loadToolSpecs()
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if !enabled[spec.Name] {
			continue
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(spec.InputSchema, &schema); err != nil {
			return fmt.Errorf("tool %s: %w", spec.Name, err)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			return fmt.Errorf("tool %s: %w", spec.Name, err)
		}
		tool := &mcp.Tool{
			Name: spec.Name, Description: spec.Description,
			InputSchema: &schema, Annotations: annotationsFor(spec.Name),
		}
		srv.AddTool(tool, routeTool(spec.Name, resolved, sess, proxy))
	}
	return nil
}

func routeTool(name string, schema *jsonschema.Resolved, sess *rdpSession, proxy *agentProxy) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArguments(req.Params.Arguments)
		if err != nil {
			return errorResult("Invalid arguments: " + err.Error()), nil
		}
		if err := schema.Validate(map[string]any(args)); err != nil {
			return errorResult("Invalid arguments: " + err.Error()), nil
		}

		if desktopTools[name] {
			result, err := localDesktop(ctx, name, sess, proxy, args)
			if err != nil {
				return errorResult(name + " error: " + err.Error()), nil
			}
			return toCallResult(result), nil
		}

		if proxy == nil {
			return errorResult(name + " needs the in-session agent, which is disabled (--no-agent)."), nil
		}
		if (name == "FileUpload" || name == "FileDownload") && args.has("local_path") {
			result, err := proxy.transfer(ctx, name, args)
			if err != nil {
				return errorResult(name + " error: " + err.Error()), nil
			}
			return toCallResult(result), nil
		}
		result, err := proxy.call(ctx, name, map[string]any(args))
		if err != nil {
			return errorResult(name + " error: " + err.Error()), nil
		}
		if name == "GetSystemInfo" && len(result.Content) > 0 && result.Content[0].Type == "text" {
			result.Content[0].Text = insertAfterTaskID(result.Content[0].Text, controllerSummary(sess, proxy)+"\n")
		}
		return toCallResult(result), nil
	}
}

func localDesktop(ctx context.Context, name string, sess *rdpSession, proxy *agentProxy, args arguments) (toolResult, error) {
	switch name {
	case "Snapshot":
		return sess.snapshot(ctx, args, proxy)
	case "Wait":
		return waitTool(ctx, args)
	case "ReconnectSession":
		return sess.reconnect(ctx, args.boolOr("force", false))
	case "Click":
		at, err := clickTarget(args, func(index int) (image.Point, error) {
			if proxy == nil {
				return image.Point{}, errors.New("clicking an element needs the in-session agent, which is disabled (--no-agent); use x and y")
			}
			conn, err := proxy.connection(ctx)
			if err != nil {
				return image.Point{}, err
			}
			return conn.elementCenter(ctx, index)
		})
		if err != nil {
			return toolResult{}, err
		}
		return sess.withInput(ctx, func(in *desktopInput) (toolResult, error) { return in.click(at, args) })
	}
	return sess.withInput(ctx, func(in *desktopInput) (toolResult, error) {
		switch name {
		case "Type":
			return in.typeText(ctx, args)
		case "Move":
			return in.move(ctx, args)
		case "Scroll":
			return in.scroll(args)
		case "Shortcut":
			return in.shortcut(args)
		}
		return toolResult{}, fmt.Errorf("no local handler for %s", name)
	})
}

func insertAfterTaskID(text, insert string) string {
	if strings.HasPrefix(text, "[task:") {
		if end := strings.Index(text, "] "); end >= 0 {
			return text[:end+2] + insert + text[end+2:]
		}
	}
	return insert + text
}

func controllerSummary(sess *rdpSession, proxy *agentProxy) string {
	return fmt.Sprintf("**Controller:** win-rdp-mcp %s, RDP to %s as %s (%dx%d), agent build %s",
		Version, sess.host, sess.user, sess.size.width, sess.size.height, proxy.link.build)
}

func controllerInstructions(noAgent bool) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }
	w("# win-rdp-mcp (controller)")
	w("")
	w("Drives a Windows target over RDP. You are given only RDP credentials; the desktop is driven directly and the system agent is pushed into the session automatically.")
	w("")
	w("- Start with `Snapshot` to see the screen. Coordinates are screen pixels at the session resolution.")
	w("- `Click`, `Type`, `Move`, `Scroll`, `Shortcut` drive the desktop with no footprint on the target.")
	w("- `Snapshot` waits for the screen to settle after your last action, so no `Wait` is needed between an action and a `Snapshot`.")
	w("- `Type` sends real key presses. Apps with autocomplete or text suggestions can still change what lands; check with `Snapshot` when it matters.")
	w("- If another client takes the session, the next call reconnects on its own; `ReconnectSession` with `force` reconnects on demand.")
	if !noAgent {
		w("- The window/element list, `Shell`, files, registry, services and the rest come from an agent pushed into the session over RDP. The first such call takes about ten seconds while it starts; if it dies it is relaunched on the next call.")
	} else {
		w("- Running with --no-agent: only the desktop tools above are available.")
	}
	return strings.TrimRight(b.String(), "\n")
}

func firstText(blocks []contentBlock) string {
	for _, b := range blocks {
		if b.Type == "text" {
			return b.Text
		}
	}
	return ""
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
