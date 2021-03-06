// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// TODO(adg): packages at weekly/release
// TODO(adg): some means to register new packages

// +build appengine

package build

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/build/types"

	"cache"

	"appengine"
	"appengine/datastore"
	"appengine/memcache"
)

func init() {
	handleFunc("/", uiHandler)
}

// uiHandler draws the build status page.
func uiHandler(w http.ResponseWriter, r *http.Request) {
	d := dashboardForRequest(r)
	c := d.Context(appengine.NewContext(r))
	now := cache.Now(c)
	key := "build-ui"

	page, _ := strconv.Atoi(r.FormValue("page"))
	if page < 0 {
		page = 0
	}
	key += fmt.Sprintf("-page%v", page)

	branch := r.FormValue("branch")
	if branch != "" {
		key += "-branch-" + branch
	}

	repo := r.FormValue("repo")
	if repo != "" {
		key += "-repo-" + repo
	}

	var b []byte
	if cache.Get(r, now, key, &b) {
		w.Write(b)
		return
	}

	pkg := &Package{} // empty package is the main repository
	if repo != "" {
		var err error
		pkg, err = GetPackage(c, repo)
		if err != nil {
			logErr(w, r, err)
			return
		}
	}
	commits, err := dashCommits(c, pkg, page, branch)
	if err != nil {
		logErr(w, r, err)
		return
	}
	builders := commitBuilders(commits)

	var tipState *TagState
	if pkg.Kind == "" && page == 0 && (branch == "" || branch == "default") {
		// only show sub-repo state on first page of normal repo view
		tipState, err = TagStateByName(c, "tip")
		if err != nil {
			logErr(w, r, err)
			return
		}
	}

	p := &Pagination{}
	if len(commits) == commitsPerPage {
		p.Next = page + 1
	}
	if page > 0 {
		p.Prev = page - 1
		p.HasPrev = true
	}
	data := &uiTemplateData{d, pkg, commits, builders, tipState, p, branch}

	switch r.FormValue("mode") {
	case "failures":
		failuresHandler(w, r, data)
		return
	case "json":
		jsonHandler(w, r, data)
		return
	}

	// Populate building URLs for the HTML UI only.
	data.populateBuildingURLs(c)

	var buf bytes.Buffer
	if err := uiTemplate.Execute(&buf, data); err != nil {
		logErr(w, r, err)
		return
	}

	cache.Set(r, now, key, buf.Bytes())

	buf.WriteTo(w)
}

// failuresHandler is https://build.golang.org/?mode=failures , where it outputs
// one line per failure on the front page, in the form:
//    hash builder failure-url
func failuresHandler(w http.ResponseWriter, r *http.Request, data *uiTemplateData) {
	w.Header().Set("Content-Type", "text/plain")
	d := dashboardForRequest(r)
	for _, c := range data.Commits {
		for _, b := range data.Builders {
			res := c.Result(b, "")
			if res == nil || res.OK || res.LogHash == "" {
				continue
			}
			url := fmt.Sprintf("https://%v%v/log/%v", r.Host, d.Prefix, res.LogHash)
			fmt.Fprintln(w, c.Hash, b, url)
		}
	}
}

// jsonHandler is https://build.golang.org/?mode=json
// The output is a types.BuildStatus JSON object.
func jsonHandler(w http.ResponseWriter, r *http.Request, data *uiTemplateData) {
	d := dashboardForRequest(r)

	// cell returns one of "" (no data), "ok", or a failure URL.
	cell := func(res *Result) string {
		switch {
		case res == nil:
			return ""
		case res.OK:
			return "ok"
		}
		return fmt.Sprintf("https://%v%v/log/%v", r.Host, d.Prefix, res.LogHash)
	}

	var res types.BuildStatus
	res.Builders = data.Builders

	// First the commits from the main section (the "go" repo)
	for _, c := range data.Commits {
		rev := types.BuildRevision{
			Repo:     "go",
			Revision: c.Hash,
			Results:  make([]string, len(data.Builders)),
		}
		for i, b := range data.Builders {
			rev.Results[i] = cell(c.Result(b, ""))
		}
		res.Revisions = append(res.Revisions, rev)
	}

	// Then the one commit each for the subrepos.
	// TODO(bradfitz): we'll probably want more than one later, for people looking at
	// the subrepo-specific build history pages. But for now this gets me some data
	// to make forward progress.
	tip := data.TipState // a TagState
	for _, pkgState := range tip.Packages {
		goRev := tip.Tag.Hash
		rev := types.BuildRevision{
			Repo:       pkgState.Package.Name,
			Revision:   pkgState.Commit.Hash,
			GoRevision: goRev,
			Results:    make([]string, len(data.Builders)),
		}
		for i, b := range res.Builders {
			rev.Results[i] = cell(pkgState.Commit.Result(b, goRev))
		}
		res.Revisions = append(res.Revisions, rev)
	}

	v, _ := json.MarshalIndent(res, "", "\t")
	w.Header().Set("Content-Type", "text/json; charset=utf-8")
	w.Write(v)
}

type Pagination struct {
	Next, Prev int
	HasPrev    bool
}

// dashCommits gets a slice of the latest Commits to the current dashboard.
// If page > 0 it paginates by commitsPerPage.
func dashCommits(c appengine.Context, pkg *Package, page int, branch string) ([]*Commit, error) {
	offset := page * commitsPerPage
	q := datastore.NewQuery("Commit").
		Ancestor(pkg.Key(c)).
		Order("-Num")

	var commits []*Commit
	if branch == "" {
		_, err := q.Limit(commitsPerPage).Offset(offset).
			GetAll(c, &commits)
		return commits, err
	}

	// Look for commits on a specific branch.
	for t, n := q.Run(c), 0; len(commits) < commitsPerPage && n < 1000; {
		var c Commit
		_, err := t.Next(&c)
		if err == datastore.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		if !isBranchCommit(&c, branch) {
			continue
		}
		if n >= offset {
			commits = append(commits, &c)
		}
		n++
	}
	return commits, nil
}

// isBranchCommit reports whether the given commit is on the specified branch.
// It does so by examining the commit description, so there will be some bad
// matches where the branch commits do not begin with the "[branch]" prefix.
func isBranchCommit(c *Commit, b string) bool {
	d := strings.TrimSpace(c.Desc)
	if b == "default" {
		return !strings.HasPrefix(d, "[")
	}
	return strings.HasPrefix(d, "["+b+"]")
}

// commitBuilders returns the names of the builders that provided
// Results for the provided commits.
func commitBuilders(commits []*Commit) []string {
	builders := make(map[string]bool)
	for _, commit := range commits {
		for _, r := range commit.Results() {
			builders[r.Builder] = true
		}
	}
	k := keys(builders)
	sort.Sort(builderOrder(k))
	return k
}

func keys(m map[string]bool) (s []string) {
	for k := range m {
		s = append(s, k)
	}
	sort.Strings(s)
	return
}

// builderOrder implements sort.Interface, sorting builder names
// ("darwin-amd64", etc) first by builderPriority and then alphabetically.
type builderOrder []string

func (s builderOrder) Len() int      { return len(s) }
func (s builderOrder) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s builderOrder) Less(i, j int) bool {
	pi, pj := builderPriority(s[i]), builderPriority(s[j])
	if pi == pj {
		return s[i] < s[j]
	}
	return pi < pj
}

func builderPriority(builder string) (p int) {
	// Put -temp builders at the end, always.
	if strings.HasSuffix(builder, "-temp") {
		defer func() { p += 20 }()
	}
	// Group race builders together.
	if isRace(builder) {
		return 1
	}
	// If the OS has a specified priority, use it.
	if p, ok := osPriority[builderOS(builder)]; ok {
		return p
	}
	// The rest.
	return 10
}

func isRace(s string) bool {
	return strings.Contains(s, "-race-") || strings.HasSuffix(s, "-race")
}

func unsupported(builder string) bool {
	if strings.HasSuffix(builder, "-temp") {
		return true
	}
	return unsupportedOS(builderOS(builder))
}

func unsupportedOS(os string) bool {
	if os == "race" || os == "android" {
		return false
	}
	p, ok := osPriority[os]
	return !ok || p > 0
}

// Priorities for specific operating systems.
var osPriority = map[string]int{
	"darwin":  0,
	"freebsd": 0,
	"linux":   0,
	"windows": 0,
	// race == 1
	"android":   2,
	"openbsd":   3,
	"netbsd":    4,
	"dragonfly": 5,
}

// TagState represents the state of all Packages at a Tag.
type TagState struct {
	Tag      *Commit
	Packages []*PackageState
}

// PackageState represents the state of a Package at a Tag.
type PackageState struct {
	Package *Package
	Commit  *Commit
}

// TagStateByName fetches the results for all Go subrepos at the specified Tag.
func TagStateByName(c appengine.Context, name string) (*TagState, error) {
	tag, err := GetTag(c, name)
	if err != nil {
		return nil, err
	}
	pkgs, err := Packages(c, "subrepo")
	if err != nil {
		return nil, err
	}
	var st TagState
	for _, pkg := range pkgs {
		com, err := pkg.LastCommit(c)
		if err != nil {
			c.Warningf("%v: no Commit found: %v", pkg, err)
			continue
		}
		st.Packages = append(st.Packages, &PackageState{pkg, com})
	}
	st.Tag, err = tag.Commit(c)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

type uiTemplateData struct {
	Dashboard  *Dashboard
	Package    *Package
	Commits    []*Commit
	Builders   []string
	TipState   *TagState
	Pagination *Pagination
	Branch     string
}

// populateBuildingURLs populates each commit in Commits' buildingURLs map with the
// URLs of builds which are currently in progress.
func (td *uiTemplateData) populateBuildingURLs(ctx appengine.Context) {
	// need are memcache keys: "building|<hash>|<gohash>|<builder>"
	// The hash is of the main "go" repo, or the subrepo commit hash.
	// The gohash is empty for the main repo, else it's the Go hash.
	var need []string

	commit := map[string]*Commit{} // commit hash -> Commit

	// TODO(bradfitz): this only populates the main repo, not subpackages currently.
	for _, b := range td.Builders {
		for _, c := range td.Commits {
			if c.Result(b, "") == nil {
				commit[c.Hash] = c
				need = append(need, "building|"+c.Hash+"||"+b)
			}
		}
	}
	if len(need) == 0 {
		return
	}
	m, err := memcache.GetMulti(ctx, need)
	if err != nil {
		// oh well. this is a cute non-critical feature anyway.
		ctx.Debugf("GetMulti of building keys: %v", err)
		return
	}
	for k, it := range m {
		f := strings.SplitN(k, "|", 4)
		if len(f) != 4 {
			continue
		}
		hash, goHash, builder := f[1], f[2], f[3]
		c, ok := commit[hash]
		if !ok {
			continue
		}
		m := c.buildingURLs
		if m == nil {
			m = make(map[builderAndGoHash]string)
			c.buildingURLs = m
		}
		m[builderAndGoHash{builder, goHash}] = string(it.Value)
	}

}

var uiTemplate = template.Must(
	template.New("ui.html").Funcs(tmplFuncs).ParseFiles("build/ui.html"),
)

var tmplFuncs = template.FuncMap{
	"buildDashboards":   buildDashboards,
	"builderOS":         builderOS,
	"builderSpans":      builderSpans,
	"builderSubheading": builderSubheading,
	"builderTitle":      builderTitle,
	"repoURL":           repoURL,
	"shortDesc":         shortDesc,
	"shortHash":         shortHash,
	"shortUser":         shortUser,
	"tail":              tail,
	"unsupported":       unsupported,
}

func splitDash(s string) (string, string) {
	i := strings.Index(s, "-")
	if i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// builderOS returns the os tag for a builder string
func builderOS(s string) string {
	os, _ := splitDash(s)
	return os
}

// builderOSOrRace returns the builder OS or, if it is a race builder, "race".
func builderOSOrRace(s string) string {
	if isRace(s) {
		return "race"
	}
	return builderOS(s)
}

// builderArch returns the arch tag for a builder string
func builderArch(s string) string {
	_, arch := splitDash(s)
	arch, _ = splitDash(arch) // chop third part
	return arch
}

// builderSubheading returns a short arch tag for a builder string
// or, if it is a race builder, the builder OS.
func builderSubheading(s string) string {
	if isRace(s) {
		return builderOS(s)
	}
	arch := builderArch(s)
	switch arch {
	case "amd64":
		return "x64"
	}
	return arch
}

// builderArchChar returns the architecture letter for a builder string
func builderArchChar(s string) string {
	arch := builderArch(s)
	switch arch {
	case "386":
		return "8"
	case "amd64":
		return "6"
	case "arm":
		return "5"
	}
	return arch
}

type builderSpan struct {
	N           int
	OS          string
	Unsupported bool
}

// builderSpans creates a list of tags showing
// the builder's operating system names, spanning
// the appropriate number of columns.
func builderSpans(s []string) []builderSpan {
	var sp []builderSpan
	for len(s) > 0 {
		i := 1
		os := builderOSOrRace(s[0])
		u := unsupportedOS(os) || strings.HasSuffix(s[0], "-temp")
		for i < len(s) && builderOSOrRace(s[i]) == os {
			i++
		}
		sp = append(sp, builderSpan{i, os, u})
		s = s[i:]
	}
	return sp
}

// builderTitle formats "linux-amd64-foo" as "linux amd64 foo".
func builderTitle(s string) string {
	return strings.Replace(s, "-", " ", -1)
}

// buildDashboards returns the known public dashboards.
func buildDashboards() []*Dashboard {
	return dashboards
}

// shortDesc returns the first line of a description.
func shortDesc(desc string) string {
	if i := strings.Index(desc, "\n"); i != -1 {
		desc = desc[:i]
	}
	return limitStringLength(desc, 100)
}

// shortHash returns a short version of a hash.
func shortHash(hash string) string {
	if len(hash) > 12 {
		hash = hash[:12]
	}
	return hash
}

// shortUser returns a shortened version of a user string.
func shortUser(user string) string {
	if i, j := strings.Index(user, "<"), strings.Index(user, ">"); 0 <= i && i < j {
		user = user[i+1 : j]
	}
	if i := strings.Index(user, "@"); i >= 0 {
		return user[:i]
	}
	return user
}

// repoRe matches Google Code repositories and subrepositories (without paths).
var repoRe = regexp.MustCompile(`^code\.google\.com/p/([a-z0-9\-]+)(\.[a-z0-9\-]+)?$`)

// repoURL returns the URL of a change at a Google Code repository or subrepo.
func repoURL(dashboard, hash, packagePath string) (string, error) {
	if packagePath == "" {
		if dashboard == "Gccgo" {
			return "https://go.googlesource.com/gofrontend/+/" + hash, nil
		}
		if dashboard == "Mercurial" {
			return "https://golang.org/change/" + hash, nil
		}
		// TODO(adg): use the above once /change/ points to git hashes
		return "https://go.googlesource.com/go/+/" + hash, nil
	}

	// TODO(adg): remove this old hg stuff, one day.
	if dashboard == "Mercurial" {
		m := repoRe.FindStringSubmatch(packagePath)
		if m == nil {
			return "", errors.New("unrecognized package: " + packagePath)
		}
		url := "https://code.google.com/p/" + m[1] + "/source/detail?r=" + hash
		if len(m) > 2 {
			url += "&repo=" + m[2][1:]
		}
		return url, nil
	}

	repo := strings.TrimPrefix(packagePath, "golang.org/x/")
	return "https://go.googlesource.com/" + repo + "/+/" + hash, nil
}

// tail returns the trailing n lines of s.
func tail(n int, s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) < n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
