package reference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Curl builds a multi-line curl command for the operation.
//
// The base URL is chosen in this order: the server argument, an explicit
// override such as Options.ServerURL; the operation's first server; the
// document's first server; otherwise none, which leaves a relative URL. Pass
// "" for server when there is no override so operation servers win over
// document servers. Declared servers have their {variables} replaced by their
// defaults, and so does a server argument equal to a declared server URL.
//
// The base URL is joined with path, with path parameters replaced by their
// examples and required query parameters appended, serialized as ParamValues
// describes. Credentials from the first security requirement appear as shell
// variables such as $API_KEY or $TOKEN, double-quoted so the shell expands
// them. The body is the example of the first request content type: JSON as
// -d, multipart/form-data as one --form-string per property (-F field=@path
// for binary properties), form-urlencoded as one --data-urlencode per
// property, and anything else as --data-binary '@file'.
func (d *Document) Curl(method, path string, item *PathItem, op *Operation, server string) string {
	method = strings.ToUpper(method)
	server = d.serverURL(op, server)

	var query []string
	var args []string

	for _, p := range d.Parameters(item, op) {
		switch p.In {
		case "path":
			value := strings.Join(ParamValues(p, d.parameterExample(p)), ",")
			path = strings.ReplaceAll(path, "{"+p.Name+"}", url.PathEscape(value))
		case "query":
			if p.Required {
				for _, value := range ParamValues(p, d.parameterExample(p)) {
					query = append(query, url.QueryEscape(p.Name)+"="+url.QueryEscape(value))
				}
			}
		case "header":
			if p.Required && !reservedHeader(p.Name) {
				value := strings.Join(ParamValues(p, d.parameterExample(p)), ",")
				args = append(args, "-H "+shellQuote(p.Name+": "+value))
			}
		}
	}

	var authQuery []queryPlaceholder
	if op != nil {
		var authArgs []string
		authArgs, authQuery = d.securityArgs(op)
		args = append(authArgs, args...)
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
	first += " " + quoteTarget(target, len(query) > 0, authQuery)

	lines := append([]string{first}, args...)
	return strings.Join(lines, " \\\n  ")
}

// serverURL applies the precedence documented on Curl.
func (d *Document) serverURL(op *Operation, override string) string {
	var declared []Server
	if op != nil {
		declared = append(declared, op.Servers...)
	}
	declared = append(declared, d.Servers...)

	if override != "" {
		for _, s := range declared {
			if s.URL == override {
				return s.Expand()
			}
		}
		return override
	}
	if len(declared) > 0 {
		return declared[0].Expand()
	}
	return ""
}

// queryPlaceholder is a credential sent in the query string: name=$Variable.
type queryPlaceholder struct {
	Name     string `json:"name"`
	Variable string `json:"var"`
}

// curlAuth is the credential part of Curl as JSON, for the client to put in
// the curl it copies: {"args":[...],"query":[{"name":..,"var":..}]}.
func (d *Document) curlAuth(op *Operation) string {
	args, query := d.securityArgs(op)
	if len(args) == 0 && len(query) == 0 {
		return ""
	}
	out, err := json.Marshal(struct {
		Args  []string           `json:"args,omitempty"`
		Query []queryPlaceholder `json:"query,omitempty"`
	}{args, query})
	if err != nil {
		return ""
	}
	return string(out)
}

// quoteTarget single-quotes the URL and appends credential query pairs with
// their variable double-quoted, so 'https://h/p?api-key='"$API_KEY" is one
// shell word in which only the credential expands.
func quoteTarget(target string, hasQuery bool, auth []queryPlaceholder) string {
	if len(auth) == 0 {
		return shellQuote(target)
	}
	var b strings.Builder
	literal := target
	for i, q := range auth {
		sep := "&"
		if i == 0 && !hasQuery {
			sep = "?"
		}
		literal += sep + url.QueryEscape(q.Name) + "="
		b.WriteString(shellQuote(literal))
		b.WriteString(`"$` + q.Variable + `"`)
		literal = ""
	}
	return b.String()
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

// ParamValues serializes an example value of p into the values sent under
// p.Name, following the OpenAPI style and explode rules for arrays so the
// curl sample and the try-it client agree:
//
//   - style defaults to form for query and cookie parameters and to simple
//     for path and header parameters;
//   - explode defaults to true for form and false for every other style;
//   - an exploded array of a query style (form, pipeDelimited,
//     spaceDelimited) yields one value per item, which the caller sends as
//     repeated keys (a=1&a=2);
//   - any other array yields a single value joined with "," (form, simple
//     and anything unrecognized), "|" (pipeDelimited) or " "
//     (spaceDelimited).
//
// Items and non-array values are rendered with ParamString, so objects
// travel as compact JSON; the deepObject, label and matrix styles are not
// modeled. An exploded empty array yields no values.
func ParamValues(p *Parameter, v any) []string {
	items, isArray := v.([]any)
	if !isArray {
		return []string{ParamString(v)}
	}

	style := p.Style
	if style == "" {
		style = "simple"
		if p.In == "query" || p.In == "cookie" {
			style = "form"
		}
	}
	explode := style == "form"
	if p.Explode != nil {
		explode = *p.Explode
	}

	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = ParamString(item)
	}
	sep := ","
	repeatable := false
	switch style {
	case "form":
		repeatable = true
	case "pipeDelimited":
		sep = "|"
		repeatable = true
	case "spaceDelimited":
		sep = " "
		repeatable = true
	}
	if explode && repeatable {
		return parts
	}
	return []string{strings.Join(parts, sep)}
}

// ParamString renders an example value as text for a URL, header or form
// field: nil as "", strings verbatim, booleans as true or false, numbers
// without an exponent (1000000, not 1e+06), arrays comma-joined, and objects
// (*Ordered[any], map[string]any, map[any]any) as compact JSON.
func ParamString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case []any:
		parts := make([]string, len(t))
		for i, item := range t {
			parts[i] = ParamString(item)
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

// securityArgs returns the curl flags and query credentials for the first
// security requirement of op, in the order the schemes are declared in
// components.
func (d *Document) securityArgs(op *Operation) ([]string, []queryPlaceholder) {
	reqs := d.OperationSecurity(op)
	if len(reqs) == 0 {
		return nil, nil
	}
	req := reqs[0]

	var args []string
	var query []queryPlaceholder
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
			variable := envName(name)
			switch scheme.In {
			case "query":
				query = append(query, queryPlaceholder{Name: name, Variable: variable})
			case "cookie":
				args = append(args, "-b "+shellPlaceholder(name+"=", variable))
			default:
				args = append(args, "-H "+shellPlaceholder(name+": ", variable))
			}
		case "http":
			if strings.EqualFold(scheme.Scheme, "basic") {
				args = append(args, `-u "$BASIC_USER:$BASIC_PASSWORD"`)
			} else {
				args = append(args, "-H "+shellPlaceholder("Authorization: Bearer ", "TOKEN"))
			}
		case "oauth2", "openidconnect":
			args = append(args, "-H "+shellPlaceholder("Authorization: Bearer ", "TOKEN"))
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

// bodyArgs returns the flags that send the example of the first content type
// of the request body, as described on Curl.
func (d *Document) bodyArgs(op *Operation) []string {
	body := d.RequestBody(op.RequestBody)
	if body == nil || body.Content.Len() == 0 {
		return nil
	}

	contentType := body.Content.Keys[0]
	mt := body.Content.Map[contentType]

	switch mediaType(contentType) {
	case "multipart/form-data":
		// curl sets the multipart Content-Type with its boundary.
		return d.formArgs(mt, "--form-string ")
	case "application/x-www-form-urlencoded":
		return append([]string{"-H " + shellQuote("Content-Type: "+contentType)}, d.formArgs(mt, "--data-urlencode ")...)
	}

	args := []string{"-H " + shellQuote("Content-Type: "+contentType)}
	if !isJSONMediaType(contentType) {
		return append(args, "--data-binary "+shellQuote("@file"))
	}

	example := d.MediaExampleJSON(mt, ForRequest)
	if example == "" {
		return args
	}
	return append(args, "-d "+shellQuote(example))
}

// formArgs returns one flag per property of the media type's example object,
// repeating the flag for each item of an array value. For multipart,
// properties whose schema is format: binary (or an array of those) are sent as
// a file upload, -F field=@path; every other value uses --form-string, so a
// value starting with @ or < is sent literally instead of reading a local file.
func (d *Document) formArgs(mt *MediaType, flag string) []string {
	v, ok := d.MediaExample(mt, ForRequest)
	if !ok {
		return nil
	}
	keys, values := objectEntries(v)

	var props Ordered[*Schema]
	if mt.Schema != nil {
		flat := d.Flatten(mt.Schema)
		if flat != nil {
			props = flat.Properties
		}
	}

	var args []string
	for _, key := range keys {
		prop, _ := props.Get(key)
		if flag == "--form-string " && d.isBinary(prop) {
			args = append(args, "-F "+shellQuote(key+"=@path"))
			continue
		}
		items, isArray := values[key].([]any)
		if !isArray {
			items = []any{values[key]}
		}
		for _, item := range items {
			args = append(args, flag+shellQuote(key+"="+ParamString(item)))
		}
	}
	return args
}

// isBinary reports whether s is a file: format binary, or an array of those.
func (d *Document) isBinary(s *Schema) bool {
	flat := d.Flatten(s)
	if flat == nil {
		return false
	}
	if flat.Format == "binary" {
		return true
	}
	items := d.Flatten(flat.Items)
	return items != nil && items.Format == "binary"
}

// objectEntries returns the keys and values of an example object: in order
// for *Ordered[any], sorted for plain maps, and none for anything else.
func objectEntries(v any) ([]string, map[string]any) {
	switch t := v.(type) {
	case *Ordered[any]:
		if t == nil {
			return nil, nil
		}
		return t.Keys, t.Map
	case Ordered[any]:
		return t.Keys, t.Map
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys, t
	case map[any]any:
		m := make(map[string]any, len(t))
		keys := make([]string, 0, len(t))
		for k, val := range t {
			key := fmt.Sprint(k)
			m[key] = val
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return keys, m
	}
	return nil, nil
}

// mediaType returns the lower-cased media type without parameters.
func mediaType(contentType string) string {
	mt, _, _ := strings.Cut(strings.ToLower(contentType), ";")
	return strings.TrimSpace(mt)
}

// isJSONMediaType matches application/json, its parameters and +json
// suffixes such as application/problem+json.
func isJSONMediaType(contentType string) bool {
	mt := mediaType(contentType)
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

// shellQuote wraps s in single quotes for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellPlaceholder double-quotes literal followed by $variable so the shell
// expands the variable and nothing else: "API-KEY: $API_KEY". An
// interactive bash also expands ! as history inside double quotes, and a
// backslash before it would stay in the word, so ! closes the quotes and is
// written single-quoted: "X-Key"'!'": $X_KEY".
func shellPlaceholder(literal, variable string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`", "!", `"'!'"`).Replace(literal)
	return `"` + escaped + "$" + variable + `"`
}
