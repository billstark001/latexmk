package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"

	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

type credentials struct {
	userDefaultFile                             string
	userFile, projectFile                       string
	userSource, projectSource                   *ValueSource
	lookup                                      func(string) (string, bool)
	envToken, envFile, cliToken, cliFile, start string
	cliSet                                      bool
}

// Load resolves configuration and authenticates against the resolved project root.
// Local-only callers should use LoadArgs to keep credential reads deferred.
func Load(start string) (Resolved, error) {
	cfg, err := load(start, nil)
	if err == nil {
		err = cfg.Authenticate(cfg.ProjectRoot)
	}
	return cfg, err
}

// LoadArgs removes shared credential flags without opening token files. Authenticate
// is called only for network operations, after entry/root and CLI options are resolved.
func LoadArgs(start string, args []string) (Resolved, []string, error) {
	var envFile *string
	var token, tokenFile, mode string
	var hasToken, hasFile bool
	remaining := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			remaining = append(remaining, args[i:]...)
			break
		}
		key, value, inline := strings.Cut(args[i], "=")
		switch key {
		case "--no-env-file":
			empty := ""
			envFile = &empty
		case "--token", "--token-file", "--token-mode", "--env-file":
			if !inline {
				i++
				if i >= len(args) {
					return Resolved{}, nil, fmt.Errorf("%s requires a value", key)
				}
				value = args[i]
			}
			switch key {
			case "--token":
				token = value
				hasToken = true
			case "--token-file":
				tokenFile = value
				hasFile = true
			case "--token-mode":
				mode = value
			case "--env-file":
				envFile = &value
			}
		default:
			remaining = append(remaining, args[i])
		}
	}
	if hasToken && hasFile {
		return Resolved{}, nil, fmt.Errorf("--token and --token-file are mutually exclusive")
	}
	if hasFile && tokenFile == "" {
		return Resolved{}, nil, fmt.Errorf("--token-file requires a nonempty path")
	}
	cfg, err := load(start, envFile)
	if err != nil {
		return cfg, nil, err
	}
	cfg.auth.cliToken, cfg.auth.cliFile, cfg.auth.cliSet = token, tokenFile, hasToken || hasFile
	if mode != "" {
		cfg.TokenMode = mode
	}
	if tokenFile != "" {
		cfg.DenyFiles = append(cfg.DenyFiles, tokenFile)
	}
	return cfg, remaining, nil
}

// Authenticate selects the highest-priority credential for root, opening only
// files needed by that selection. It clears any prior credential before resolving.
func (c *Resolved) Authenticate(root string) error {
	if root == "" {
		root = c.ProjectRoot
	}
	if root == "" && c.ConfigPath != "" {
		root = filepath.Dir(c.ConfigPath)
	}
	if root == "" {
		root = c.auth.start
	}
	a := c.auth
	c.Token, c.TokenSource = "", "none"
	type source struct {
		token, file, name string
		optional          bool
	}
	var candidates []source
	switch c.TokenMode {
	case "", "auto":
		candidates = []source{{a.envToken, "", "LATEXMK_TOKEN", false}, {"", a.envFile, "LATEXMK_TOKEN_FILE", false},
			{"", a.userFile, "user config", false}, {"", a.projectFile, "project config", false},
			{"", filepath.Join(root, TokenFileName), "project token file", true}}
		candidates = append(
			candidates,
			source{"", a.userDefaultFile, "user token file", true},
		)
	case "env":
		candidates = []source{{a.envToken, "", "LATEXMK_TOKEN", false}, {"", a.envFile, "LATEXMK_TOKEN_FILE", false}}
	case "file":
		file := a.envFile
		if file == "" {
			file = a.userFile
		}
		if file == "" {
			file = a.projectFile
		}
		if file == "" {
			file = filepath.Join(root, TokenFileName)
		}
		candidates = []source{{"", file, "token file", false}}
	case "none":
		return nil
	default:
		return fmt.Errorf("tokenMode must be auto, env, file, or none")
	}
	if a.cliSet {
		candidates = []source{{a.cliToken, a.cliFile, "CLI", false}}
	}
	for _, candidate := range candidates {
		var declared *ValueSource
		if candidate.name == "user config" {
			declared = a.userSource
		} else if candidate.name == "project config" {
			declared = a.projectSource
		}
		if declared != nil {
			token, err := declared.resolve("token", a.lookup)
			if err != nil {
				return fmt.Errorf("%s: %w", candidate.name, err)
			}
			return c.setCredential(token, candidate.name)
		}
		if candidate.token != "" {
			return c.setCredential(candidate.token, candidate.name)
		}
		if candidate.file == "" {
			continue
		}
		c.DenyFiles = append(c.DenyFiles, candidate.file)
		if candidate.optional {
			if _, err := os.Stat(candidate.file); os.IsNotExist(err) {
				continue
			}
		}
		token, err := ReadTokenFile(candidate.file)
		if err != nil {
			return fmt.Errorf("%s: %w", candidate.name, err)
		}
		return c.setCredential(token, candidate.name+": "+candidate.file)
	}
	return nil
}

func environmentPath(value, envPath, key string) string {
	if value == "" || filepath.IsAbs(value) {
		return value
	}
	if _, present := os.LookupEnv(key); !present && envPath != "" {
		return filepath.Join(filepath.Dir(envPath), value)
	}
	return value
}

func loadEnvironment(start, configPath string, configured, override *string) (string, map[string]string, error) {
	choice := configured
	if override != nil {
		choice = override
	}
	name := ""
	if choice != nil {
		if *choice == "" {
			return "", nil, nil
		}
		name = *choice
		if !filepath.IsAbs(name) {
			name = filepath.Join(start, name)
		}
	} else {
		dir := start
		if configPath != "" {
			dir = filepath.Dir(configPath)
		}
		for {
			candidate := filepath.Join(dir, EnvFileName)
			if _, err := os.Stat(candidate); err == nil {
				name = candidate
				break
			} else if !os.IsNotExist(err) {
				return "", nil, err
			}
			if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if name == "" {
		return "", nil, nil
	}
	f, err := safefs.OpenRegularFile(name)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = f.Close() }()
	payload, err := safefs.ReadLimited(f, maxTokenFileSize)
	if err != nil {
		return "", nil, fmt.Errorf("invalid environment file %s (maximum %d bytes): %w", name, maxTokenFileSize, err)
	}
	values, err := godotenv.Parse(bytes.NewReader(payload))
	if err != nil {
		return "", nil, fmt.Errorf("parse environment file %s (expected dotenv assignments)", name)
	}
	return name, values, nil
}

func (c *Resolved) setCredential(token, source string) error {
	if !protocol.ValidBearerToken(token) {
		return fmt.Errorf("%s must contain one token without whitespace or control characters", source)
	}
	c.Token, c.TokenSource = token, source
	return nil
}
