package main // import "github.com/myob-technology/hubr"

import (
	"archive/zip"
	"bufio"
	"context"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/awserr"
	"github.com/aws/aws-sdk-go-v2/aws/external"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
	git "gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/format/config"
	"gopkg.in/src-d/go-git.v4/plumbing/format/diff"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
)

const (
	// the default tag if not supplied
	defaultTag = "latest"

	// the number of parallel uploads or downloads
	workers = 3
)

var (
	// the default org/owner if not supplied
	defaultOrg = os.Getenv("HUBR_DEFAULT_ORG")

	// default auth chain (key:value,key:value)
	defaultChain = "env:GITHUB_API_TOKEN,env:TOKEN"

	// context used for github calls
	ctxbg = context.Background()

	// hubr version, set at build time
	// -ldflags="-X main.hubr=$(head -n 1 VERSION)"
	hubr = "unknown"
)

// asset is a GitHub release asset and a pointer to the release
type asset struct {
	github.ReleaseAsset
	Release *github.RepositoryRelease
	id      ident
}

// client is a wrapper over the github client.
type client struct {
	*github.Client
}

// NewClient creates a new client. It attempts to acquire a GitHub token from
// the auth chain defined by the global defaultChain.
// The chain takes the form of a string "k:v,k:v,k:v".
// - key "env" calls os.Getenv(v)
// - key "ssm" calls ssmGet(v)
// The first result which is not missing is used for GitHub authentication.
// If no result is found hubr will attempt to invoke a git credential helper.
func NewClient() (*client, error) {
	var token string
	for _, p := range strings.Split(defaultChain, ",") {
		kv := strings.Split(p, ":")
		if len(kv) != 2 {
			return nil, fmt.Errorf("invalid auth chain value: %v", p)
		}
		switch kv[0] {
		case "env":
			token = os.Getenv(kv[1])
		case "ssm":
			token, _ = ssmGet(kv[1])
		default:
			return nil, fmt.Errorf("invalid auth chain value: %v", p)
		}
		if token != "" {
			break
		}
	}
	if token == "" {
		token = credHelper()
	}
	if token == "" {
		return nil, fmt.Errorf("auth chain failed: " + defaultChain)
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctxbg, ts)
	return &client{Client: github.NewClient(tc)}, nil
}

// CreateRelease creates a GitHub release with the given tag, name and body.
// If the release already exists nothing happens and no error is returned.
// If pre is true the release will be a prerelease.
func (c *client) CreateRelease(id ident, name, body string, pre bool) error {
	r, rsp, err := c.Repositories.GetReleaseByTag(ctxbg, id.org, id.repo, id.tag)
	if rsp.StatusCode != http.StatusNotFound {
		if err != nil {
			return err
		}
		return nil
	}

	r = &github.RepositoryRelease{
		TagName:    &id.tag,
		Name:       &name,
		Body:       &body,
		Prerelease: &pre,
	}

	_, _, err = c.Repositories.CreateRelease(ctxbg, id.org, id.repo, r)
	return err
}

// DraftRelease creates a GitHub draft release with the given tag, name and body.
// If the release already exists nothing happens and no error is returned.
// If pre is true the release will be a prerelease.
func (c *client) DraftRelease(id ident, name, body string, pre bool) (*github.RepositoryRelease, error) {
	r, err := c.GetDraft(id)
	switch {
	case err == nil:
		return r, nil
	case isNotFound(err):
	default:
		return nil, fmt.Errorf("get release: %s", err)
	}

	r = &github.RepositoryRelease{
		TagName:    github.String(id.tag),
		Name:       github.String(name),
		Body:       github.String(body),
		Draft:      github.Bool(true),
		Prerelease: github.Bool(pre),
	}

	r, _, err = c.Repositories.CreateRelease(ctxbg, id.org, id.repo, r)
	return r, err
}

// ListReleases returns a slice of releases for the given repo.
func (c *client) ListReleases(id ident) ([]*github.RepositoryRelease, error) {
	rs, _, err := c.Repositories.ListReleases(ctxbg, id.org, id.repo,
		&github.ListOptions{Page: 0})
	if err != nil {
		return []*github.RepositoryRelease{}, err
	}

	return rs, nil
}

// GetDraft returns the first release with a matching tag. The returned release
// may or may not actually be a draft.
func (c *client) GetDraft(id ident) (*github.RepositoryRelease, error) {
	rs, err := c.ListReleases(id)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if id.tag == r.GetTagName() {
			return r, nil
		}
	}
	return nil, errNotFound{id}
}

// GetRelease returns the release for a given tag, which may be "latest" for the
// latest full release.
func (c *client) GetRelease(id ident) (*github.RepositoryRelease, error) {
	var (
		r   *github.RepositoryRelease
		err error
	)

	switch id.tag {
	case "edge":
		rs, err := c.ListReleases(id)
		if err != nil {
			return nil, err
		}
		if len(rs) == 0 {
			return nil, errNoReleases{id}
		}
		return rs[0], nil
	case "stable":
		fallthrough
	case defaultTag:
		r, _, err = c.Repositories.GetLatestRelease(ctxbg, id.org, id.repo)
	default:
		r, _, err = c.Repositories.GetReleaseByTag(ctxbg, id.org, id.repo, id.tag)
	}

	return r, err
}

// PublishRelease changes a release from draft to not draft. If the release
// does not exist an error is returned. If the release exists and is not a
// draft nothing happens and no error is returned.
func (c *client) PublishRelease(id ident) error {
	r, err := c.GetDraft(id)
	if err != nil {
		return fmt.Errorf("get release: %s", err)
	}

	if !r.GetDraft() {
		return nil
	}

	*r.Draft = false
	r, _, err = c.Repositories.EditRelease(ctxbg, id.org, id.repo, r.GetID(), r)
	return err
}

// CreateTag creates a tag on GitHub. If msg is blank a lightweight tag will be
// created. If the tag already exists, nothing happens. If the tag exists and
// does not resolve to the same commit sha, an error is returned.
func (c *client) CreateTag(id ident, sha, msg string) error {
	refstr := "tags/" + id.tag
	ref, rsp, err := c.Git.GetRef(ctxbg, id.org, id.repo, refstr)
	if rsp.StatusCode != http.StatusNotFound {
		if err != nil {
			return err
		}
		if sha == ref.GetObject().GetSHA() {
			return nil
		}
		if msg == "" {
			return errors.New("ref " + refstr + " exists on github and the sha is incorrect")
		}
		t, rsp, err := c.Git.GetTag(ctxbg, id.org, id.repo, ref.GetObject().GetSHA())
		if rsp.StatusCode != http.StatusNotFound {
			if err != nil {
				return err
			}
			if sha != t.GetObject().GetSHA() {
				return errors.New("tag " + id.tag + " exists on github and the sha is incorrect")
			}
		}
		return nil
	}
	err = nil

	_, rsp, err = c.Repositories.GetCommit(ctxbg, id.org, id.repo, sha)
	if err != nil {
		if rsp.StatusCode == 422 {
			return fmt.Errorf("create tag %s: sha %s not found, is the commit pushed?",
				id.String(), sha)
		}
		return fmt.Errorf("create tag: verify sha %s: %s", sha, err)
	}

	obj := &github.GitObject{SHA: &sha, Type: github.String("commit")}
	if msg != "" {
		pld := &github.Tag{
			Tag:     &id.tag,
			Object:  obj,
			Message: &msg,
		}
		t, _, err := c.Git.CreateTag(ctxbg, id.org, id.repo, pld)
		if err != nil {
			return fmt.Errorf("create annotated tag: %s", err)
		}
		obj.SHA = t.SHA
	}

	pld := &github.Reference{
		Ref:    &refstr,
		Object: obj,
	}
	_, _, err = c.Git.CreateRef(ctxbg, id.org, id.repo, pld)
	if err != nil {
		return fmt.Errorf("create tag ref: %s", err)
	}
	return nil
}

// GlobAssets returns a slice of assets or an error and filters the result by
// using the ident as a glob (filepath.Match).
func (c *client) GlobAssets(id ident) ([]asset, error) {
	r, err := c.GetRelease(id)
	if err != nil {
		return []asset{}, fmt.Errorf("get asset: %s", err)
	}

	id.tag = r.GetTagName()
	as := []asset{}
	for _, a := range r.Assets {
		ok, err := filepath.Match(id.asset, a.GetName())
		if err != nil {
			return []asset{}, fmt.Errorf("%s: %s", id, err)
		}
		if !ok {
			continue
		}

		nid := ident{
			org:   id.org,
			repo:  id.repo,
			tag:   id.tag,
			asset: a.GetName(),
			dst:   id.dst,
		}
		if nid.dst == "" {
			nid.dst = nid.asset
		}

		as = append(as, asset{a, r, nid})
	}
	if len(as) == 0 {
		return as, errNotFound{id}
	}
	return as, nil
}

// List tags lists all the tag refs for a repo.
func (c *client) ListTags(id ident) ([]string, error) {
	ts, _, err := c.Repositories.ListTags(ctxbg, id.org, id.repo,
		&github.ListOptions{Page: 0})
	if err != nil {
		return []string{}, err
	}
	ss := make([]string, len(ts))
	for i, t := range ts {
		ss[i] = t.GetName()
	}
	return ss, nil
}

// downer performs downloaads using parallel workers. Call queue(dir, as) to
// append a slice of assets to download, such as returned by client.GlobAssets.
// Call wait() to wait on the workers and collect any errors.
// Attempting to queue after a wait will cause a panic.
type downer struct {
	c     *client
	queue func(string, []asset)
	wait  func() []error
}

// newDowner creates a new downer using a client and a number of
// parallel workers. Calling newDowner starts the worker pool.
func newDowner(c *client, wkrs int) downer {
	type dl struct {
		dir string
		a   asset
	}

	dlc := make(chan dl)
	done := make(chan struct{})
	errs, eall := erraggr()

	d := downer{
		c: c,
		queue: func(dir string, as []asset) {
			for _, a := range as {
				dlc <- dl{dir, a}
			}
		},
		wait: func() []error {
			close(dlc)
			for i := 0; i < wkrs; i++ {
				<-done
			}
			return <-eall
		},
	}

	for i := 0; i < wkrs; i++ {
		go func() {
			for v := range dlc {
				errs <- d.download(v.dir, v.a)
			}
			done <- struct{}{}
		}()
	}

	return d
}

// download is called by workers for the downer
// don't call this directly! use d.queue(dir, as)
func (d *downer) download(dir string, a asset) error {
	log.Printf("get %s", a.id)
	rc, rd, err := d.c.Repositories.DownloadReleaseAsset(ctxbg,
		a.id.org, a.id.repo, a.GetID())
	if err != nil {
		return fmt.Errorf("download %s: %s", a.id, err)
	}

	if rc == nil {
		rsp, err := http.Get(rd)
		if err != nil {
			return fmt.Errorf("download redirect %s: %s", a.id, err)
		}
		rc = rsp.Body
	}
	defer rc.Close()

	w := os.Stdout
	if dir != "\x00" {
		f, err := os.Create(filepath.Join(dir, a.id.dst))
		if err != nil {
			return fmt.Errorf("download create %s: %s", a.id, err)
		}
		defer f.Close()
		w = f
	}

	_, err = io.Copy(w, rc)
	if err != nil {
		return fmt.Errorf("download copy %s: %s", a.id, err)
	}
	return err
}

// upper performs uploaads using parallel workers. Call queue(dst, src) to append
// an upload job. Call wait() to wait on the workers and collect any errors.
// Attempting to queue after a wait will cause a panic.
type upper struct {
	c     *client
	r     *github.RepositoryRelease
	id    ident
	queue func(string, string)
	wait  func() []error
}

// newUpper creates a new upper for a release using a client and a number of
// parallel workers. Calling newUpper starts the worker pool.
func newUpper(c *client, wkrs int, id ident, r *github.RepositoryRelease) upper {
	type ul struct {
		dst string
		src string
	}

	ulc := make(chan ul)
	done := make(chan struct{})
	errs, eall := erraggr()

	u := upper{
		c:  c,
		r:  r,
		id: id,
		queue: func(dst, src string) {
			ulc <- ul{dst, src}
		},
		wait: func() []error {
			close(ulc)
			for i := 0; i < wkrs; i++ {
				<-done
			}
			return <-eall
		},
	}

	for i := 0; i < wkrs; i++ {
		go func() {
			for v := range ulc {
				errs <- u.upload(v.dst, v.src)
			}
			done <- struct{}{}
		}()
	}

	return u
}

// upload is called by workers for the upper
// don't call this directly! use u.queue(dst, src)
func (u *upper) upload(dst string, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}

	for _, a := range u.r.Assets {
		if dst != a.GetName() {
			continue
		}
		if st.Size() != int64(a.GetSize()) {
			return errors.New("release asset " + u.id.tag + " " + dst + " exists and is a different size to " + src)
		}
		return nil
	}

	_, _, err = u.c.Repositories.UploadReleaseAsset(ctxbg,
		u.id.org, u.id.repo, u.r.GetID(),
		&github.UploadOptions{Name: dst}, f)
	return err
}

type errNotFound struct {
	ident
}

func isNotFound(err error) bool {
	_, ok := err.(errNotFound)
	return ok
}

func (e errNotFound) Error() string {
	return fmt.Sprintf("%s was not found", e.ident.String())
}

type errNoReleases struct {
	ident
}

func isNoReleases(err error) bool {
	_, ok := err.(errNoReleases)
	return ok
}

func (e errNoReleases) Error() string {
	return fmt.Sprintf("%s has no releases", e.ident.String())
}

// regexp pattern for ident
const (
	idSlugPart = `(?:([\d\w_-]+)/)?`
	idRepoPart = `([\d\w_-]+)`
	idTagPart  = `(?:@([\d\w\._-]+))?`
	idGlobPart = `(?::([\d\w\.\*\?\[\]\^_-]+))?`
	idFilePart = `(?::([\d\w\._-]+))?`
	idRe       = "^" + idSlugPart + idRepoPart + idTagPart + idGlobPart + idFilePart + "$"
)

// regexp for ident
var (
	idRx     = regexp.MustCompile(idRe)
	noGlobRx = regexp.MustCompile(`^[\d\w\._-]+$`)
)

// ident can identify a repo, tag, or asset and destination name.
type ident struct {
	org, repo, tag, asset, dst string
}

func parseId(s string) (ident, bool) {
	ms := idRx.FindStringSubmatch(s)
	if len(ms) != 6 {
		return ident{}, false
	}
	id := ident{ms[1], ms[2], ms[3], ms[4], ms[5]}
	if id.org == "" {
		id.org = defaultOrg
	}
	if id.org == "" {
		log.Printf("%s has no org and HUBR_DEFAULT_ORG is not set", s)
		return ident{}, false
	}
	if id.tag == "" {
		id.tag = defaultTag
	}
	glob := !noGlobRx.MatchString(id.asset)
	switch {
	case glob && id.dst != "":
		return ident{}, false
	case !glob && id.dst == "":
		id.dst = id.asset
	}

	return id, true
}

func (id ident) String() string {
	s := id.org + "/" + id.repo
	if id.tag != defaultTag {
		s += "@" + id.tag
	}
	if id.asset != "" {
		s += ":" + id.asset
	}
	if id.dst != id.asset && id.dst != "" {
		s += ":" + id.dst
	}
	return s
}

// increment is a semver increment
type increment int

const (
	noinc increment = iota
	major
	minor
	patch
	allinc
)

// parseIncrement converts a string to an increment.
func parseIncrement(s string) (increment, error) {
	i := map[string]increment{
		"major": major, "minor": minor, "patch": patch,
	}[s]
	if i == noinc {
		return i, errors.New("not an increment: " + s)
	}
	return i, nil
}

// String returns a string representation of the increment.
func (i increment) String() string {
	return map[increment]string{
		noinc: "invalid", major: "major", minor: "minor", patch: "patch",
	}[i]
}

// spec is a set of parameters to create or update a release.
type spec struct {
	id                ident
	draft, pre, keepd bool
	sha, name, body   string
	uploads           []string
	wkrs              int
}

// release does exactly what it says. A tag is created if one does not
// exist. A release is created if one does not exist. Files listed in uploads
// are uploaded.
func (s spec) release() error {
	c, err := NewClient()
	if err != nil {
		return err
	}

	err = c.CreateTag(s.id, s.sha, "release "+s.name)
	if err != nil {
		return fmt.Errorf("tag: %s", err)
	}

	r, err := c.DraftRelease(s.id, s.name, s.body, s.pre)
	if err != nil {
		return fmt.Errorf("draft release: %s", err)
	}

	if len(s.uploads) > 0 {
		u := newUpper(c, s.wkrs, s.id, r)
		for _, src := range s.uploads {
			dst := src
			if !s.keepd {
				dst = filepath.Base(src)
			}
			u.queue(dst, src)
			log.Print("uploading ", src)
		}
		errs := u.wait()
		if len(errs) > 0 {
			for _, err := range errs {
				log.Print(err)
			}
			return errors.New("uploads failed")
		}
	}

	if s.draft {
		log.Print(s.id.repo, " ", s.id.tag, " draft release updated")
		return nil
	}

	err = c.PublishRelease(s.id)
	if err != nil {
		return fmt.Errorf("publish release: %s", err)
	}

	octolog(c, s.id.String()+" released!")
	return nil
}

// versionRe matches a semver of the form 0.0.0 with any prefix or suffix.
var versionRe = regexp.MustCompile(`(\d+)(?:\.(\d+))?(?:\.(\d+))?`)

// version is a semver version of the form 0.0.0 with any prefix or suffix
type version string

// parseVersion converts a string into a version. It returns an error if s does
// not match the version regexp.
func parseVersion(s string) (version, error) {
	if !versionRe.MatchString(s) {
		return version(""), errors.New("version does not match 0.0.0 pattern")
	}
	return version(s), nil
}

// bump returns a new version incremented as instructed.
func (v version) bump(incr increment) version {
	now := string(v)

	ms := versionRe.FindStringSubmatch(now)
	if len(ms) != 4 {
		now = "v0.0.0"
		ms = []string{"0.0.0", "0", "0", "0"}
	}

	i, _ := strconv.Atoi(ms[incr])
	i++
	ms[incr] = strconv.Itoa(i)
	for i := incr + 1; i < allinc; i++ {
		ms[i] = "0"
	}
	next := ms[major] + "." + ms[minor] + "." + ms[patch]

	l := versionRe.FindStringIndex(now)
	return version(now[:l[0]] + next + now[l[1]:])
}

// isBefore returns true if v is an earlier version than u. Any prefixes or
// suffixes are ignored.
func (v version) isBefore(u version) bool {
	vs := versionRe.FindStringSubmatch(string(v))
	if len(vs) != 4 {
		vs = []string{"0.0.0", "0", "0", "0"}
	}
	us := versionRe.FindStringSubmatch(string(u))
	if len(us) != 4 {
		us = []string{"0.0.0", "0", "0", "0"}
	}

	for i := major; i < allinc; i++ {
		v, _ := strconv.Atoi(vs[i])
		u, _ := strconv.Atoi(us[i])
		if v > u {
			return false
		}
		if v < u {
			return true
		}
	}
	return false
}

// String returns the version string or v0.0.0 if the string is empty.
func (v version) String() string {
	if v == "" {
		return "v0.0.0"
	}
	return strings.TrimRight(string(v), "\n")
}

// versioner sifts through a local git repo for version information.
type versioner struct {
	*git.Repository
	path string
}

// newVersioner returns a versioner for a local git repo using the given file
// path of the VERSION file in the repository. The working directory must be
// inside a git repository.
func newVersioner(path string) (versioner, error) {
	r, err := git.PlainOpenWithOptions(".", &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return versioner{}, err
	}
	return versioner{r, path}, nil
}

// head returns the value of the VERSION file at HEAD.
func (vr versioner) head() (version, error) {
	var v version

	head, err := vr.Head()
	if err != nil {
		return v, err
	}

	c, err := vr.CommitObject(head.Hash())
	if err != nil {
		return v, err
	}

	return vr.at(c)
}

// at returns the value of the VERSION file at c.
func (vr versioner) at(c *object.Commit) (version, error) {
	var v version

	t, err := c.Tree()
	if err != nil {
		return v, err
	}

	f, err := t.File(vr.path)
	if err == object.ErrFileNotFound {
		return v, nil
	}
	if err != nil {
		return v, err
	}

	s, err := f.Contents()
	if err != nil {
		return v, err
	}

	l := strings.SplitN(s, "\n", 2)
	return parseVersion(l[0])
}

// logDiff returns the additions made to the version file in the last commit.
func (vr versioner) logDiff() ([]string, error) {
	h, err := vr.Head()
	if err != nil {
		return []string{}, err
	}
	hc, err := vr.CommitObject(h.Hash())
	if err != nil {
		return []string{}, err
	}

	switch hc.NumParents() {
	case 0:
		return []string{}, nil
	case 1:
	default:
		return []string{}, errors.New("head is a merge commit; merge commits cannot be releases")
	}

	ht, err := hc.Tree()
	if err != nil {
		return []string{}, err
	}

	pc, err := hc.Parent(0)
	if err != nil {
		return []string{}, err
	}

	pt, err := pc.Tree()
	if err != nil {
		return []string{}, err
	}

	ss := []string{}
	chs, err := pt.Diff(ht)
	for _, ch := range chs {
		p, err := ch.Patch()
		if err != nil {
			return []string{}, err
		}
		for _, fp := range p.FilePatches() {
			if fp.IsBinary() {
				continue
			}
			if _, to := fp.Files(); to.Path() != vr.path {
				continue
			}
			for _, c := range fp.Chunks() {
				if c.Type() == diff.Add {
					ss = append(ss, c.Content())
				}
			}
		}
	}
	return ss, nil
}

// files returns a map of files and directories that have changed since the
// last release.
func (vr versioner) files() (map[string]bool, error) {
	fs := map[string]bool{}

	h, err := vr.Head()
	if err != nil {
		return fs, fmt.Errorf("head: %s", err)
	}
	hc, err := vr.CommitObject(h.Hash())
	if err != nil {
		return fs, fmt.Errorf("head commit: %s", err)
	}

	ok, err := vr.isRelease()
	if err != nil {
		return fs, fmt.Errorf("head is release: %s", err)
	}

	var vbase version
	switch ok {
	case true:
		pc, err := hc.Parent(0)
		if err != nil {
			return fs, fmt.Errorf("head commit: %s", err)
		}
		vbase, err = vr.at(pc)
	default:
		vbase, err = vr.at(hc)
	}
	if err != nil {
		return fs, fmt.Errorf("base version: %s", err)
	}

	snd, rcv := passCommits()
	snd <- hc

	var cmt *object.Commit
	for c := range rcv {
		switch {
		case c == nil:
			continue
		case c.NumParents() == 0:
		case c.NumParents() == 1:
			v, err := vr.at(c)
			if err != nil {
				return fs, fmt.Errorf("version of %s: %s", c.Hash.String(), err)
			}
			if v.isBefore(vbase) {
				continue
			}
			p, err := c.Parent(0)
			if err != nil {
				return fs, fmt.Errorf("parent of %s: %s", c.Hash.String(), err)
			}
			snd <- p
		default:
			err := c.Parents().ForEach(func(c *object.Commit) error {
				v, err := vr.at(c)
				if err != nil {
					return err
				}
				if v.isBefore(vbase) {
					return nil
				}
				snd <- c
				return nil
			})
			if err != nil {
				return fs, fmt.Errorf("merge %s: %s", c.Hash.String(), err)
			}
		}
		cmt = c
	}

	if cmt == nil {
		return fs, fmt.Errorf("cant get commits. missing VERSION?")
	}

	ht, err := hc.Tree()
	if err != nil {
		return fs, fmt.Errorf("head tree: %s", err)
	}

	ct, err := cmt.Tree()
	if err != nil {
		return fs, fmt.Errorf("commit tree: %s", err)
	}

	cs, err := ht.Diff(ct)
	if err != nil {
		return fs, fmt.Errorf("diff: %s", err)
	}

	put := func(ss ...string) {
		for _, s := range ss {
			if s == "" {
				continue
			}
			fs[s] = true
			for {
				d := path.Dir(s)
				if d == "." || d == "/" {
					break
				}
				s = d
				fs[s] = true
			}
		}
	}

	for _, c := range cs {
		put(c.From.Name, c.To.Name)
	}
	return fs, nil
}

// isRelease returns true if the version has changed in the HEAD commit.
func (vr versioner) isRelease() (bool, error) {
	head, err := vr.Head()
	if err != nil {
		return false, err
	}

	hc, err := vr.CommitObject(head.Hash())
	if err != nil {
		return false, err
	}

	switch hc.NumParents() {
	case 1:
	case 0:
		// no parent probably counts as a kind of release
		return true, nil
	default:
		// merge commits cannot be releases
		return false, nil
	}

	hv, err := vr.at(hc)
	if err != nil {
		return false, err
	}

	pc, err := hc.Parent(0)
	if err != nil {
		return false, err
	}

	pv, err := vr.at(pc)
	if err != nil {
		return false, err
	}

	return hv != pv, nil
}

// lastLog returns the content of the version file at HEAD.
func (vr versioner) lastLog() (string, error) {
	h, err := vr.Head()
	if err != nil {
		return "", err
	}

	c, err := vr.CommitObject(h.Hash())
	if err != nil {
		return "", err
	}

	t, err := c.Tree()
	if err != nil {
		return "", err
	}

	f, err := t.File(vr.path)
	if err == object.ErrFileNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}

	return f.Contents()
}

// logHead creates a changelog from the previous release up to HEAD. The log is
// very complicated.
//
// First, a mainline of commits is calculated. If a parent of a merge commit has
// the same version as the child, it is considered mainline.
//
// After the mainline is calculated, the log is constructed from the commit
// messages of the mainline up to and not including the previous release commit.
//
// Any branches encountered during the second traversal are tracked back to the
// mainline and their commit messages are inserted into the log.
func (vr versioner) logHead() ([]string, error) {
	h, err := vr.Head()
	if err != nil {
		return []string{}, err
	}

	hc, err := vr.CommitObject(h.Hash())
	if err != nil {
		return []string{}, err
	}

	ml, err := vr.mainline(hc)
	if err != nil {
		return []string{}, err
	}

	return vr.logMain(hc, ml)
}

// logMain constructs a changelog starting from c along the mainline ml.  The
// log is constructed from the commit messages of the mainline up to and not
// including the previous release commit.
//
// Any branches encountered during the second traversal are tracked back to the
// mainline and their commit messages are inserted into the log.
func (vr versioner) logMain(c *object.Commit, ml map[plumbing.Hash]bool) ([]string, error) {
	snd, rcv := passCommits()
	snd <- c

	msgs := []string{}
	for c := range rcv {
		switch {
		case c == nil:
		case c.NumParents() == 0:
			msgs = append(msgs, c.Message)
		case c.NumParents() == 1:
			cv, err := vr.at(c)
			if err != nil {
				return msgs, err
			}
			pc, err := c.Parent(0)
			if err != nil {
				return msgs, err
			}
			pv, err := vr.at(pc)
			if err != nil {
				return msgs, err
			}
			if cv != pv {
				continue
			}
			msgs = append(msgs, c.Message)
			snd <- pc
		default:
			msgs = append(msgs, c.Message)
			err := c.Parents().ForEach(func(c *object.Commit) error {
				switch {
				case ml[c.Hash]:
					snd <- c
				default:
					msgs = append(msgs, vr.logBranch(c, ml)...)
				}
				return nil
			})
			if err != nil {
				return msgs, err
			}
		}
	}
	return msgs, nil
}

// logBranch constructs a branch changelog starting from c back to the mainline
// ml. It returns a slice of all commit messages on the branch.
func (vr versioner) logBranch(c *object.Commit, ml map[plumbing.Hash]bool) []string {
	snd, rcv := passCommits()
	snd <- c

	ss := []string{}
	for c := range rcv {
		if c == nil {
			continue
		}

		if ml[c.Hash] {
			continue
		}

		ss = append(ss, c.Message)

		if c.NumParents() == 0 {
			continue
		}

		c.Parents().ForEach(func(c *object.Commit) error {
			snd <- c
			return nil
		})
	}
	return ss
}

// mainline traverses commits from c and returns a map of commit hashes which
// are considered the mainline. For any merge commit encountered, its parents
// are considered mainline if they have the same version as the child.
// Note: doing things this way is probably not sustainable. But it's a start.
func (vr versioner) mainline(c *object.Commit) (map[plumbing.Hash]bool, error) {
	snd, rcv := passCommits()
	snd <- c

	ml := map[plumbing.Hash]bool{}
	for c := range rcv {
		if c == nil {
			continue
		}

		if ml[c.Hash] {
			continue
		}

		np := c.NumParents()
		if np == 0 {
			ml[c.Hash] = true
			continue
		}

		cv, err := vr.at(c)
		if err != nil {
			return nil, err
		}

		err = c.Parents().ForEach(func(c *object.Commit) error {
			if np > 1 {
				pv, err := vr.at(c)
				if err != nil {
					return err
				}
				if cv != pv {
					return nil
				}
			}

			snd <- c
			return nil
		})
		ml[c.Hash] = true
		if err != nil {
			return ml, err
		}
	}
	return ml, nil
}

// hubr assemble! setup the subcmds and maybe invoke one.
func main() {
	subs := map[string]struct {
		fn  func([]string) error
		use string
	}{
		"assets":  {assets, "list release assets"},
		"bump":    {bump, "create a new version"},
		"cat":     {cat, "print release asset contents"},
		"get":     {get, "download release assets"},
		"install": {install, "install binary or zip assets"},
		"now":     {now, "test for a release commit"},
		"push":    {push, "release using version file"},
		"release": {release, "release by tag"},
		"resolve": {resolve, "resolve a tag"},
		"say":     {say, "octocat says"},
		"tags":    {tags, "list release tags"},
		"what":    {what, "list or check file changes"},
		"who":     {who, "get token user"},
	}
	flag.Usage = func() {
		o := flag.CommandLine.Output()
		fmt.Fprintf(o, helpMain, os.Args[0], os.Args[0])
		fmt.Fprintln(o, "\nOptions:")
		flag.PrintDefaults()
		// print the subcmds in a style matching the flag package
		fmt.Fprintln(o, "\nCommands:")
		// this slice hides hidden/utility subs from the main help output
		ks := []string{"assets", "bump", "cat", "get", "install", "now",
			"push", "release", "resolve", "tags", "what", "who"}
		for _, k := range ks {
			fmt.Fprintf(o, "  %s\n    \t%s\n", k, subs[k].use)
		}
		fmt.Fprintln(o)
	}

	v := flag.Bool("v", false, "print version on standard output and exit")
	flag.Parse()
	if *v {
		fmt.Println(hubr + "-" + runtime.GOOS + "-" + runtime.GOARCH)
		os.Exit(0)
	}
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	sub, ok := subs[flag.Arg(0)]
	if !ok {
		fmt.Fprintf(flag.CommandLine.Output(), "unrecognised command: %q\n\n", flag.Arg(0))
		flag.Usage()
		os.Exit(2)
	}

	log.SetFlags(0)
	if err := sub.fn(flag.Args()[1:]); err != nil {
		log.Fatal(err)
	}
}

// Subcmd assets lists release assets for one or more GitHub releases.
// With no flags, assets will be listed three per line.
// With -l, one asset per line with content type, size and label
// If more than one identifier is provided, headings are included.
func assets(args []string) error {
	f := flag.NewFlagSet("assets", flag.ExitOnError)
	f.Usage = usageFor(f)
	list := f.Bool("l", false, "one per line, with description")
	f.Parse(args)

	args, err := readArgs(f.Args())
	if err != nil {
		log.Print(err)
		f.Usage()
		os.Exit(2)
	}

	if len(args) == 0 {
		f.Usage()
		os.Exit(2)
	}

	c, err := NewClient()
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 16, 8, 2, ' ', 0)
	for _, arg := range args {
		id, ok := parseId(arg)
		if !ok {
			return errors.New("failed to parse " + arg + ", does not match " + helpOrgPart + "<repo>[@<tag>]")
		}

		r, err := c.GetRelease(id)
		if err != nil {
			return err
		}

		id.tag = r.GetTagName()
		if len(args) > 1 {
			io.WriteString(w, id.String()+":\n")
		}
		if id.asset == "" {
			id.asset = "*"
		}

		i := 0
		for _, a := range r.Assets {
			ok, err := filepath.Match(id.asset, a.GetName())
			if err != nil {
				return fmt.Errorf("%s is not a valid glob pattern", id.asset)
			}
			if !ok {
				continue
			}
			switch {
			case *list:
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", a.GetName(),
					a.GetContentType(), a.GetSize(), a.GetLabel())
			default:
				i++
				n := "\t"
				// format into 3 columns
				if i%3 == 2 {
					n = "\n"
				}
				io.WriteString(w, a.GetName()+n)
			}
		}
		if !*list && i%3 != 2 {
			io.WriteString(w, "\n")
		}

		if len(args) > 1 {
			io.WriteString(w, "\n")
		}
	}

	return w.Flush()
}

// Subcmd bump creates a new version.
func bump(args []string) error {
	f := flag.NewFlagSet("bump", flag.ExitOnError)
	f.Usage = usageFor(f)
	latest := f.String("latest", "", "use latest release of `"+helpOrgPart+"<repo>` (default version file)")
	vfile := f.String("v", "VERSION", "path to the version file in the repository")
	write := f.Bool("w", false, "write to the version file (default stdout)")
	nolog := f.Bool("n", false, "print the version only, not the log")
	f.Parse(args)

	if f.NArg() != 1 {
		f.Usage()
		os.Exit(2)
	}

	inc, err := parseIncrement(f.Arg(0))
	if err != nil {
		log.Printf("parse version increment: %s", err)
		f.Usage()
		os.Exit(2)
	}

	var (
		v    version
		last string
		msgs = []string{}
	)
	switch *latest {
	case "":
		vr, err := newVersioner(*vfile)
		if err != nil {
			return fmt.Errorf("open local repository: %s", err)
		}
		u, err := vr.head()
		if err != nil {
			return fmt.Errorf("get latest version: %s", err)
		}
		v = u
		if *nolog {
			break
		}
		ss, err := vr.logHead()
		if err != nil {
			return fmt.Errorf("calculate log: %s", err)
		}
		msgs = ss
		s, err := vr.lastLog()
		if err != nil {
			return fmt.Errorf("get committed version file contents: %s", err)
		}
		last = s
	default:
		id, ok := parseId(*latest)
		if !ok {
			log.Printf("%s does not match "+helpOrgPart+"<repo>", *latest)
			f.Usage()
			os.Exit(2)
		}
		c, err := NewClient()
		if err != nil {
			return err
		}
		r, err := c.GetRelease(id)
		if err != nil {
			return err
		}
		u, err := parseVersion(r.GetTagName())
		if err != nil {
			return err
		}
		v = u
		if *nolog {
			break
		}
		id.tag = v.String()
		msgs = []string{"bumped from " + id.String()}
	}

	v = v.bump(inc)
	var w io.Writer

	switch {
	case *write:
		dir, err := locateGitDir(".")
		if err != nil {
			return fmt.Errorf("locate .git: %s", err)
		}

		of, err := os.Create(filepath.Join(dir, *vfile))
		if err != nil {
			return fmt.Errorf("write version file: %s", err)
		}
		defer of.Close()
		w = of
	default:
		w = os.Stdout
	}

	fmt.Fprintln(w, v.String())
	if *nolog {
		return nil
	}

	if len(msgs) > 0 {
		fmt.Fprintln(w)
		for _, msg := range msgs {
			b := "- "
			for _, l := range strings.Split(msg, "\n") {
				if l == "" {
					continue
				}
				fmt.Fprintln(w, b+l)
				b = "  "
			}
		}
		fmt.Fprintln(w)
	}

	if *write && last != "" {
		fmt.Fprint(w, "\n", last)
	}
	return nil
}

// Subcmd cat downloads one or more assets and writes one at a time to stdout.
// A distinction is made from subcmd get which is not safe to write to stdout.
func cat(args []string) error {
	f := flag.NewFlagSet("cat", flag.ExitOnError)
	f.Usage = usageFor(f)
	f.Parse(args)

	args, err := readArgs(f.Args())
	if err != nil {
		log.Print(err)
		f.Usage()
		os.Exit(2)
	}

	if len(args) == 0 {
		f.Usage()
		os.Exit(2)
	}

	c, err := NewClient()
	if err != nil {
		return err
	}

	d := newDowner(c, 1)
	for _, arg := range args {
		id, _ := parseId(arg)
		if id.asset == "" {
			return errors.New("failed to parse " + arg + ", does not match " + helpOrgPart + "<repo>[@<tag>]:<asset>[:<dst>]")
		}
		as, err := c.GlobAssets(id)
		if err != nil {
			return err
		}
		d.queue("\x00", as)
	}

	errs := d.wait()
	if len(errs) > 0 {
		for _, err := range errs {
			log.Print(err)
		}
		return errors.New("get failed")
	}

	return nil
}

// Subcmd get downloads one or more assets to the working directory.
func get(args []string) error {
	f := flag.NewFlagSet("get", flag.ExitOnError)
	dir := f.String("d", ".", "output `dir`ectory")
	wkr := f.Int("w", workers, "number of download workers")
	f.Usage = usageFor(f)
	f.Parse(args)

	args, err := readArgs(f.Args())
	if err != nil {
		log.Print(err)
		f.Usage()
		os.Exit(2)
	}

	if len(args) == 0 {
		f.Usage()
		os.Exit(2)
	}

	c, err := NewClient()
	if err != nil {
		return err
	}

	d := newDowner(c, *wkr)
	for _, arg := range args {
		id, _ := parseId(arg)
		if id.asset == "" {
			return errors.New("failed to parse " + arg + ", does not match " + helpOrgPart + "<repo>[@<tag>]:<asset>[:<dest>]")
		}
		as, err := c.GlobAssets(id)
		if err != nil {
			return err
		}
		d.queue(*dir, as)
	}

	errs := d.wait()
	if len(errs) > 0 {
		for _, err := range errs {
			log.Print(err)
		}
		return errors.New("get failed")
	}

	return nil
}

// Subcmd install downloads one or more assets and installs based on content-type.
// For application/zip, any executables in the zip file will be installed.
// For application/octet-stream, the file is made executable.
// No further types are supported.
func install(args []string) error {
	f := flag.NewFlagSet("install", flag.ExitOnError)
	f.Usage = usageFor(f)
	dir := f.String("d", ".", "install `dir`ectory")
	wkr := f.Int("w", workers, "number of download workers")
	f.Parse(args)

	args, err := readArgs(f.Args())
	if err != nil {
		log.Print(err)
		f.Usage()
		os.Exit(2)
	}

	if len(args) == 0 {
		f.Usage()
		os.Exit(2)
	}

	c, err := NewClient()
	if err != nil {
		return err
	}

	// setup a temp directory for install operations
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("hubr-%d", time.Now().Unix()))
	if err := os.MkdirAll(tmp, 0755); err != nil {
		return fmt.Errorf("failed to create dir in %s: %s", os.TempDir(), err)
	}
	defer os.RemoveAll(tmp)

	ass := []asset{}
	d := newDowner(c, *wkr)
	for _, arg := range args {
		id, _ := parseId(arg)
		if id.asset == "" {
			return errors.New("failed to parse " + arg + ", does not match " + helpOrgPart + "<repo>[@<tag>]:<asset>[:<dest>]")
		}

		as, err := c.GlobAssets(id)
		if err != nil {
			return err
		}

		ass = append(ass, as...)
		d.queue(tmp, as)
	}

	errs := d.wait()
	if len(errs) > 0 {
		for _, err := range errs {
			log.Print(err)
		}
		return errors.New("download failed")
	}

	for _, a := range ass {
		src := filepath.Join(tmp, a.id.dst)
		dst := filepath.Join(*dir, a.id.dst)

		t := detectContentType(src)
		if t != a.GetContentType() {
			log.Printf("warning: content type mismatch: detected %s, github reported %s", t, a.GetContentType())
		}
		switch t {
		case "application/octet-stream":
			err = installBin(src, dst)
		case "application/zip":
			err = installZip(src, *dir)
		default:
			return fmt.Errorf("unsupported content type: %s", a.GetContentType())
		}
		if err != nil {
			return err
		}
	}

	return nil
}

// Subcmd now checks if head is a release commit.
func now(args []string) error {
	f := flag.NewFlagSet("now", flag.ExitOnError)
	f.Usage = usageFor(f)
	vfile := f.String("v", "VERSION", "path to the version file in the repository")
	f.Parse(args)

	vr, err := newVersioner(*vfile)
	if err != nil {
		return fmt.Errorf("open local repository: %s", err)
	}

	ok, err := vr.isRelease()
	if err != nil {
		return fmt.Errorf("check head: %s", err)
	}

	if !ok {
		return errors.New("not a release")
	}

	return nil
}

// Subcmd push creates a GitHub release for a release commit. Pushing is
// idempotent. If the current commit is not a release commit, nothing happens.
// If releases or release assets already exist, creation will nop.
func push(args []string) error {
	f := flag.NewFlagSet("push", flag.ExitOnError)
	f.Usage = usageFor(f)
	vfile := f.String("v", "VERSION", "path to the version file in the repository")
	draft := f.Bool("d", false, "leave as draft; do not publish release")
	keepd := f.Bool("f", false, "use the full file path for uploads (default basename only)")
	wkrs := f.Int("w", workers, "number of upload workers")
	f.Parse(args)

	if f.NArg() == 0 {
		f.Usage()
		os.Exit(2)
	}

	id, ok := parseId(f.Arg(0))
	if !ok || id.tag != defaultTag {
		log.Printf("failed to parse %s, does not match "+helpOrgPart+"<repo>", f.Arg(0))
		f.Usage()
		os.Exit(2)
	}
	uploads := f.Args()[1:]

	vr, err := newVersioner(*vfile)
	if err != nil {
		return fmt.Errorf("open local repository: %s", err)
	}

	ok, err = vr.isRelease()
	if err != nil {
		return fmt.Errorf("check release commit: %s", err)
	}
	if !ok {
		log.Print("push: nop, head is not a release commit")
		return nil
	}

	v, err := vr.head()
	if err != nil {
		return fmt.Errorf("get version of head: %s", err)
	}

	h, err := vr.Head()
	if err != nil {
		return fmt.Errorf("get head: %s", err)
	}

	chs, err := vr.logDiff()
	if err != nil {
		return fmt.Errorf("get changes: %s", err)
	}

	id.tag = v.String()
	return spec{
		id:      id,
		draft:   *draft,
		keepd:   *keepd,
		sha:     h.Hash().String(),
		name:    id.tag,
		body:    strings.Join(chs, "\n"),
		uploads: uploads,
		wkrs:    *wkrs,
	}.release()
}

// Subcmd release creates a GitHub release for a tag.
// If releases or release assets already exist, creation will nop.
func release(args []string) error {
	f := flag.NewFlagSet("release", flag.ExitOnError)
	f.Usage = usageFor(f)
	name := f.String("name", "", "release name (defaults to tag)")
	body := f.String("body", "", "release body string, or @file, or - to read from stdin")
	sha := f.String("sha", "", "sha of release commit (defaults to detect from tag or head)")
	draft := f.Bool("d", false, "leave as draft; do not publish release")
	keepd := f.Bool("f", false, "use the full file path for uploads (default basename only)")
	pre := f.Bool("pre", false, "create prerelease")
	wkrs := f.Int("w", workers, "number of upload workers")
	f.Parse(args)

	if f.NArg() == 0 {
		log.Print("release one or more arguments")
		f.Usage()
		os.Exit(2)
	}

	id, ok := parseId(f.Arg(0))
	if !ok || id.tag == defaultTag || id.tag == "stable" || id.tag == "edge" {
		log.Printf("failed to parse %s, does not match "+helpOrgPart+"<repo>@<tag>", f.Arg(0))
		f.Usage()
		os.Exit(2)
	}
	uploads := f.Args()[1:]

	switch {
	case *body == "-":
		b, err := ioutil.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		*body = string(b)
	case len(*body) == 0:
	case (*body)[0] == '@':
		b, err := ioutil.ReadFile((*body)[1:])
		if err != nil {
			return err
		}
		*body = string(b)
	}

	if *name == "" {
		*name = id.tag
	}

	if *sha == "" {
		vr, err := newVersioner("")
		if err != nil {
			return fmt.Errorf("open local repository: %s", err)
		}
		ref, err := vr.Tag(id.tag)
		switch err {
		case nil:
			obj, err := vr.TagObject(ref.Hash())
			switch err {
			case nil:
				*sha = obj.Target.String()
			case plumbing.ErrObjectNotFound:
				*sha = ref.Hash().String()
			default:
				return err
			}
		case plumbing.ErrObjectNotFound, git.ErrTagNotFound:
			h, err := vr.Head()
			if err != nil {
				return fmt.Errorf("get local head: %s", err)
			}
			*sha = h.Hash().String()
		default:
			return fmt.Errorf("local repository: %s", err)
		}
	}

	return spec{
		id:      id,
		draft:   *draft,
		pre:     *pre,
		keepd:   *keepd,
		sha:     *sha,
		name:    *name,
		body:    *body,
		uploads: uploads,
		wkrs:    *wkrs,
	}.release()
}

// Subcmd resolve resolves release tags.
func resolve(args []string) error {
	f := flag.NewFlagSet("resolve", flag.ExitOnError)
	f.Usage = usageFor(f)
	w := f.Bool("w", false, "print web urls")
	f.Parse(args)

	args, err := readArgs(f.Args())
	if err != nil {
		log.Print(err)
		f.Usage()
		os.Exit(2)
	}

	if len(args) == 0 {
		f.Usage()
		os.Exit(2)
	}

	c, err := NewClient()
	if err != nil {
		return err
	}

	for _, arg := range args {
		id, ok := parseId(arg)
		if !ok {
			return fmt.Errorf("failed to parse %s, does not match "+helpOrgPart+"<repo>[@<tag>]", f.Arg(0))
		}

		r, err := c.GetRelease(id)
		if err != nil {
			return fmt.Errorf("%s: %s", arg, err)
		}
		id.tag = r.GetTagName()
		switch {
		case *w:
			fmt.Println(r.GetHTMLURL())
		default:
			fmt.Println(id)
		}
	}
	return nil
}

// Subcmd say is a mystery, who knows what it truly does...
func say(args []string) error {
	c, err := NewClient()
	if err != nil {
		return err
	}
	octolog(c, strings.Join(args, " "))
	return nil
}

// Subcmd tags lists tags for a repo. By default only full release tags are listed.
// With the -a flag, prereleases, draft releases, annotated and lightweight tags are
// also printed.
// With the -l flag, tags a printed one per line with additional information.
func tags(args []string) error {
	f := flag.NewFlagSet("tags", flag.ExitOnError)
	f.Usage = usageFor(f)
	list := f.Bool("l", false, "one per line, with description")
	all := f.Bool("a", false, "list all including draft, pre-release and unreleased tags")
	la := f.Bool("la", false, "shorthand for -l -a")
	f.Parse(args)

	args, err := readArgs(f.Args())
	if err != nil {
		log.Print(err)
		f.Usage()
		os.Exit(2)
	}

	if len(args) == 0 {
		log.Print("tags requires at least one argument")
		f.Usage()
		os.Exit(2)
	}

	if *la {
		*list, *all = true, true
	}

	c, err := NewClient()
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 12, 8, 2, ' ', 0)
	for _, arg := range args {
		id, ok := parseId(arg)
		if !ok {
			return fmt.Errorf("failed to parse %s, does not match "+helpOrgPart+"<repo>", f.Arg(0))
		}

		// get the releases, map them by tag, then get all the tags
		rs, err := c.ListReleases(id)
		if err != nil {
			return err
		}
		m := make(map[string]*github.RepositoryRelease, len(rs))
		for _, r := range rs {
			m[r.GetTagName()] = r
		}
		ts, err := c.ListTags(id)
		if err != nil {
			return err
		}

		if len(args) > 1 {
			// headings for multiple args
			io.WriteString(w, id.String()+":\n")
		}

		i := 0
		for _, t := range ts {
			r, ok := m[t]
			switch {
			case !*all && (!ok || r.GetDraft() || r.GetPrerelease()):
				continue
			case *list && ok:
				io.WriteString(w, r.GetTagName()+"\trelease\t"+r.GetCreatedAt().Format("2006-01-02 15:04 MST"))
				if r.GetPrerelease() {
					io.WriteString(w, "\tpre-release")
				}
				if r.GetDraft() {
					io.WriteString(w, "\tdraft")
				}
				io.WriteString(w, "\n")
			case *list && *all:
				io.WriteString(w, t+"\ttag\n")
			default:
				n := "\t"
				if i%5 == 4 {
					n = "\n"
				}
				io.WriteString(w, t+n)
				i++
			}
		}
		if i%5 != 0 {
			io.WriteString(w, "\n")
		}
		if len(args) > 1 {
			io.WriteString(w, "\n")
		}
	}
	return w.Flush()
}

// Subcmd what lists the files that have changed or checks if named files have
// changed since the previous release commit.
func what(args []string) error {
	f := flag.NewFlagSet("what", flag.ExitOnError)
	vfile := f.String("v", "VERSION", "path to the version file in the repository")
	all := f.Bool("all", false, "return success if all named files changed (default any)")
	f.Usage = usageFor(f)
	f.Parse(args)

	vr, err := newVersioner(*vfile)
	if err != nil {
		return fmt.Errorf("open local repository: %s", err)
	}

	fs, err := vr.files()
	if err != nil {
		return err
	}

	if f.NArg() == 0 {
		ss := []string{}
		for s := range fs {
			ss = append(ss, s)
		}
		sort.Strings(ss)
		for _, s := range ss {
			fmt.Println(s)
		}
		return nil
	}

	nc := []string{}
	for _, arg := range f.Args() {
		p := path.Clean(arg)
		if *all {
			if !fs[p] {
				nc = append(nc, p)
			}
			continue
		}
		if fs[p] {
			return nil
		}
	}
	if !*all {
		return errors.New("no changes detected")
	}
	if len(nc) > 0 {
		return errors.New("no changes detected: " + strings.Join(nc, ", "))
	}
	return nil
}

// Subcmd who prints the owner of the GitHub personal access token used by hubr.
func who(args []string) error {
	c, err := NewClient()
	if err != nil {
		return err
	}
	u, _, err := c.Users.Get(ctxbg, "")
	if err != nil {
		return err
	}
	fmt.Println(u.GetLogin())
	return nil
}

// detectContentType determines the mime type of the file at path.
func detectContentType(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	b := make([]byte, 512)
	_, err = f.Read(b)
	if err != nil {
		return ""
	}

	return http.DetectContentType(b)
}

// credHelper attempts to invoke a git credential helper by parsing git config
// first in a local repository if present and then in the home directory.
// if anything goes wrong it returns an empty string.
func credHelper() string {
	find := func(p string) string {
		f, err := os.Open(p)
		if err != nil {
			return ""
		}
		defer f.Close()
		var c config.Config
		if err := config.NewDecoder(f).Decode(&c); err != nil {
			return ""
		}
		for _, s := range c.Sections {
			if s.Name != "credential" {
				continue
			}
			for _, o := range s.Options {
				if o.Key == "helper" {
					return o.Value
				}
			}
		}
		return ""
	}

	var h string
	d, err := locateGitDir(".")
	if err == nil {
		h = find(filepath.Join(d, ".git", "config"))
	}
	if h == "" {
		h = find(filepath.Join(os.Getenv("HOME"), ".gitconfig"))
	}
	if h == "" {
		return ""
	}

	// see https://git-scm.com/docs/gitcredentials#gitcredentials-helper
	if h[0] != filepath.Separator {
		h = "git credential-" + h
	}
	h = h + " get"

	// using sh seems to be the easiest way to deal with the helper string
	// without having to split the args
	r, w := io.Pipe()
	cmd := exec.Command("/bin/sh", "-c", h)
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n")
	cmd.Stdout = w
	if err := cmd.Start(); err != nil {
		return ""
	}

	var t string
	s := bufio.NewScanner(r)
	for s.Scan() {
		p := strings.Split(s.Text(), "=")
		if len(p) != 2 {
			continue
		}
		if p[0] == "password" {
			t = p[1]
			break
		}
	}
	cmd.Wait()

	return t
}

// detectExecutable detects if the file at path is a pe, mach-o or elf executable.
func detectExecutable(path string) string {
	pf, err := pe.Open(path)
	if err == nil {
		pf.Close()
		return "windows"
	}

	mf, err := macho.Open(path)
	if err == nil && mf.FileHeader.Type == macho.TypeExec {
		mf.Close()
		return "darwin"
	}

	ef, err := elf.Open(path)
	if err == nil && ef.FileHeader.Type == elf.ET_EXEC {
		ef.Close()
		return "linux"
	}

	return ""
}

// installBin copies src to dst and makes it executable.
// it may emit some warnings which may or may not be helpful depending on the context.
func installBin(src, dst string) error {
	dstf, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	srcf, err := os.Open(src)
	if err != nil {
		return err
	}

	if _, err = io.Copy(dstf, srcf); err != nil {
		return err
	}
	srcf.Close()
	dstf.Close()

	x := detectExecutable(dst)
	switch {
	case x == "":
		log.Printf("warning: %s is not a known executable binary format", dst)
	case x != runtime.GOOS:
		log.Printf("warning: %s is a %s executable, os is %s", dst, x, runtime.GOOS)
	}
	log.Printf("  %s", dst)
	return nil
}

// installZip unzips executable files in the zip file src into dir.
// it may emit some warnings which may or may not be helpful depending on the context.
func installZip(src, dir string) error {
	rc, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	for _, f := range rc.File {
		if f.FileInfo().Mode()&0111 == 0 {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()

		dst := filepath.Join(dir, f.Name)

		o, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer o.Close()

		if _, err := io.Copy(o, rc); err != nil {
			return err
		}
		if err := os.Chmod(dst, f.FileInfo().Mode()); err != nil {
			return err
		}

		x := detectExecutable(dst)
		switch {
		case x == "":
			log.Printf("warning: %s is not a known executable binary format", dst)
		case x != runtime.GOOS:
			log.Printf("warning: %s is a %s executable, os is %s", dst, x, runtime.GOOS)
		}

		log.Printf("  %s", dst)
	}
	return nil
}

// locateGitDir locates a .git directory in the working directory or a parent.
// This is necessary to locate the VERSION file irl as there is no way to get
// the detected path back from go-git.
func locateGitDir(path string) (string, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	if p == "/" {
		return "", errors.New("unable to locate .git directory")
	}

	s, err := os.Stat(filepath.Join(p, ".git"))
	if err != nil {
		if os.IsNotExist(err) {
			return locateGitDir(filepath.Dir(p))
		}
		return "", err
	}

	if !s.IsDir() {
		return locateGitDir(filepath.Dir(p))
	}

	return p, nil
}

// octolog needs no introduction.
func octolog(c *client, s string) {
	p := strings.Replace(fmt.Sprintf(fmt.Sprintf("%%%ds", len(s)), ""), " ", "x", -1)
	o, _, err := c.Octocat(ctxbg, p)
	if err != nil {
		log.Print(s)
	}
	log.Print(strings.Replace(o, p, s, 1))
}

// erraggr returns a pair of channels for aggregating errors. it reads errors
// from the first channel, and returns after sending once on the second channel.
func erraggr() (chan error, chan []error) {
	var (
		rcv  = make(chan error)
		snd  = make(chan []error)
		errs = []error{}
	)
	go func() {
		for {
			select {
			case err := <-rcv:
				if err != nil {
					errs = append(errs, err)
				}
			case snd <- errs:
				return
			}
		}
	}()
	return rcv, snd
}

// passCommits returns a pair of channels which act as a commit passing buffer.
// If the read channel is read and the queue is empty, a nil commit will be sent,
// the read channel is closed and passing ends.
// Duplicate commits will be discarded.
func passCommits() (chan<- *object.Commit, <-chan *object.Commit) {
	snd := make(chan *object.Commit)
	rcv := make(chan *object.Commit)
	seen := map[plumbing.Hash]bool{}

	go func() {
		cs := []*object.Commit{}
		for {
			var c *object.Commit
			if len(cs) > 0 {
				c = cs[0]
			}
			select {
			case c := <-snd:
				if seen[c.Hash] {
					continue
				}
				seen[c.Hash] = true
				cs = append(cs, c)
			case rcv <- c:
				if len(cs) == 0 {
					close(rcv)
					return
				}
				cs = cs[1:]
			}
		}
	}()
	return snd, rcv
}

// ssmGet makes an aws ssm get parameter request using the default aws config.
// If the parameter is missing ssmGet returns an empty string and nil error.
// The parameter will be decrypted.
func ssmGet(p string) (string, error) {
	cfg, err := external.LoadDefaultAWSConfig()
	if err != nil {
		return "", err
	}
	rsp, err := ssm.New(cfg).GetParameterRequest(&ssm.GetParameterInput{
		Name:           aws.String(p),
		WithDecryption: aws.Bool(true),
	}).Send()
	if err != nil {
		if e, ok := err.(awserr.Error); ok {
			switch e.Code() {
			case ssm.ErrCodeParameterNotFound:
				// allows the auth chain to continue
				return "", nil
			case "MissingAuthenticationToken",
				"ExpiredTokenException",
				"EC2RoleRequestError":
				return "", errors.New("not authenticated with aws")
			}
		}
		return "", err
	}
	return *rsp.Parameter.Value, nil // TODO not sure if safe
}

// readArgs returns a slice of args. If one of the args supplied is "-", args
// will be scanned from stdin and inserted into the list, one per line.
func readArgs(args []string) ([]string, error) {
	b := false
	as := []string{}
	for _, a := range args {
		if a != "-" {
			as = append(as, strings.TrimSpace(a))
			continue
		}
		if b {
			return as, errors.New("-: cannot read stdin more than once")
		}
		b = true
		s := bufio.NewScanner(os.Stdin)
		for s.Scan() {
			as = append(as, strings.TrimSpace(s.Text()))
		}
		if err := s.Err(); err != nil {
			return as, err
		}
	}
	return as, nil
}

// usageFor constructs a generic usage function for subcmds
func usageFor(f *flag.FlagSet) func() {
	return func() {
		o := f.Output()
		fmt.Fprintf(o, help[f.Name()], os.Args[0], f.Name())
		fmt.Fprintln(o)
		i := 0
		f.VisitAll(func(f *flag.Flag) { i++ })
		if i > 0 {
			fmt.Fprintln(o, "Options:")
			f.PrintDefaults()
			fmt.Fprintln(o)
		}
	}
}

var (
	// if hubr is built with defaultOrg, the org part of usages becomes optional.
	helpOrgPart = func() string {
		if defaultOrg == "" {
			return "<org>/"
		}
		return "[<org>/]"
	}()
	helpDefaultOrg = func() string {
		if defaultOrg == "" {
			return "\n  A default org may be set by env HUBR_DEFAULT_ORG"
		}
		return "\n  The default org is " + defaultOrg + ". "
	}()
)

// primary usage
var helpMain = `Usage: %s [opts] <cmd>

  Command %s deals with GitHub tags, releases and assets.

  A GitHub personal access token is required. The following chain of token
  sources was specified at build time:
  	` + defaultChain + `

  For more help, -h any subcommand.
`

var help = map[string]string{
	// usage of the assets command
	"assets": `Usage: %s %s [opts] ` + helpOrgPart + `<repo>[@<tag>][:<asset>] [...]

  List release assets for one or more release tags. The parameter "-" will cause
  additional parameters to be read from standard input.

Parameter: ` + helpOrgPart + `<repo>[@<tag>][:<asset>]` + helpDefaultOrg + `
  The default tag is ` + defaultTag + `. Values of stable and edge are allowed.
  The value of asset is a glob, see https://godoc.org/path/filepath#Match.
  The default pattern matches all assets.
`,

	// usage of the bump command
	"bump": `Usage: %s %s [opts] <major|minor|patch>

  Create a new semantic version from a version file at head. If no version file
  is present the starting version is v0.0.0. Runs in local git repository. If
  the -latest flag is present the version will instead be bumped from the latest
  GitHub release. The resulting version string may be emitted on stdout or
  written to a version file.

  A changelog is generated if the -n flag is not present. First, a mainline is
  calculated from head. For any merge commit, the mainline is considered to be
  any parent commit where the version does not change.

  The log is constructed from the mainline commit messages for commits matching
  the current value of the version file. Any branches encontered are traversed
  back to the mainline and their commit messages inserted.

  The log for the new version may be printed on standard output or written to
  the version file. New lines are prepended to the committed content of the
  version file.
`,

	// usage of the cat command
	"cat": `Usage: %s %s ` + helpOrgPart + `<repo>[@<tag>]:<asset> [...]

  Download one or more release assets and concatenate the contents on standard
  output. The parameter "-" will cause additional parameters to be read from
  standard input. The cat command is very similar to get, however get is not
  safe to use to write to stdout. The get command is guaranteed to run faster
  if more than one asset is got.

Parameter: ` + helpOrgPart + `<repo>[@<tag>]:<asset>` + helpDefaultOrg + `
  The default tag is ` + defaultTag + `. Values of stable and edge are allowed.
  The value of asset is a glob, see https://godoc.org/path/filepath#Match.
  The default pattern matches all assets.
`,

	// usage of the get command
	"get": `Usage: %s %s [opts] ` + helpOrgPart + `<repo>[@<tag>]:<asset>[:<dest>] [...]

  Download one or more release assets to the working directory. The parameter "-"
  will cause additional parameters to be read from standard input.

Parameter: ` + helpOrgPart + `<repo>[@<tag>]:<asset>[:<dest>]` + helpDefaultOrg + `
  The default tag is ` + defaultTag + `. Values of stable and edge are allowed.
  The value of asset is a glob, see https://godoc.org/path/filepath#Match.
  The default pattern matches all assets.
  The default dest is the name of the asset, dest is not allowed when globbing.
`,

	// usage of the install command
	"install": `Usage: %s %s [opts] ` + helpOrgPart + `<repo>[@<tag>]:<asset>[:<dest>] [...]

  Install one or more standalone executables to a directory. The parameter "-"
  will cause additional parameters to be read from standard input.

  Supports application/octet-stream and application/zip.

Parameter: ` + helpOrgPart + `<repo>[@<tag>]:<asset>[:<dest>]` + helpDefaultOrg + `
  The default tag is ` + defaultTag + `. Values of stable and edge are allowed.
  The value of asset is a glob, see https://godoc.org/path/filepath#Match.
  The default pattern matches all assets.
  The default dest is the name of the asset, dest is not allowed when globbing.
`,

	// usage of the now command
	"now": `Usage: %s %s [opts]

  Test if the local repository head is a release commit. Based on version file.
  A release commit is any non-merge commit where the version file changes.
  See also bump, push.
`,

	// usage of the push command
	"push": `Usage: %s %s [opts] ` + helpOrgPart + `<repo> [<asset-file>] [...]

  Create a GitHub release for a release commit. Pushing is idempotent. If the
  current commit is not a release commit, nothing happens. Push is based on a
  version file. A release commit is any non-merge commit where the version file
  changes. The release version is the value of the version file in a release
  commit.

  If a tag does not exist for the release version, one is created. If the
  release does not exist it is created as a draft. The release body is created
  from additions to the changelog file in the release commit.

  Any asset files are uploaded. If the -f flag is present the full path of the
  file is used for the name. GitHub will replace path separators with dots.
  Otherwise, the basename will be used. If a release asset already exists,
  nothing happens.

  If the release is in draft state and the -d flag is present, the release
  remains in a draft state. Otherwise the release is published.

Parameter: ` + helpOrgPart + `<repo>` + helpDefaultOrg + `

Parameter: <asset-file>
  A path to a local release asset to be uploaded.
`,

	// usage of the release command
	"release": `Usage: %s %s [opts] ` + helpOrgPart + `<repo>@<tag> [<asset-file>] [...]

  Create a GitHub release for a specified tag. If the tag does not exist it will
  be created using a specified sha. If a sha is not specified, hubr will look
  for the tag in the local repository. If the tag does not exist, the sha of
  head is used. If the release does not exist it is created as a draft.

  Any asset files are uploaded. If the -f flag is present the full path of the
  file is used for the name. GitHub will replace path separators with dots.
  Otherwise, the basename will be used. If a release asset already exists,
  nothing happens.

  If the release is in draft state and the -d flag is present, the release
  remains in a draft state. Otherwise the release is published.

Parameter: ` + helpOrgPart + `<repo>@<tag>` + helpDefaultOrg + `.
  Tag values ` + defaultTag + `, stable, edge are not allowed.

Parameter: <asset-file>
  A path to a local release asset to be uploaded.
`,

	// usage of the resolve command
	"resolve": `Usage: %s %s [opts] ` + helpOrgPart + `<repo>[@<tag>] [...]

  Resolve tags! Returns the actual tag for latest, stable or edge release. The
  parameter "-" will cause additional parameters to be read from standard input.

  The output of resolve is a version locked form of the input, which may in turn
  be fed to the input of subcommands assets, get, release, and tags.

Parameter: ` + helpOrgPart + `<repo>[@<tag>]` + helpDefaultOrg + `
  The default tag is ` + defaultTag + `. Values of stable and edge are allowed.
`,

	// usage of the tags command
	"tags": `Usage: %s %s [opts] ` + helpOrgPart + `<repo> [...]

  List full release tags for one or more repositories. The parameter "-" will
  cause additional parameters to be read from standard input. Use the -a flag to
  list all tags including releases, pre-releases and unreleased tags.

Parameter: ` + helpOrgPart + `<repo>` + helpDefaultOrg + `
`,

	// usage of the what command
	"what": `Usage: %s %s [opts] [<repo-file>] [...]

Parameter: <repo-file>
  A file in the local repository, pathed from the repository root.

  List or check changes to the file tree since the last version. Depends on
  version file.  If no parameters are present, every file and directory that has
  changed since the last release commit is listed. With parameters, hubr will
  exit success if any of the named files or directories have changed. With the
  -all flag hubr will only exit success if all of the named files or directories
  have changed.
`,
}
