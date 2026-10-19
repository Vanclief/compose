package apidocs

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
)

// Curl builds a multi-line curl command for the operation.
//
// The URL is server (or the operation's first server, or the document's first
// server) joined with path, with path parameters replaced by their examples
// and required query parameters appended. Credentials from the first security
// requirement appear as shell-style placeholders such as $API_KEY or $TOKEN
// for the reader to fill in. The body is the example of the first request
// content type.
func (d *Document) Curl(method, path string, item *PathItem, op *Operation, server string) string {
	method = strings.ToUpper(method)

	if server == "" && op != nil && len(op.Servers) > 0 {
		server = op.Servers[0].URL
	}
	if server == "" && len(d.Servers) > 0 {
		server = d.Servers[0].URL
	}

	var query []string
	var args []string

	for _, p := range d.mergedParameters(item, op) {
		switch p.In {
		case "path":
			value := url.PathEscape(paramString(d.parameterExample(p)))
			path = strings.ReplaceAll(path, "{"+p.Name+"}", value)
		case "query":
			if p.Required {
				query = append(query, url.QueryEscape(p.Name)+"="+url.QueryEscape(paramString(d.parameterExample(p))))
			}
		case "header":
			if p.Required && !reservedHeader(p.Name) {
				args = append(args, "-H "+shellQuote(p.Name+": "+paramString(d.parameterExample(p))))
			}
		}
	}

	if op != nil {
		authArgs, authQuery := d.securityArgs(op)
		args = append(authArgs, args...)
		query = append(query, authQuery...)
		args = append(args, d.bodyArgs(op)...)
	}

	target := strings.TrimRight(server, "/") + path
	if len(query) > 0 {
		target += "?" + strings.Join(query, "&")
	}

	first := "curl"
	if method != "GET" {
		first += " -X " + method
	}
	first += " " + shellQuote(target)

	lines := append([]string{first}, args...)
	return strings.Join(lines, " \\\n  ")
}

// mergedParameters returns the path-level parameters overridden by the
// operation's parameters of the same name and location, refs resolved.
func (d *Document) mergedParameters(item *PathItem, op *Operation) []*Parameter {
	var out []*Parameter
	index := map[string]int{}

	add := func(params []*Parameter) {
		for _, raw := range params {
			p := d.Parameter(raw)
			if p == nil {
				continue
			}
			key := p.In + "\x00" + p.Name
			i, exists := index[key]
			if exists {
				out[i] = p
				continue
			}
			index[key] = len(out)
			out = append(out, p)
		}
	}

	if item != nil {
		add(item.Parameters)
	}
	if op != nil {
		add(op.Parameters)
	}
	return out
}

// parameterExample returns the parameter's example, its first named example,
// or one generated from its schema or content.
func (d *Document) parameterExample(p *Parameter) any {
	if p.Example != nil {
		return p.Example
	}
	for _, key := range p.Examples.Keys {
		ex := d.Example(p.Examples.Map[key])
		if ex != nil && ex.Value != nil {
			return ex.Value
		}
	}
	if p.Schema != nil {
		return d.ExampleValue(p.Schema, ForRequest)
	}
	for _, key := range p.Content.Keys {
		v, ok := d.MediaExample(p.Content.Map[key], ForRequest)
		if ok {
			return v
		}
	}
	return nil
}

// paramString renders an example as it appears in a URL or header: scalars
// as text, arrays comma separated (the default simple and form styles), and
// objects as compact JSON.
func paramString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		parts := make([]string, len(t))
		for i, item := range t {
			parts[i] = paramString(item)
		}
		return strings.Join(parts, ",")
	case *Ordered[any], Ordered[any], map[string]any, map[any]any:
		var buf bytes.Buffer
		appendJSON(&buf, t)
		return buf.String()
	}
	return fmt.Sprint(v)
}

// reservedHeader reports header parameters OpenAPI says to ignore because
// curl derives them from other flags.
func reservedHeader(name string) bool {
	switch strings.ToLower(name) {
	case "accept", "content-type", "authorization":
		return true
	}
	return false
}

// securityArgs returns the curl flags and query pairs for the first security
// requirement of op, in the order the schemes are declared in components.
func (d *Document) securityArgs(op *Operation) ([]string, []string) {
	reqs := d.OperationSecurity(op)
	if len(reqs) == 0 {
		return nil, nil
	}
	req := reqs[0]

	var args []string
	var query []string
	for _, key := range d.Components.SecuritySchemes.Keys {
		_, used := req[key]
		scheme := d.Components.SecuritySchemes.Map[key]
		if !used || scheme == nil {
			continue
		}

		switch strings.ToLower(scheme.Type) {
		case "apikey":
			name := scheme.Name
			if name == "" {
				name = key
			}
			placeholder := "$" + envName(name)
			switch scheme.In {
			case "query":
				query = append(query, url.QueryEscape(name)+"="+placeholder)
			case "cookie":
				args = append(args, "-b "+shellQuote(name+"="+placeholder))
			default:
				args = append(args, "-H "+shellQuote(name+": "+placeholder))
			}
		case "http":
			if strings.EqualFold(scheme.Scheme, "basic") {
				args = append(args, "-u "+shellQuote("$USER:$PASSWORD"))
			} else {
				args = append(args, "-H "+shellQuote("Authorization: Bearer $TOKEN"))
			}
		case "oauth2", "openidconnect":
			args = append(args, "-H "+shellQuote("Authorization: Bearer $TOKEN"))
		}
	}
	return args, query
}

// envName upper-cases name and replaces anything but letters and digits with
// underscores: API-KEY becomes API_KEY.
func envName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// bodyArgs returns the Content-Type header and body flag for the first content
// type of the request body.
func (d *Document) bodyArgs(op *Operation) []string {
	body := d.RequestBody(op.RequestBody)
	if body == nil || body.Content.Len() == 0 {
		return nil
	}

	contentType := body.Content.Keys[0]
	args := []string{"-H " + shellQuote("Content-Type: "+contentType)}

	if !isJSONMediaType(contentType) {
		return append(args, "--data-binary "+shellQuote("@file"))
	}

	example := d.MediaExampleJSON(body.Content.Map[contentType], ForRequest)
	if example == "" {
		return args
	}
	return append(args, "-d "+shellQuote(example))
}

// isJSONMediaType matches application/json, its parameters and +json
// suffixes such as application/problem+json.
func isJSONMediaType(contentType string) bool {
	mediaType, _, _ := strings.Cut(strings.ToLower(contentType), ";")
	mediaType = strings.TrimSpace(mediaType)
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// shellQuote wraps s in single quotes for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
