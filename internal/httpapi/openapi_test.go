package httpapi_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/Max252t/Cardo-backend/internal/httpapi"
)

const openAPIPath = "../../docs/openapi.yaml"

var (
	ginParam      = regexp.MustCompile(`:([A-Za-z0-9_]+)`)
	templateParam = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)
	httpMethods   = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}
)

type specAuth struct{ httpapi.AuthService }

type specTokens struct{ httpapi.TokenVerifier }

type specUsers struct{ httpapi.UserService }

type specBoards struct{ httpapi.BoardService }

type specMembers struct{ httpapi.MemberService }

type specSockets struct{ httpapi.BoardSockets }

type specClaims struct{ httpapi.ClaimsParser }

type specLimiter struct{ httpapi.RateLimiter }

func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(openAPIPath))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	return doc
}

func registeredRoutes(t *testing.T) map[string]bool {
	t.Helper()
	router, err := httpapi.NewRouter(httpapi.Deps{
		Auth:        specAuth{},
		Tokens:      specTokens{},
		Users:       specUsers{},
		Boards:      specBoards{},
		Members:     specMembers{},
		Sockets:     specSockets{},
		Claims:      specClaims{},
		RateLimiter: specLimiter{},
	})
	require.NoError(t, err)
	return routeSet(router.Routes())
}

func routeSet(routes gin.RoutesInfo) map[string]bool {
	out := make(map[string]bool, len(routes))
	for _, r := range routes {
		out[r.Method+" "+ginParam.ReplaceAllString(r.Path, "{$1}")] = true
	}
	return out
}

func asMap(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	require.True(t, ok, "%s must be a mapping", what)
	return m
}

type operation struct {
	key  string
	path string
	item map[string]any
	op   map[string]any
}

func specOperations(t *testing.T, doc map[string]any) []operation {
	t.Helper()
	paths := asMap(t, doc["paths"], "paths")
	var ops []operation
	for path, rawItem := range paths {
		item := asMap(t, rawItem, "path item "+path)
		for _, method := range httpMethods {
			rawOp, ok := item[method]
			if !ok {
				continue
			}
			ops = append(ops, operation{
				key:  strings.ToUpper(method) + " " + path,
				path: path,
				item: item,
				op:   asMap(t, rawOp, "operation "+method+" "+path),
			})
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].key < ops[j].key })
	return ops
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestOpenAPIMatchesRegisteredRoutes(t *testing.T) {
	doc := loadSpec(t)
	documented := map[string]bool{}
	for _, o := range specOperations(t, doc) {
		documented[o.key] = true
	}
	registered := registeredRoutes(t)

	var missingInSpec, missingInRouter []string
	for _, k := range sortedKeys(registered) {
		if !documented[k] {
			missingInSpec = append(missingInSpec, k)
		}
	}
	for _, k := range sortedKeys(documented) {
		if !registered[k] {
			missingInRouter = append(missingInRouter, k)
		}
	}
	require.Empty(t, missingInSpec, "routes registered by the router but absent from docs/openapi.yaml")
	require.Empty(t, missingInRouter, "operations in docs/openapi.yaml with no registered route")
}

func TestOpenAPIOperationsAreComplete(t *testing.T) {
	doc := loadSpec(t)
	seenIDs := map[string]string{}
	for _, o := range specOperations(t, doc) {
		id, _ := o.op["operationId"].(string)
		require.NotEmpty(t, id, "%s has no operationId", o.key)
		if prev, dup := seenIDs[id]; dup {
			require.Failf(t, "duplicate operationId", "%q is used by %s and %s", id, prev, o.key)
		}
		seenIDs[id] = o.key

		responses, ok := o.op["responses"].(map[string]any)
		require.True(t, ok, "%s has no responses", o.key)
		require.NotEmpty(t, responses, "%s has no responses", o.key)
	}
}

func TestOpenAPIPathParametersAreDeclared(t *testing.T) {
	doc := loadSpec(t)
	for _, o := range specOperations(t, doc) {
		declared := map[string]bool{}
		for _, list := range []any{o.item["parameters"], o.op["parameters"]} {
			params, _ := list.([]any)
			for _, raw := range params {
				p := asMap(t, resolveParam(t, doc, raw), "parameter of "+o.key)
				if p["in"] == "path" {
					name, _ := p["name"].(string)
					declared[name] = true
				}
			}
		}
		for _, m := range templateParam.FindAllStringSubmatch(o.path, -1) {
			require.True(t, declared[m[1]], "%s does not declare path parameter %q", o.key, m[1])
		}
	}
}

func resolveParam(t *testing.T, doc map[string]any, raw any) any {
	t.Helper()
	m := asMap(t, raw, "parameter")
	ref, ok := m["$ref"].(string)
	if !ok {
		return m
	}
	return resolvePointer(t, doc, ref)
}

func TestOpenAPIReferencesResolve(t *testing.T) {
	doc := loadSpec(t)
	refs := collectRefs(doc, nil)
	require.NotEmpty(t, refs)
	for _, ref := range refs {
		resolvePointer(t, doc, ref)
	}
}

func collectRefs(node any, acc []string) []string {
	switch v := node.(type) {
	case map[string]any:
		for key, child := range v {
			if s, ok := child.(string); ok && (key == "$ref" || strings.HasPrefix(s, "#/components/")) {
				acc = append(acc, s)
				continue
			}
			acc = collectRefs(child, acc)
		}
	case []any:
		for _, child := range v {
			acc = collectRefs(child, acc)
		}
	}
	return acc
}

func resolvePointer(t *testing.T, doc map[string]any, ref string) any {
	t.Helper()
	require.True(t, strings.HasPrefix(ref, "#/"), "unsupported reference %q", ref)
	var node any = doc
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := node.(map[string]any)
		require.True(t, ok, "reference %q passes through a non-mapping at %q", ref, part)
		next, found := m[part]
		require.True(t, found, "reference %q does not resolve: %q is missing", ref, part)
		node = next
	}
	return node
}
