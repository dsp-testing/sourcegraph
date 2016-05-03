package prefetch

import (
	"encoding/base64"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"path"

	"github.com/gorilla/mux"
	"sourcegraph.com/sourcegraph/sourcegraph/app/auth"
	"sourcegraph.com/sourcegraph/sourcegraph/go-sourcegraph/routevar"
	"sourcegraph.com/sourcegraph/sourcegraph/go-sourcegraph/sourcegraph"
	"sourcegraph.com/sourcegraph/sourcegraph/httpapi"
	apirouter "sourcegraph.com/sourcegraph/sourcegraph/httpapi/router"
	"sourcegraph.com/sourcegraph/sourcegraph/util/handlerutil"
	"sourcegraph.com/sourcegraph/sourcegraph/util/httputil/httpctx"
)

type FlushWriter interface {
	io.Writer
	http.Flusher
}

const (
	DefRoute = apirouter.Def
)

var router *mux.Router
var apiHandler http.Handler

func init() {
	router = mux.NewRouter().StrictSlash(true)
	router.Path("/" + routevar.Repo + routevar.RepoRevSuffix + "/-/def/" + routevar.Def).Name(DefRoute)

	apiHandler = httpapi.NewHandler(nil)
}

func copyVars(vars map[string]string) map[string]string {
	vars2 := make(map[string]string, len(vars))
	for k, v := range vars {
		vars2[k] = v
	}
	return vars2
}

// FetchURLsForRequest generates push promise URLs for the current request.
func FetchURLsForRequest(req *http.Request) ([]string, error) {
	var fetches []string
	err := router.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		m := &mux.RouteMatch{}
		route.Match(req, m)
		if m.Route == nil {
			return nil
		}
		name := m.Route.GetName()
		vars := m.Vars
		switch name {
		case DefRoute:
			var err error
			fetches, err = fetchURLsForDef(req, vars)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	for i, url := range fetches {
		fetches[i] = template.JSEscapeString(url)
	}
	return fetches, nil
}

// ResolveFetches fetches data for the given promise URLs and writes a callback
// to the current response that injects the fetched data.
func ResolveFetches(w FlushWriter, req *http.Request, urls []string) error {
	if len(urls) == 0 {
		return nil
	}

	for _, url := range urls {
		fetchReq, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return err
		}

		ctx := httpctx.FromRequest(req)
		httpctx.SetForRequest(fetchReq, ctx)
		for _, c := range req.Cookies() {
			fetchReq.AddCookie(c)
		}
		sess, err := auth.ReadSessionCookie(req)
		if err != nil {
			return err
		}
		// HTTP API doesn't accept cookie auth; we need to generate the necessary auth header.
		auth := base64.StdEncoding.EncodeToString([]byte("Basic: x-oauth-basic: " + sess.AccessToken))
		fetchReq.Header.Set("Authorization", auth)

		out := httptest.NewRecorder()
		apiHandler.ServeHTTP(out, fetchReq)
		resp := out.Body.String()
		w.Write([]byte(`
<script>window.__resolvePushPromise("` + template.JSEscapeString(url) + `","` + template.JSEscapeString(resp) + `")</script>`))
	}
	w.(http.Flusher).Flush()
	return nil
}

// fetchURLsForDef returns the def route in routePatterns.js. The URLs returned must be
// kept in sync with fetches generated by BlobMain in BlobMain.js
func fetchURLsForDef(req *http.Request, vars map[string]string) ([]string, error) {
	var fetches []string
	resolveURL, err := apirouter.URL(apirouter.RepoResolve, vars)
	if err != nil {
		return nil, err
	}
	fetches = append(fetches, resolveURL.String())
	authURL, err := apirouter.URL(apirouter.AuthInfo, vars)
	if err != nil {
		return nil, err
	}
	fetches = append(fetches, authURL.String())
	inventoryURL, err := apirouter.URL(apirouter.RepoInventory, vars)
	if err != nil {
		return nil, err
	}
	fetches = append(fetches, inventoryURL.String())

	// TODO kick-off the other fetches while we do the more intensive
	// URL resolution below.
	ctx, cl := handlerutil.Client(req)
	repo, err := cl.Repos.Get(ctx, &sourcegraph.RepoSpec{URI: vars["Repo"]})
	if err != nil {
		return nil, err
	}
	defVars := copyVars(vars)
	if defVars["Rev"] == "" {
		defVars["Rev"] = "@" + repo.DefaultBranch
	}
	defURL, err := apirouter.URL(apirouter.Def, defVars)
	if err != nil {
		return nil, err
	}
	fetches = append(fetches, defURL.String())
	defRefsURL, err := apirouter.URL(apirouter.DefRefLocations, vars)
	if err != nil {
		return nil, err
	}
	q := defRefsURL.Query()
	q.Set("ReposOnly", "true")
	defRefsURL.RawQuery = q.Encode()
	fetches = append(fetches, defRefsURL.String())

	def, err := handlerutil.GetDefCommon(ctx, vars, nil)
	if err != nil {
		return nil, err
	}
	treeVars := copyVars(defVars)
	treeVars["Path"] = path.Join("/", def.File)
	treeURL, err := apirouter.URL(apirouter.RepoTree, treeVars)
	if err != nil {
		return nil, err
	}
	q = treeURL.Query()
	q.Set("ContentsAsString", "true")
	treeURL.RawQuery = q.Encode()
	fetches = append(fetches, treeURL.String())

	authorVars := copyVars(defVars)
	authorVars["Rev"] = "@" + def.CommitID // Authors API needs a commit ID
	authorURL, err := apirouter.URL(apirouter.DefAuthors, authorVars)
	if err != nil {
		return nil, err
	}
	fetches = append(fetches, authorURL.String())
	return fetches, nil
}
