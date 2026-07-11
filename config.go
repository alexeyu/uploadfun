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
// all at once.
func LoadConfig(path string) ([]Endpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	globals, errs := resolveGlobals(raw)
	if len(raw.Endpoints) == 0 {
		errs = append(errs, errors.New("endpoints: at least one endpoint is required"))
	}

	seenNames := make(map[string]bool, len(raw.Endpoints))
	endpoints := make([]Endpoint, 0, len(raw.Endpoints))
	for i, re := range raw.Endpoints {
		endpoint, endpointErrs := buildEndpoint(re, i, globals, seenNames)
		errs = append(errs, endpointErrs...)
		endpoints = append(endpoints, endpoint)
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return endpoints, nil
}

// globalDefaults are the config-level attempts/timeout values applied to
// any endpoint that doesn't override them.
type globalDefaults struct {
	attempts       int
	retryDelay     time.Duration
	connectTimeout time.Duration
	stallTimeout   time.Duration
}

func resolveGlobals(raw rawConfig) (globalDefaults, []error) {
	var errs []error
	g := globalDefaults{
		attempts:       DefaultAttempts,
		retryDelay:     DefaultRetryDelay,
		connectTimeout: DefaultConnectTimeout,
		stallTimeout:   DefaultStallTimeout,
	}

	if raw.Attempts != nil {
		g.attempts = *raw.Attempts
		if g.attempts < 1 {
			errs = append(errs, fmt.Errorf("attempts: must be >= 1, got %d", g.attempts))
		}
	}

	var err error
	if g.retryDelay, err = parseDuration(raw.RetryDelay, g.retryDelay); err != nil {
		errs = append(errs, fmt.Errorf("retry_delay: %w", err))
	}
	if g.connectTimeout, err = parseDuration(raw.ConnectTimeout, g.connectTimeout); err != nil {
		errs = append(errs, fmt.Errorf("connect_timeout: %w", err))
	}
	if g.stallTimeout, err = parseDuration(raw.StallTimeout, g.stallTimeout); err != nil {
		errs = append(errs, fmt.Errorf("stall_timeout: %w", err))
	}
	return g, errs
}

// buildEndpoint resolves and validates a single raw endpoint against the
// config's global defaults, recording a duplicate-name check against
// seenNames as a side effect.
func buildEndpoint(
	re rawEndpoint, index int, g globalDefaults, seenNames map[string]bool,
) (Endpoint, []error) {
	var errs []error
	label := endpointLabel(index, re.Name)

	f, fieldErrs := interpolateFields(re, label)
	errs = append(errs, fieldErrs...)
	errs = append(errs, validateName(label, f.name, seenNames)...)

	protocol, protocolErrs := validateProtocol(label, re.Protocol)
	errs = append(errs, protocolErrs...)

	if f.host == "" {
		errs = append(errs, fmt.Errorf("%s: host is required", label))
	}
	if f.username == "" {
		errs = append(errs, fmt.Errorf("%s: username is required", label))
	}
	errs = append(errs, validateAuth(label, protocol, f.password, f.privateKey)...)

	overwrite, overwriteErrs := resolveOverwrite(label, f.overwriteRaw)
	errs = append(errs, overwriteErrs...)

	privateKey, keyErr := resolvePrivateKeyPath(f.privateKey)
	if keyErr != nil {
		errs = append(errs, fmt.Errorf("%s: private_key: %w", label, keyErr))
	}

	attempts, retryDelay, connectTimeout, stallTimeout, durationErrs :=
		resolveEndpointDurations(label, re, g)
	errs = append(errs, durationErrs...)

	endpoint := Endpoint{
		Name:           f.name,
		Protocol:       protocol,
		Host:           f.host,
		Port:           re.Port,
		Username:       f.username,
		Password:       f.password,
		PrivateKey:     privateKey,
		Overwrite:      overwrite,
		Attempts:       attempts,
		RetryDelay:     retryDelay,
		ConnectTimeout: connectTimeout,
		StallTimeout:   stallTimeout,
	}
	return endpoint, errs
}

func endpointLabel(index int, name string) string {
	if name != "" {
		return fmt.Sprintf("endpoint %q", name)
	}
	return fmt.Sprintf("endpoints[%d]", index)
}

// resolvedFields holds an endpoint's string fields after ${ENV_VAR}
// interpolation.
type resolvedFields struct {
	name         string
	host         string
	username     string
	password     string
	privateKey   string
	overwriteRaw string
}

func interpolateFields(re rawEndpoint, label string) (resolvedFields, []error) {
	var errs []error
	resolve := func(field, value string) string {
		v, err := interpolateEnv(value)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %s: %w", label, field, err))
		}
		return v
	}

	f := resolvedFields{
		name:         resolve("name", re.Name),
		host:         resolve("host", re.Host),
		username:     resolve("username", re.Username),
		password:     resolve("password", re.Password),
		privateKey:   resolve("private_key", re.PrivateKey),
		overwriteRaw: resolve("overwrite", re.Overwrite),
	}
	return f, errs
}

func validateName(label, name string, seenNames map[string]bool) []error {
	if name == "" {
		return []error{fmt.Errorf("%s: name is required", label)}
	}
	if seenNames[name] {
		return []error{fmt.Errorf("%s: duplicate endpoint name", label)}
	}
	seenNames[name] = true
	return nil
}

func validateProtocol(label, rawProtocol string) (Protocol, []error) {
	protocol := Protocol(rawProtocol)
	switch protocol {
	case ProtocolFTP, ProtocolFTPS, ProtocolSFTP:
		return protocol, nil
	case "":
		return protocol, []error{fmt.Errorf("%s: protocol is required", label)}
	default:
		return protocol, []error{fmt.Errorf("%s: unknown protocol %q", label, rawProtocol)}
	}
}

// validateAuth checks that the password/private_key combination matches
// what the protocol supports and requires.
func validateAuth(label string, protocol Protocol, password, privateKey string) []error {
	var errs []error
	switch protocol {
	case ProtocolFTP, ProtocolFTPS:
		if privateKey != "" {
			errs = append(errs, fmt.Errorf(
				"%s: private_key is not supported for protocol %q", label, protocol))
		}
		if password == "" {
			errs = append(errs, fmt.Errorf("%s: password is required for protocol %q", label, protocol))
		}
	case ProtocolSFTP:
		if password == "" && privateKey == "" {
			errs = append(errs, fmt.Errorf("%s: sftp requires password or private_key", label))
		}
	}
	return errs
}

func resolveOverwrite(label, raw string) (OverwriteMode, []error) {
	if raw == "" {
		return OverwriteDeleteFirst, nil
	}
	switch OverwriteMode(raw) {
	case OverwriteDeleteFirst, OverwriteDirect:
		return OverwriteMode(raw), nil
	default:
		return OverwriteDeleteFirst, []error{fmt.Errorf("%s: unknown overwrite mode %q", label, raw)}
	}
}

func resolvePrivateKeyPath(privateKey string) (string, error) {
	if privateKey == "" {
		return "", nil
	}
	return expandHome(privateKey)
}

func resolveEndpointDurations(label string, re rawEndpoint, g globalDefaults) (
	attempts int, retryDelay, connectTimeout, stallTimeout time.Duration, errs []error,
) {
	attempts = g.attempts
	if re.Attempts != nil {
		attempts = *re.Attempts
		if attempts < 1 {
			errs = append(errs, fmt.Errorf("%s: attempts must be >= 1, got %d", label, attempts))
		}
	}

	var err error
	if retryDelay, err = parseDuration(re.RetryDelay, g.retryDelay); err != nil {
		errs = append(errs, fmt.Errorf("%s: retry_delay: %w", label, err))
	}
	if connectTimeout, err = parseDuration(re.ConnectTimeout, g.connectTimeout); err != nil {
		errs = append(errs, fmt.Errorf("%s: connect_timeout: %w", label, err))
	}
	if stallTimeout, err = parseDuration(re.StallTimeout, g.stallTimeout); err != nil {
		errs = append(errs, fmt.Errorf("%s: stall_timeout: %w", label, err))
	}
	return attempts, retryDelay, connectTimeout, stallTimeout, errs
}

// parseDuration returns def when raw is unset, otherwise the parsed
// duration or the error from parsing it.
func parseDuration(raw *string, def time.Duration) (time.Duration, error) {
	if raw == nil {
		return def, nil
	}
	return time.ParseDuration(*raw)
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
