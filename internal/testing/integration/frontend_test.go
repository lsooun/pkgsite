// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/safehtml/template"
	"golang.org/x/pkgsite/internal"
	"golang.org/x/pkgsite/internal/fetch"
	"golang.org/x/pkgsite/internal/frontend"
	"golang.org/x/pkgsite/internal/middleware"
	"golang.org/x/pkgsite/internal/postgres"
	"golang.org/x/pkgsite/internal/proxy"
	"golang.org/x/pkgsite/internal/queue"
	"golang.org/x/pkgsite/internal/source"
	"golang.org/x/pkgsite/internal/testing/htmlcheck"
	"golang.org/x/pkgsite/internal/testing/testhelper"
)

var (
	in      = htmlcheck.In
	hasText = htmlcheck.HasText
)

func TestModulePackageDirectoryResolution(t *testing.T) {
	// The shared test state sets up the following scenario to exercise
	// what happens when a directory becomes a package,
	// and then becomes a directory again. Specifically:
	//  + at v1.2.3, github.com/golang/found/dir is a directory (containing dir/pkg)
	//  + at v1.2.4, github.com/golang/found/dir is a package
	//  + at v1.2.5, github.com/golang/found/dir is again just a directory
	versions := []*proxy.Module{
		{
			ModulePath: "github.com/golang/found",
			Version:    "v1.2.3",
			Files: map[string]string{
				"go.mod":         "module github.com/golang/found",
				"found.go":       "package found\nconst Value = 123",
				"dir/pkg/pkg.go": "package pkg\nconst Value = 321",
				"LICENSE":        testhelper.MITLicense,
			},
		},
		{
			ModulePath: "github.com/golang/found",
			Version:    "v1.2.4",
			Files: map[string]string{
				"go.mod":         "module github.com/golang/found",
				"found.go":       "package found\nconst Value = 124",
				"dir/pkg/pkg.go": "package pkg\nconst Value = 421",
				"dir/dir.go":     "package dir\nconst Value = \"I'm a package!\"",
				"LICENSE":        testhelper.MITLicense,
			},
		},
		{
			ModulePath: "github.com/golang/found",
			Version:    "v1.2.5",
			Files: map[string]string{
				"go.mod":         "module github.com/golang/found",
				"found.go":       "package found\nconst Value = 125",
				"dir/pkg/pkg.go": "package pkg\nconst Value = 521",
				"LICENSE":        testhelper.MITLicense,
			},
		},
	}

	tests := []struct {
		// Test description.
		desc string
		// URL Path (relative to the test server) to check
		urlPath string
		// The expected HTTP status code.
		wantCode int
		// If non-nil, used to verify the resulting page.
		want htmlcheck.Checker
	}{
		{
			desc:     "missing module",
			urlPath:  "/mod/github.com/golang/missing",
			wantCode: http.StatusNotFound,
		},
		{
			desc:     "latest module",
			urlPath:  "/mod/github.com/golang/found",
			wantCode: http.StatusOK,
			want: in("",
				in(".DetailsHeader", hasText("v1.2.5")),
				in(".DetailsHeader-badge", in(".DetailsHeader-badge--latest"))),
		},
		{
			desc:     "versioned module",
			urlPath:  "/mod/github.com/golang/found@v1.2.3",
			wantCode: http.StatusOK,
			want:     in(".DetailsHeader", hasText("v1.2.3")),
		},
		{
			desc:     "non-existent version",
			urlPath:  "/mod/github.com/golang/found@v1.1.3",
			wantCode: http.StatusNotFound,
			want:     in("h3.Fetch-message.js-fetchMessage", hasText("github.com/golang/found@v1.1.3")),
		},
		{
			desc:     "latest package",
			urlPath:  "/github.com/golang/found/dir/pkg",
			wantCode: http.StatusOK,
			want: in("",
				in(".DetailsHeader", hasText("v1.2.5")),
				in(".DetailsContent", hasText("521")),
				in(".DetailsHeader-badge", in(".DetailsHeader-badge--latest"))),
		},
		{
			desc:     "earlier package",
			urlPath:  "/github.com/golang/found@v1.2.3/dir/pkg",
			wantCode: http.StatusOK,
			want: in("",
				in(".DetailsHeader", hasText("v1.2.3")),
				in(".DetailsContent", hasText("321"))),
		},
		{
			desc:     "dir is initially a directory",
			urlPath:  "/github.com/golang/found@v1.2.3/dir",
			wantCode: http.StatusOK,
			want:     in(".Directories", hasText("pkg")),
		},
		{
			desc:     "dir becomes a package",
			urlPath:  "/github.com/golang/found@v1.2.4/dir",
			wantCode: http.StatusOK,
			want: in("",
				in(".DetailsHeader", hasText("v1.2.4")),
				in(".DetailsContent", hasText("I'm a package"))),
		},
		{
			desc:     "dir becomes a directory again",
			urlPath:  "/github.com/golang/found@v1.2.5/dir",
			wantCode: http.StatusOK,
			want: in("",
				in(".DetailsHeader", hasText("v1.2.5"))),
		},
		{
			desc:     "latest package for /dir",
			urlPath:  "/github.com/golang/found/dir",
			wantCode: http.StatusOK,
			want: in("",
				in(".DetailsHeader", hasText("v1.2.5"))),
		},
	}
	ctx := context.Background()
	ts := setupFrontend(ctx, t, nil)
	processVersions(ctx, t, versions)
	defer postgres.ResetTestDB(testDB, t)

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			testURL := ts.URL + test.urlPath
			validateResponse(t, http.MethodGet, testURL, test.wantCode, test.want)
		})
	}
}

// TODO(https://github.com/golang/go/issues/40096): factor out this code reduce
// duplication
func setupFrontend(ctx context.Context, t *testing.T, q queue.Queue) *httptest.Server {
	t.Helper()
	s, err := frontend.NewServer(frontend.ServerConfig{
		DataSourceGetter:     func(context.Context) internal.DataSource { return testDB },
		TaskIDChangeInterval: 10 * time.Minute,
		StaticPath:           template.TrustedSourceFromConstant("../../../content/static"),
		ThirdPartyPath:       "../../../third_party",
		AppVersionLabel:      "",
		Queue:                q,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Install(mux.Handle, nil, nil)

	experimenter, err := middleware.NewExperimenter(ctx, 1*time.Minute, testDB.GetExperiments, nil)
	if err != nil {
		t.Fatal(err)
	}
	enableCSP := true
	mw := middleware.Chain(
		middleware.AcceptRequests(http.MethodGet, http.MethodPost),
		middleware.SecureHeaders(enableCSP),
		middleware.LatestVersions(s.GetLatestMinorVersion, s.GetLatestMajorVersion),
		middleware.Experiment(experimenter),
	)
	return httptest.NewServer(mw(mux))
}

// TODO(https://github.com/golang/go/issues/40098): factor out this code reduce
// duplication
func setupQueue(ctx context.Context, t *testing.T, proxyModules []*proxy.Module, experimentNames ...string) (queue.Queue, func()) {
	proxyClient, teardown := proxy.SetupTestClient(t, proxyModules)
	sourceClient := source.NewClient(1 * time.Second)
	q := queue.NewInMemory(ctx, 1, experimentNames,
		func(ctx context.Context, mpath, version string) (int, error) {
			return frontend.FetchAndUpdateState(ctx, mpath, version, proxyClient, sourceClient, testDB)
		})
	return q, func() {
		teardown()
	}
}

func processVersions(ctx context.Context, t *testing.T, testModules []*proxy.Module) {
	t.Helper()
	proxyClient, teardown := proxy.SetupTestClient(t, testModules)
	defer teardown()

	for _, tm := range testModules {
		fetchAndInsertModule(ctx, t, tm, proxyClient)
	}
}

func fetchAndInsertModule(ctx context.Context, t *testing.T, tm *proxy.Module, proxyClient *proxy.Client) {
	sourceClient := source.NewClient(1 * time.Second)
	res := fetch.FetchModule(ctx, tm.ModulePath, tm.Version, proxyClient, sourceClient)
	defer res.Defer()
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if err := testDB.InsertModule(ctx, res.Module); err != nil {
		t.Fatal(err)
	}
}

func validateResponse(t *testing.T, method, testURL string, wantCode int, wantHTML htmlcheck.Checker) {
	t.Helper()

	var (
		resp *http.Response
		err  error
	)
	if method == http.MethodPost {
		resp, err = http.Post(testURL, "text/plain", nil)
	} else {
		resp, err = http.Get(testURL)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != wantCode {
		t.Fatalf("%q request to %q returned status %d, want %d", method, testURL, resp.StatusCode, wantCode)
	}
	if wantHTML != nil {
		if err := htmlcheck.Run(resp.Body, wantHTML); err != nil {
			t.Fatal(err)
		}
	}
}
