package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/billstark001/latexmk/packages/shared/safefs"

	"github.com/joho/godotenv"

	"github.com/billstark001/latexmk/packages/shared/protocol"
)

type credentials struct {
	userDefaultFile                             string
	userFile, projectFile                       string
	userSource, projectSource                   *ValueSource
	lookup                                      func(string) (string, bool)
	envToken, envFile, cliToken, cliFile, start string
	cliSet                                      bool
}

// CredentialOptions contains already parsed command options. Set markers retain
// explicit empty values without exposing argv syntax to configuration loading.
type CredentialOptions struct {
	EnvFile      *string
	Token        string
	TokenFile    string
	TokenMode    string
	TokenSet     bool
	TokenFileSet bool
	TokenModeSet bool
}

func (o CredentialOptions) Validate() error {
	hasToken, hasFile := o.TokenSet || o.Token != "", o.TokenFileSet || o.TokenFile != ""
	if hasToken && hasFile {
		return fmt.Errorf("--token and --token-file are mutually exclusive")
	}
	if hasToken && o.Token == "" {
		return fmt.Errorf("--token requires a nonempty value")
	}
	if hasFile && o.TokenFile == "" {
		return fmt.Errorf("--token-file requires a nonempty path")
	}
	if o.TokenModeSet && o.TokenMode == "" {
		return fmt.Errorf("--token-mode requires a nonempty value")
	}
	return nil
}

// Load resolves configuration without opening credential files. Network callers
// authenticate explicitly after their entry and project root have been resolved.
func Load(start string, options CredentialOptions) (Resolved, error) {
	if err := options.Validate(); err != nil {
		return Resolved{}, err
	}
	cfg, err := load(start, options.EnvFile)
	if err != nil {
		return cfg, err
	}
	cfg.auth.cliToken, cfg.auth.cliFile = options.Token, options.TokenFile
	cfg.auth.cliSet = options.TokenSet || options.TokenFileSet || options.Token != "" || options.TokenFile != ""
	if options.TokenMode != "" {
		cfg.TokenMode = options.TokenMode
	}
	if options.TokenFile != "" {
		cfg.DenyFiles = append(cfg.DenyFiles, options.TokenFile)
	}
	return cfg, nil
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
