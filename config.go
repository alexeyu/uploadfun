package uploadfun

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type rawConfig struct {
	Endpoints      []rawEndpoint `yaml:"endpoints"`
	Attempts       *int          `yaml:"attempts"`
	RetryDelay     *string       `yaml:"retry_delay"`
	ConnectTimeout *string       `yaml:"connect_timeout"`
	StallTimeout   *string       `yaml:"stall_timeout"`
}

type rawEndpoint struct {
	Name       string `yaml:"name"`
	Protocol   string `yaml:"protocol"`
	Host       string `yaml:"host"`
	Port       int    `yaml:"port"`
	Username   string `yaml:"username"`
	Password   string `yaml:"password"`
	PrivateKey string `yaml:"private_key"`
	Overwrite  string `yaml:"overwrite"`

	Attempts       *int    `yaml:"attempts"`
	RetryDelay     *string `yaml:"retry_delay"`
	ConnectTimeout *string `yaml:"connect_timeout"`
	StallTimeout   *string `yaml:"stall_timeout"`
}

var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// LoadConfig reads and validates a YAML endpoint config, resolving
// ${ENV_VAR} interpolation and global-default/per-endpoint-override
// merging along the way. It validates the whole file and collects every
// error rather than stopping at the first, so a caller can report them
// all at once (see ARCHITECTURE.md "Config format" / Validation).
func LoadConfig(path string) ([]Endpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	var errs []error

	globalAttempts := DefaultAttempts
	if raw.Attempts != nil {
		globalAttempts = *raw.Attempts
		if globalAttempts < 1 {
			errs = append(errs, fmt.Errorf("attempts: must be >= 1, got %d", globalAttempts))
		}
	}
	globalRetryDelay := parseGlobalDuration(raw.RetryDelay, DefaultRetryDelay, "retry_delay", &errs)
	globalConnectTimeout := parseGlobalDuration(raw.ConnectTimeout, DefaultConnectTimeout, "connect_timeout", &errs)
	globalStallTimeout := parseGlobalDuration(raw.StallTimeout, DefaultStallTimeout, "stall_timeout", &errs)

	if len(raw.Endpoints) == 0 {
		errs = append(errs, errors.New("endpoints: at least one endpoint is required"))
	}

	seenNames := make(map[string]bool, len(raw.Endpoints))
	endpoints := make([]Endpoint, 0, len(raw.Endpoints))
	for i, re := range raw.Endpoints {
		label := fmt.Sprintf("endpoints[%d]", i)
		if re.Name != "" {
			label = fmt.Sprintf("endpoint %q", re.Name)
		}
		resolve := func(field, value string) string {
			v, err := interpolateEnv(value)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %s: %w", label, field, err))
			}
			return v
		}

		name := resolve("name", re.Name)
		host := resolve("host", re.Host)
		username := resolve("username", re.Username)
		password := resolve("password", re.Password)
		privateKey := resolve("private_key", re.PrivateKey)
		overwriteRaw := resolve("overwrite", re.Overwrite)

		if name == "" {
			errs = append(errs, fmt.Errorf("%s: name is required", label))
		} else if seenNames[name] {
			errs = append(errs, fmt.Errorf("%s: duplicate endpoint name", label))
		} else {
			seenNames[name] = true
		}

		protocol := Protocol(re.Protocol)
		switch protocol {
		case ProtocolFTP, ProtocolFTPS, ProtocolSFTP:
		case "":
			errs = append(errs, fmt.Errorf("%s: protocol is required", label))
		default:
			errs = append(errs, fmt.Errorf("%s: unknown protocol %q", label, re.Protocol))
		}

		if host == "" {
			errs = append(errs, fmt.Errorf("%s: host is required", label))
		}
		if username == "" {
			errs = append(errs, fmt.Errorf("%s: username is required", label))
		}

		switch protocol {
		case ProtocolFTP, ProtocolFTPS:
			if privateKey != "" {
				errs = append(errs, fmt.Errorf("%s: private_key is not supported for protocol %q", label, protocol))
			}
			if password == "" {
				errs = append(errs, fmt.Errorf("%s: password is required for protocol %q", label, protocol))
			}
		case ProtocolSFTP:
			if password == "" && privateKey == "" {
				errs = append(errs, fmt.Errorf("%s: sftp requires password or private_key", label))
			}
		}

		overwrite := OverwriteDeleteFirst
		if overwriteRaw != "" {
			switch OverwriteMode(overwriteRaw) {
			case OverwriteDeleteFirst, OverwriteDirect:
				overwrite = OverwriteMode(overwriteRaw)
			default:
				errs = append(errs, fmt.Errorf("%s: unknown overwrite mode %q", label, overwriteRaw))
			}
		}

		if privateKey != "" {
			expanded, err := expandHome(privateKey)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: private_key: %w", label, err))
			} else {
				privateKey = expanded
			}
		}

		attempts := globalAttempts
		if re.Attempts != nil {
			attempts = *re.Attempts
			if attempts < 1 {
				errs = append(errs, fmt.Errorf("%s: attempts must be >= 1, got %d", label, attempts))
			}
		}
		retryDelay := parseEndpointDuration(re.RetryDelay, globalRetryDelay, label, "retry_delay", &errs)
		connectTimeout := parseEndpointDuration(re.ConnectTimeout, globalConnectTimeout, label, "connect_timeout", &errs)
		stallTimeout := parseEndpointDuration(re.StallTimeout, globalStallTimeout, label, "stall_timeout", &errs)

		endpoints = append(endpoints, Endpoint{
			Name:           name,
			Protocol:       protocol,
			Host:           host,
			Port:           re.Port,
			Username:       username,
			Password:       password,
			PrivateKey:     privateKey,
			Overwrite:      overwrite,
			Attempts:       attempts,
			RetryDelay:     retryDelay,
			ConnectTimeout: connectTimeout,
			StallTimeout:   stallTimeout,
		})
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return endpoints, nil
}

func parseGlobalDuration(raw *string, def time.Duration, field string, errs *[]error) time.Duration {
	if raw == nil {
		return def
	}
	d, err := time.ParseDuration(*raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", field, err))
		return def
	}
	return d
}

func parseEndpointDuration(raw *string, global time.Duration, label, field string, errs *[]error) time.Duration {
	if raw == nil {
		return global
	}
	d, err := time.ParseDuration(*raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %s: %w", label, field, err))
		return global
	}
	return d
}

// interpolateEnv replaces every ${VAR} occurrence in s with the value of
// the VAR environment variable. It's an error for a referenced variable
// to be unset, so a typo'd or forgotten env var is caught here rather
// than surfacing as a confusing auth failure at upload time.
func interpolateEnv(s string) (string, error) {
	var missing []string
	result := envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := envVarPattern.FindStringSubmatch(match)[1]
		val, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return match
		}
		return val
	})
	if len(missing) > 0 {
		return s, fmt.Errorf("undefined environment variable(s): %s", strings.Join(missing, ", "))
	}
	return result, nil
}

// expandHome expands a leading "~" or "~/" in p to the current user's
// home directory, matching shells' handling of key file paths.
func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}
