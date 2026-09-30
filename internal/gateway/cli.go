package gateway

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const help = `Codex Gateway — a local multi-provider Responses gateway

Usage: codex-gateway [--home DIRECTORY] [COMMAND]

  add-provider  [NAME] [--official] — add a provider, prompting for missing values
  add-model     [MODEL_ID] [--provider NAME] — add a model and prepare its catalog
  (no command)  Start the gateway and open Codex

Advanced commands:
  init          Initialize private configuration (--auth-mode codex|token)
  provider      add NAME --base-url URL --api-key-env NAME | list | remove NAME
  model         add ALIAS --provider NAME --upstream-model ID --template SLUG | list | remove ALIAS
  catalog       import FILE | list | build
  config        show | set KEY VALUE
  validate      Check configuration (--credentials also resolves API keys)
  start         Start or reuse an idle-compatible background gateway
  serve         Run the gateway in the foreground
  status        Inspect this gateway
  stop          Stop an idle gateway
  run           [--codex-bin PATH] [-- CODEX_ARGUMENTS...]

Configuration is independent of Codex's global settings. Interactive API keys
are saved in private files. Use COMMAND --help for options.
`

func newFlags(name string, out io.Writer) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(out)
	return f
}

// Go's flag parser normally stops at the first positional argument. Preserve
// the existing CLI's "provider add NAME --option VALUE" interface.
func parseFlags(f *flag.FlagSet, args []string) ([]string, error) {
	var options, positions []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positions = append(positions, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positions = append(positions, arg)
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name == "h" || name == "help" {
			return nil, f.Parse([]string{"-h"})
		}
		option := f.Lookup(name)
		if option == nil {
			return nil, fmt.Errorf("unknown option --%s", name)
		}
		options = append(options, arg)
		boolean, isBool := option.Value.(interface{ IsBoolFlag() bool })
		if !hasValue && !(isBool && boolean.IsBoolFlag()) {
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--%s requires a value", name)
			}
			options = append(options, args[i])
		}
	}
	return positions, f.Parse(options)
}

func requireArgs(args []string, count int) error {
	if len(args) != count {
		return fmt.Errorf("expected %d positional arguments; see --help", count)
	}
	return nil
}

func printJSON(out io.Writer, value any) error {
	e := json.NewEncoder(out)
	e.SetIndent("", "  ")
	e.SetEscapeHTML(false)
	return e.Encode(value)
}

func Execute(args []string, out, errOut io.Writer) int {
	return ExecuteWithInput(args, os.Stdin, out, errOut)
}

func ExecuteWithInput(args []string, in io.Reader, out, errOut io.Writer) int {
	home := DefaultHome()
	for len(args) > 0 {
		if args[0] == "--help" || args[0] == "-h" {
			fmt.Fprint(out, help)
			return 0
		}
		if args[0] == "--version" {
			fmt.Fprintln(out, Version)
			return 0
		}
		if args[0] == "--home" {
			if len(args) < 2 {
				fmt.Fprintln(errOut, "error: --home requires a directory")
				return 2
			}
			home, args = args[1], args[2:]
			continue
		}
		if strings.HasPrefix(args[0], "--home=") {
			home, args = strings.TrimPrefix(args[0], "--home="), args[1:]
			continue
		}
		break
	}
	if len(args) == 0 {
		args = []string{"run"}
	}
	resolved, err := ResolvePath(home)
	if err != nil {
		fmt.Fprintln(errOut, "error: cannot resolve gateway home")
		return 2
	}
	var code int
	switch args[0] {
	case "add-provider":
		err = addProvider(resolved, args[1:], in, out)
	case "add-model":
		err = addModel(resolved, args[1:], in, out)
	default:
		code, err = dispatch(resolved, args, out, errOut)
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
		return 2
	}
	return code
}

func dispatch(home string, args []string, out, errOut io.Writer) (int, error) {
	command, args := args[0], args[1:]
	switch command {
	case "init":
		f := newFlags("init", out)
		var options InitOptions
		f.StringVar(&options.CodexHome, "codex-home", "", "Codex home directory")
		f.StringVar(&options.AuthMode, "auth-mode", "codex", "codex or token")
		f.IntVar(&options.Port, "port", 33989, "loopback port")
		f.StringVar(&options.CatalogFile, "catalog", "", "Codex model catalog JSON")
		f.StringVar(&options.BootstrapProxy, "bootstrap-proxy", "", "optional HTTP proxy for Codex account bootstrap")
		positions, err := parseFlags(f, args)
		if err != nil {
			return 0, err
		}
		if err := requireArgs(positions, 0); err != nil {
			return 0, err
		}
		if options.Port < 1 || options.Port > 65535 {
			return 0, errors.New("port must be between 1 and 65535")
		}
		if options.CatalogFile != "" {
			options.CatalogFile, err = ResolvePath(options.CatalogFile)
			if err != nil {
				return 0, err
			}
		}
		if _, err := Initialize(home, options); err != nil {
			return 0, err
		}
		return 0, printJSON(out, map[string]any{"home": home, "initialized": true})
	case "run":
		bin := ""
		if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
			fmt.Fprintln(out, "Usage: codex-gateway [--home DIRECTORY] run [--codex-bin PATH] [-- CODEX_ARGUMENTS...]")
			return 0, nil
		}
		if len(args) > 0 && args[0] == "--codex-bin" {
			if len(args) < 2 {
				return 0, errors.New("--codex-bin requires a path")
			}
			bin, args = args[1], args[2:]
		} else if len(args) > 0 && strings.HasPrefix(args[0], "--codex-bin=") {
			bin, args = strings.TrimPrefix(args[0], "--codex-bin="), args[1:]
		}
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		return RunCodex(home, args, bin)
	case "start", "stop", "status", "serve":
		if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
			fmt.Fprintf(out, "Usage: codex-gateway [--home DIRECTORY] %s\n", command)
			return 0, nil
		}
		if err := requireArgs(args, 0); err != nil {
			return 0, err
		}
		if command == "serve" {
			return 0, Serve(home)
		}
		fn := map[string]func(string) (map[string]any, error){"start": Start, "stop": Stop, "status": Status}[command]
		result, err := fn(home)
		if err != nil {
			return 0, err
		}
		return 0, printJSON(out, result)
	case "provider", "model", "catalog", "config", "validate":
		return 0, configure(home, command, args, out)
	default:
		return 0, errors.New("unknown command; use --help")
	}
}

func configure(home, command string, args []string, out io.Writer) error {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--help" || arg == "-h" {
			fmt.Fprint(out, help)
			return nil
		}
	}
	cfg, err := LoadConfig(home)
	if err != nil {
		return err
	}
	if command == "validate" {
		f := newFlags("validate", out)
		credentials := f.Bool("credentials", false, "resolve configured API key references")
		positions, err := parseFlags(f, args)
		if err != nil {
			return err
		}
		if err := requireArgs(positions, 0); err != nil {
			return err
		}
		templates, err := TemplatesFrom(filepath.Join(home, "templates.json"))
		if err != nil {
			return err
		}
		if _, err := CatalogFor(cfg, templates); err != nil {
			return err
		}
		for _, name := range []string{"admin", "client"} {
			if _, err := ReadToken(home, name); err != nil {
				return err
			}
		}
		if *credentials {
			for _, p := range cfg.Providers {
				if p.Auth == "api_key" {
					if _, err := ResolveAPIKey(p); err != nil {
						return err
					}
				}
			}
		}
		return printJSON(out, map[string]any{"valid": true, "providers": len(cfg.Providers), "models": len(cfg.Models)})
	}
	if len(args) == 0 {
		return errors.New("a subcommand is required; use --help")
	}
	action, args := args[0], args[1:]
	switch command {
	case "provider":
		switch action {
		case "list":
			if err := requireArgs(args, 0); err != nil {
				return err
			}
			return printJSON(out, cfg.Providers)
		case "remove":
			if err := requireArgs(args, 1); err != nil {
				return err
			}
			if _, ok := cfg.Providers[args[0]]; !ok {
				return errors.New("unknown provider")
			}
			for _, m := range cfg.Models {
				if m.Provider == args[0] {
					return errors.New("remove this provider's model aliases first")
				}
			}
			delete(cfg.Providers, args[0])
		case "add":
			f := newFlags("provider add NAME", out)
			var p Provider
			f.StringVar(&p.BaseURL, "base-url", "", "Responses-compatible base URL")
			f.StringVar(&p.Auth, "auth", "api_key", "api_key or codex")
			f.StringVar(&p.APIKeyEnv, "api-key-env", "", "environment variable name, never the key")
			f.StringVar(&p.APIKeyFile, "api-key-file", "", "private file containing the key")
			f.StringVar(&p.Proxy, "proxy", "", "explicit HTTP proxy; otherwise connect directly")
			f.BoolVar(&p.AllowInsecureHTTP, "allow-insecure-http", false, "allow remote HTTP on a trusted network")
			replace := f.Bool("replace", false, "replace an existing provider")
			positions, err := parseFlags(f, args)
			if err != nil {
				return err
			}
			if err := requireArgs(positions, 1); err != nil {
				return err
			}
			name := positions[0]
			if _, ok := cfg.Providers[name]; ok && !*replace {
				return errors.New("provider already exists; use --replace")
			}
			if p.APIKeyFile != "" {
				p.APIKeyFile, err = ResolvePath(p.APIKeyFile)
				if err != nil {
					return err
				}
			}
			cfg.Providers[name] = p
		default:
			return errors.New("unknown provider action")
		}
	case "model":
		switch action {
		case "list":
			if err := requireArgs(args, 0); err != nil {
				return err
			}
			return printJSON(out, cfg.Models)
		case "remove":
			if err := requireArgs(args, 1); err != nil {
				return err
			}
			if _, ok := cfg.Models[args[0]]; !ok {
				return errors.New("unknown model")
			}
			delete(cfg.Models, args[0])
			if cfg.DefaultModel == args[0] {
				cfg.DefaultModel = ""
			}
		case "add":
			f := newFlags("model add ALIAS", out)
			var m Model
			f.StringVar(&m.Provider, "provider", "", "configured provider")
			f.StringVar(&m.Model, "upstream-model", "", "upstream model ID")
			f.StringVar(&m.Template, "template", "", "compatible Codex catalog slug")
			f.StringVar(&m.DisplayName, "display-name", "", "display name")
			def, replace := f.Bool("default", false, "make this the default model"), f.Bool("replace", false, "replace existing alias")
			positions, err := parseFlags(f, args)
			if err != nil {
				return err
			}
			if err := requireArgs(positions, 1); err != nil {
				return err
			}
			alias := positions[0]
			if _, ok := cfg.Models[alias]; ok && !*replace {
				return errors.New("model already exists; use --replace")
			}
			cfg.Models[alias] = m
			if *def {
				cfg.DefaultModel = alias
			}
		default:
			return errors.New("unknown model action")
		}
	case "catalog":
		templates, err := TemplatesFrom(filepath.Join(home, "templates.json"))
		if err != nil {
			return err
		}
		switch action {
		case "list":
			if err := requireArgs(args, 0); err != nil {
				return err
			}
			return printJSON(out, templates)
		case "build":
			if err := requireArgs(args, 0); err != nil {
				return err
			}
		case "import":
			if err := requireArgs(args, 1); err != nil {
				return err
			}
			path, err := ResolvePath(args[0])
			if err != nil {
				return err
			}
			incoming, err := TemplatesFrom(path)
			if err != nil {
				return err
			}
			merged := map[string]map[string]any{}
			for _, list := range [][]map[string]any{templates.Models, incoming.Models} {
				for _, m := range list {
					merged[m["slug"].(string)] = m
				}
			}
			names := make([]string, 0, len(merged))
			for name := range merged {
				names = append(names, name)
			}
			sort.Strings(names)
			templates.Models = []map[string]any{}
			for _, name := range names {
				templates.Models = append(templates.Models, merged[name])
			}
			if _, err := CatalogFor(cfg, templates); err != nil {
				return err
			}
			if err := WriteJSON(filepath.Join(home, "templates.json"), templates); err != nil {
				return err
			}
		default:
			return errors.New("unknown catalog action")
		}
	case "config":
		switch action {
		case "show":
			if err := requireArgs(args, 0); err != nil {
				return err
			}
			return printJSON(out, cfg)
		case "set":
			if err := requireArgs(args, 2); err != nil {
				return err
			}
			value := args[1]
			switch args[0] {
			case "listen.port":
				cfg.Listen.Port, err = strconv.Atoi(value)
			case "codex_home":
				cfg.CodexHome, err = ResolvePath(value)
			case "client_auth":
				cfg.ClientAuth = value
			case "default_model":
				cfg.DefaultModel = value
				if value == "null" {
					cfg.DefaultModel = ""
				}
			case "bootstrap_proxy":
				cfg.BootstrapProxy = value
				if value == "null" {
					cfg.BootstrapProxy = ""
				}
			case "retry_invalid_encrypted_reasoning":
				if value != "true" && value != "false" {
					return errors.New("expected true or false")
				}
				cfg.RetryInvalidEncryptedReasoning = value == "true"
			default:
				return errors.New("unsupported configuration key")
			}
			if err != nil {
				return errors.New("invalid configuration value")
			}
		default:
			return errors.New("unknown config action")
		}
	}
	if err := SaveConfig(home, cfg); err != nil {
		return err
	}
	return printJSON(out, map[string]bool{"saved": true})
}
