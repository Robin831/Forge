package kiln

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/template"
)

// probePort is the placeholder port used when a manifest's templates are
// validated at load time, before any real port has been allocated. It is a
// valid (non-zero) port so expansion exercises the same code path a real start
// does.
const probePort = 1

// Context carries the values a manifest's templates expand against: the
// allocated port per service, the sanitized preview id (safe for database
// names) and the host preview URLs are built from.
type Context struct {
	// PreviewID is the sanitized bead id identifying this preview.
	PreviewID string
	// Host is the hostname used in URLs handed to services and operators
	// (settings.preview_public_host, falling back to preview_bind_host).
	Host string
	// Ports maps service name to its allocated port. Every service in the
	// manifest must have an entry.
	Ports map[string]int
}

// Expand returns a copy of the manifest with `{{.Port}}`, `{{.ServicePort
// "name"}}`, `{{.PreviewID}}` and `{{.Host}}` resolved in the setup/teardown
// commands and in each service's command and env values. The receiver is not
// modified.
//
// `{{.Port}}` resolves to the port of the service being expanded, so it is
// only available inside a service; setup and teardown must name a service
// explicitly via `{{.ServicePort "name"}}`. Referencing a service the manifest
// does not declare is an error rather than an empty string, so a typo fails
// loudly instead of producing a silently broken command line.
func (m *Manifest) Expand(ctx Context) (*Manifest, error) {
	out := &Manifest{
		Version:  m.Version,
		Setup:    m.Setup,
		Teardown: m.Teardown,
		Path:     m.Path,
		Services: make(Services, len(m.Services)),
	}

	global := templateData{ctx: ctx}
	var err error
	if out.Setup, err = expandString("setup", m.Setup, global); err != nil {
		return nil, err
	}
	if out.Teardown, err = expandString("teardown", m.Teardown, global); err != nil {
		return nil, err
	}

	for i, svc := range m.Services {
		data := templateData{ctx: ctx, service: svc.Name}
		expanded := svc
		if expanded.Command, err = expandString(fmt.Sprintf("service %q: command", svc.Name), svc.Command, data); err != nil {
			return nil, err
		}
		if len(svc.Env) > 0 {
			expanded.Env = make(map[string]string, len(svc.Env))
			for _, key := range sortedKeys(svc.Env) {
				value, err := expandString(fmt.Sprintf("service %q: env %q", svc.Name, key), svc.Env[key], data)
				if err != nil {
					return nil, err
				}
				expanded.Env[key] = value
			}
		}
		out.Services[i] = expanded
	}
	return out, nil
}

// templateData is the value manifest templates execute against. Every variable
// is a method so an unknown one fails at execution instead of expanding to the
// empty string.
type templateData struct {
	ctx Context
	// service is the service currently being expanded; empty for the
	// setup/teardown commands, where {{.Port}} has no meaning.
	service string
}

// PreviewID is the sanitized bead id of this preview.
func (d templateData) PreviewID() string { return d.ctx.PreviewID }

// Host is the hostname preview URLs are built from.
func (d templateData) Host() string { return d.ctx.Host }

// Port is the port allocated to the service being expanded.
func (d templateData) Port() (int, error) {
	if d.service == "" {
		return 0, errors.New(`{{.Port}} is only available inside a service; use {{.ServicePort "name"}} here`)
	}
	return d.ServicePort(d.service)
}

// ServicePort is the port allocated to any service in the manifest, so one
// service can be told where another one listens.
func (d templateData) ServicePort(name string) (int, error) {
	port, ok := d.ctx.Ports[name]
	if !ok {
		return 0, fmt.Errorf("unknown service %q (known services: %s)", name, strings.Join(sortedKeysInt(d.ctx.Ports), ", "))
	}
	if port <= 0 {
		return 0, fmt.Errorf("no port allocated for service %q", name)
	}
	return port, nil
}

// expandString expands one templated manifest value. what identifies the field
// in error messages (e.g. `service "api": env "VITE_API_URL"`).
func expandString(what, value string, data templateData) (string, error) {
	if !strings.Contains(value, "{{") {
		return value, nil
	}
	tmpl, err := template.New(what).Option("missingkey=error").Parse(value)
	if err != nil {
		return "", fmt.Errorf("%s: invalid template: %w", what, unwrapTemplateError(err))
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("%s: %w", what, unwrapTemplateError(err))
	}
	return buf.String(), nil
}

// unwrapTemplateError strips text/template's positional wrapper so the message
// an operator sees is the actual cause ("unknown service \"apo\"") rather than
// a nested "template: ...: executing ... at <.ServicePort>: error calling ...".
// Only execution errors are unwrapped; a parse error is already readable.
func unwrapTemplateError(err error) error {
	var execErr template.ExecError
	if !errors.As(err, &execErr) {
		return err
	}
	cause := error(execErr)
	for {
		next := errors.Unwrap(cause)
		if next == nil {
			return cause
		}
		cause = next
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysInt(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
