package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
)

const (
	windowDays     = 14
	pageSize       = 20
	githubGraphQL  = "https://api.github.com/graphql"
	githubAPI      = "https://api.github.com"
	maxRetries     = 3
	scanWorkers    = 10
	emptyRepoRetry = 7 * 24 * time.Hour
)

//go:embed views/*.html static/*
var assets embed.FS

type Release struct {
	Repo        string    `json:"repo"`
	Description string    `json:"description,omitempty"`
	AvatarURL   string    `json:"avatar_url,omitempty"`
	Version     string    `json:"version"`
	Name        string    `json:"name,omitempty"`
	Body        string    `json:"body,omitempty"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
}

type Exclusion struct {
	Repo      string    `json:"repo"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
}

type cache struct {
	UpdatedAt  time.Time   `json:"updated_at"`
	RepoCount  int         `json:"repo_count"`
	Releases   []Release   `json:"releases"`
	Exclusions []Exclusion `json:"exclusions,omitempty"`
}

type app struct {
	mu             sync.RWMutex
	cache          cache
	syncing        bool
	progress       string
	lastError      string
	token          string
	cachePath      string
	client         *http.Client
	tmpl           *template.Template
	markdown       goldmark.Markdown
	csrfToken      string
	exclusionEdits map[string]bool
	githubMu       sync.Mutex
	pauseUntil     time.Time
	primaryPauses  map[string]time.Time
	metrics        scanMetrics
}

type repository struct {
	Name        string
	Description string
	AvatarURL   string
}

type scanResult struct {
	repo     repository
	releases []Release
	empty    bool
	err      error
}

type releaseView struct {
	Release
	BodyHTML template.HTML
	Ago      string
	Date     string
}

type pageData struct {
	Page          string
	Releases      []releaseView
	Exclusions    []Exclusion
	UpdatedAt     string
	RepoCount     int
	ReleaseCount  int
	ExcludedCount int
	IsSyncing     bool
	Progress      string
	Error         string
	HasToken      bool
	WindowDays    int
	CSRFToken     string
	CurrentPage   int
	TotalPages    int
	NextPage      int
	Query         string
}

type graphResponse struct {
	Data struct {
		Viewer struct {
			Starred struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []struct {
					NameWithOwner string `json:"nameWithOwner"`
					Description   string `json:"description"`
					Owner         struct {
						AvatarURL string `json:"avatarUrl"`
					} `json:"owner"`
				} `json:"nodes"`
			} `json:"starredRepositories"`
		} `json:"viewer"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type githubRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
	Draft       bool      `json:"draft"`
}

func main() {
	tmpl, err := template.New("layout.html").Funcs(template.FuncMap{
		"initial": func(s string) string {
			if s != "" {
				return strings.ToUpper(s[:1])
			}
			return "?"
		},
		"reasonLabel": func(reason string) string {
			if reason == "no_releases" {
				return "No releases found"
			}
			return "Manually excluded"
		},
	}).ParseFS(assets, "views/*.html")
	if err != nil {
		log.Fatal(err)
	}

	a := &app{
		token:     normalizeToken(os.Getenv("GITHUB_TOKEN")),
		cachePath: filepath.Join(envOr("DATA_DIR", "data"), "releases.json"),
		client:    &http.Client{Timeout: 45 * time.Second},
		tmpl:      tmpl,
		markdown: goldmark.New(
			goldmark.WithExtensions(extension.GFM),
			goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		),
		csrfToken: randomToken(),
	}
	if err := a.loadCache(); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("cache: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", a.index)
	mux.HandleFunc("GET /excluded", a.excluded)
	mux.HandleFunc("POST /refresh", a.refresh)
	mux.HandleFunc("POST /dismiss-error", a.dismissError)
	mux.HandleFunc("POST /exclusions", a.addExclusion)
	mux.HandleFunc("POST /exclusions/remove", a.removeExclusion)
	mux.Handle("GET /static/", http.FileServerFS(assets))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	server := &http.Server{
		Addr:              envOr("LISTEN_ADDR", ":8090"),
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("GitVer listening on %s (manual scans only)", server.Addr)
	log.Fatal(server.ListenAndServe())
}

func (a *app) index(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	state, syncing, progress, lastError := a.cache, a.syncing, a.progress, a.lastError
	state.Releases = append([]Release(nil), state.Releases...)
	a.mu.RUnlock()

	cutoff := time.Now().UTC().AddDate(0, 0, -windowDays)
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	search := strings.ToLower(query)
	releases := make([]Release, 0, len(state.Releases))
	for _, release := range state.Releases {
		if release.PublishedAt.Before(cutoff) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(release.Repo+" "+release.Version+" "+release.Name+" "+release.Description+" "+release.Body), search) {
			continue
		}
		releases = append(releases, release)
	}
	updated := "Never"
	if !state.UpdatedAt.IsZero() {
		updated = timeAgo(state.UpdatedAt)
	}
	totalReleases := len(releases)
	totalPages := max(1, (totalReleases+pageSize-1)/pageSize)
	currentPage, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if currentPage < 1 {
		currentPage = 1
	}
	if currentPage > totalPages {
		currentPage = totalPages
	}
	start := (currentPage - 1) * pageSize
	end := min(start+pageSize, totalReleases)
	views := make([]releaseView, 0, end-start)
	for _, release := range releases[start:end] {
		var rendered bytes.Buffer
		if err := a.markdown.Convert([]byte(release.Body), &rendered); err != nil {
			log.Printf("render markdown for %s %s: %v", release.Repo, release.Version, err)
		}
		views = append(views, releaseView{
			Release: release, BodyHTML: template.HTML(rendered.String()),
			Ago: timeAgo(release.PublishedAt), Date: release.PublishedAt.Local().Format("2 Jan 2006, 15:04"),
		})
	}

	data := pageData{
		Page: "releases", Releases: views, UpdatedAt: updated, RepoCount: state.RepoCount,
		ReleaseCount: totalReleases, ExcludedCount: len(state.Exclusions), IsSyncing: syncing,
		Progress: progress, Error: lastError, HasToken: a.token != "", WindowDays: windowDays, CSRFToken: a.csrfToken,
		CurrentPage: currentPage, TotalPages: totalPages, Query: query,
		NextPage: min(totalPages, currentPage+1),
	}
	if isHTMX(r) && r.URL.Query().Get("fragment") == "releases" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := a.tmpl.ExecuteTemplate(w, "release-batch", data); err != nil {
			log.Printf("template: %v", err)
		}
		return
	}
	a.renderPage(w, data)
}

func (a *app) excluded(w http.ResponseWriter, _ *http.Request) {
	a.mu.RLock()
	state, syncing, lastError := a.cache, a.syncing, a.lastError
	state.Exclusions = append([]Exclusion(nil), state.Exclusions...)
	a.mu.RUnlock()
	sort.Slice(state.Exclusions, func(i, j int) bool { return state.Exclusions[i].Repo < state.Exclusions[j].Repo })
	updated := "Never"
	if !state.UpdatedAt.IsZero() {
		updated = timeAgo(state.UpdatedAt)
	}
	a.renderPage(w, pageData{
		Page: "excluded", Exclusions: state.Exclusions, UpdatedAt: updated,
		RepoCount: state.RepoCount, ReleaseCount: len(state.Releases), ExcludedCount: len(state.Exclusions),
		IsSyncing: syncing, Error: lastError, HasToken: a.token != "", WindowDays: windowDays, CSRFToken: a.csrfToken,
	})
}

func (a *app) renderPage(w http.ResponseWriter, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("template: %v", err)
	}
}

func (a *app) refresh(w http.ResponseWriter, r *http.Request) {
	if !a.validCSRF(r) {
		http.Error(w, "invalid request", http.StatusForbidden)
		return
	}
	a.startSync()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *app) dismissError(w http.ResponseWriter, r *http.Request) {
	if !a.validCSRF(r) {
		http.Error(w, "invalid request", http.StatusForbidden)
		return
	}
	a.mu.Lock()
	a.lastError = ""
	a.mu.Unlock()
	if isHTMX(r) {
		w.WriteHeader(http.StatusOK)
		return
	}
	destination := "/"
	if r.FormValue("next") == "/excluded" {
		destination = "/excluded"
	}
	http.Redirect(w, r, destination, http.StatusSeeOther)
}

func (a *app) addExclusion(w http.ResponseWriter, r *http.Request) {
	if !a.validCSRF(r) {
		http.Error(w, "invalid request", http.StatusForbidden)
		return
	}
	repo, ok := normalizeRepo(r.FormValue("repo"))
	if !ok {
		a.setError("Enter a repository as owner/name.")
		http.Redirect(w, r, "/excluded", http.StatusSeeOther)
		return
	}
	a.mu.Lock()
	if a.syncing {
		a.exclusionEdits[repo] = true
	}
	found := false
	for index := range a.cache.Exclusions {
		if strings.EqualFold(a.cache.Exclusions[index].Repo, repo) {
			a.cache.Exclusions[index].Reason = "manual"
			a.cache.Exclusions[index].CheckedAt = time.Time{}
			found = true
			break
		}
	}
	if !found {
		a.cache.Exclusions = append(a.cache.Exclusions, Exclusion{Repo: repo, Reason: "manual", CreatedAt: time.Now().UTC()})
	}
	keptReleases := a.cache.Releases[:0]
	for _, release := range a.cache.Releases {
		if !strings.EqualFold(release.Repo, repo) {
			keptReleases = append(keptReleases, release)
		}
	}
	a.cache.Releases = keptReleases
	a.lastError = ""
	err := a.saveCacheLocked()
	excludedCount := len(a.cache.Exclusions)
	releaseCount := len(a.cache.Releases)
	a.mu.Unlock()
	if err != nil {
		a.setError("Could not save exclusion: " + err.Error())
		http.Error(w, "could not save exclusion", http.StatusInternalServerError)
		return
	}
	if isHTMX(r) && r.FormValue("next") == "/" {
		trigger, _ := json.Marshal(map[string]any{"repositoryExcluded": map[string]string{"repo": repo}})
		w.Header().Set("HX-Trigger", string(trigger))
		writeCounts(w, excludedCount, releaseCount)
		return
	}
	destination := "/excluded"
	if r.FormValue("next") == "/" {
		destination = "/"
	}
	http.Redirect(w, r, destination, http.StatusSeeOther)
}

func (a *app) removeExclusion(w http.ResponseWriter, r *http.Request) {
	if !a.validCSRF(r) {
		http.Error(w, "invalid request", http.StatusForbidden)
		return
	}
	repo, ok := normalizeRepo(r.FormValue("repo"))
	excludedCount := 0
	if ok {
		a.mu.Lock()
		if a.syncing {
			a.exclusionEdits[repo] = false
		}
		kept := a.cache.Exclusions[:0]
		for _, exclusion := range a.cache.Exclusions {
			if !strings.EqualFold(exclusion.Repo, repo) {
				kept = append(kept, exclusion)
			}
		}
		a.cache.Exclusions = kept
		err := a.saveCacheLocked()
		excludedCount = len(a.cache.Exclusions)
		a.mu.Unlock()
		if err != nil {
			a.setError("Could not save exclusions: " + err.Error())
			http.Error(w, "could not save exclusions", http.StatusInternalServerError)
			return
		}
	}
	if isHTMX(r) {
		writeExclusionCount(w, excludedCount)
		return
	}
	http.Redirect(w, r, "/excluded", http.StatusSeeOther)
}

func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

func writeExclusionCount(w http.ResponseWriter, count int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<span id="exclusion-count" hx-swap-oob="true">%d</span>`, count)
}

func writeCounts(w http.ResponseWriter, exclusions, releases int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<span id="exclusion-count" hx-swap-oob="true">%d</span><b id="release-count" hx-swap-oob="true">%d</b>`, exclusions, releases)
}

func (a *app) startSync() {
	a.mu.Lock()
	if a.syncing {
		a.mu.Unlock()
		return
	}
	if a.token == "" {
		a.lastError = "Set GITHUB_TOKEN to scan your starred repositories."
		a.mu.Unlock()
		return
	}
	a.syncing, a.lastError, a.progress = true, "", "Loading starred repositories…"
	a.exclusionEdits = make(map[string]bool)
	exclusions := append([]Exclusion(nil), a.cache.Exclusions...)
	previous := append([]Release(nil), a.cache.Releases...)
	a.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		fresh, err := a.fetchReleases(ctx, exclusions, previous)
		a.mu.Lock()
		defer a.mu.Unlock()
		a.syncing, a.progress = false, ""
		defer func() { a.exclusionEdits = nil }()
		if err != nil {
			a.lastError = err.Error()
			log.Printf("scan failed: %v", err)
			return
		}
		a.mergeScanLocked(fresh)
		if err := a.saveCacheLocked(); err != nil {
			a.lastError = "Scan completed, but the cache could not be saved: " + err.Error()
		}
		log.Printf("scan complete: %d starred, %d excluded, %d recent releases", a.cache.RepoCount, len(a.cache.Exclusions), len(a.cache.Releases))
	}()
}

func (a *app) fetchReleases(ctx context.Context, exclusions []Exclusion, previous []Release) (cache, error) {
	started := time.Now()
	a.githubMu.Lock()
	a.metrics = scanMetrics{}
	a.githubMu.Unlock()
	defer a.logScanMetrics(started)
	repos, err := a.fetchStarred(ctx)
	if err != nil {
		return cache{}, err
	}
	excluded := make(map[string]bool, len(exclusions))
	rechecking := make(map[string]Exclusion)
	var keptExclusions []Exclusion
	for _, exclusion := range exclusions {
		checked := exclusion.CheckedAt
		if checked.IsZero() {
			checked = exclusion.CreatedAt
		}
		if exclusion.Reason == "no_releases" && time.Since(checked) >= emptyRepoRetry {
			rechecking[strings.ToLower(exclusion.Repo)] = exclusion
			continue
		}
		excluded[strings.ToLower(exclusion.Repo)] = true
		keptExclusions = append(keptExclusions, exclusion)
	}
	total := 0
	for _, repo := range repos {
		if !excluded[strings.ToLower(repo.Name)] {
			total++
		}
	}
	jobs := make(chan repository)
	results := make(chan scanResult)
	var workers sync.WaitGroup
	for range scanWorkers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for repo := range jobs {
				releases, empty, err := a.fetchRepoReleases(ctx, repo)
				results <- scanResult{repo: repo, releases: releases, empty: empty, err: err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, repo := range repos {
			if !excluded[strings.ToLower(repo.Name)] {
				select {
				case jobs <- repo:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	go func() { workers.Wait(); close(results) }()

	result := cache{UpdatedAt: time.Now().UTC(), RepoCount: len(repos), Exclusions: keptExclusions}
	failed := make(map[string]bool)
	completed := 0
	for item := range results {
		completed++
		a.setProgress(fmt.Sprintf("Scanning repositories… %d/%d", completed, total))
		if item.err != nil {
			failed[strings.ToLower(item.repo.Name)] = true
			if exclusion, ok := rechecking[strings.ToLower(item.repo.Name)]; ok {
				result.Exclusions = append(result.Exclusions, exclusion)
			}
			log.Printf("scan %s: %v", item.repo.Name, item.err)
			continue
		}
		if item.empty {
			exclusion, ok := rechecking[strings.ToLower(item.repo.Name)]
			if !ok {
				exclusion = Exclusion{Repo: item.repo.Name, Reason: "no_releases", CreatedAt: time.Now().UTC()}
			}
			exclusion.CheckedAt = time.Now().UTC()
			result.Exclusions = append(result.Exclusions, exclusion)
			continue
		}
		result.Releases = append(result.Releases, item.releases...)
	}
	if err := ctx.Err(); err != nil {
		return cache{}, err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -windowDays)
	for _, release := range previous {
		if failed[strings.ToLower(release.Repo)] && !release.PublishedAt.Before(cutoff) {
			result.Releases = append(result.Releases, release)
		}
	}
	sort.Slice(result.Releases, func(i, j int) bool { return result.Releases[i].PublishedAt.After(result.Releases[j].PublishedAt) })
	if len(failed) > 0 {
		a.setError(fmt.Sprintf("Scan completed, but %d repositories could not be refreshed; their previous results were kept.", len(failed)))
	}
	return result, nil
}

func (a *app) mergeScanLocked(fresh cache) {
	var exclusions []Exclusion
	for _, exclusion := range fresh.Exclusions {
		if _, edited := a.exclusionEdits[strings.ToLower(exclusion.Repo)]; !edited {
			exclusions = append(exclusions, exclusion)
		}
	}
	for _, exclusion := range a.cache.Exclusions {
		if a.exclusionEdits[strings.ToLower(exclusion.Repo)] {
			exclusions = append(exclusions, exclusion)
		}
	}
	excluded := make(map[string]bool, len(exclusions))
	for _, exclusion := range exclusions {
		excluded[strings.ToLower(exclusion.Repo)] = true
	}
	releases := fresh.Releases[:0]
	for _, release := range fresh.Releases {
		if !excluded[strings.ToLower(release.Repo)] {
			releases = append(releases, release)
		}
	}
	fresh.Releases, fresh.Exclusions = releases, exclusions
	a.cache = fresh
}

func (a *app) fetchStarred(ctx context.Context) ([]repository, error) {
	const query = `query Starred($cursor: String) { viewer { starredRepositories(first: 100, after: $cursor) { pageInfo { hasNextPage endCursor } nodes { nameWithOwner description owner { avatarUrl } } } } }`
	var repos []repository
	var cursor any
	for {
		payload, _ := json.Marshal(map[string]any{"query": query, "variables": map[string]any{"cursor": cursor}})
		body, err := a.githubRequest(ctx, http.MethodPost, githubGraphQL, payload)
		if err != nil {
			return nil, err
		}
		var graph graphResponse
		if err := json.Unmarshal(body, &graph); err != nil {
			return nil, fmt.Errorf("decode GitHub response: %w", err)
		}
		if len(graph.Errors) > 0 {
			return nil, fmt.Errorf("GitHub API: %s", graph.Errors[0].Message)
		}
		for _, node := range graph.Data.Viewer.Starred.Nodes {
			repos = append(repos, repository{Name: node.NameWithOwner, Description: node.Description, AvatarURL: node.Owner.AvatarURL})
		}
		page := graph.Data.Viewer.Starred.PageInfo
		if !page.HasNextPage {
			return repos, nil
		}
		if page.EndCursor == "" || page.EndCursor == cursor {
			return nil, errors.New("GitHub returned an invalid pagination cursor")
		}
		cursor = page.EndCursor
	}
}

func (a *app) fetchRepoReleases(ctx context.Context, repo repository) ([]Release, bool, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -windowDays)
	var releases []Release
	hasAny := false
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s/repos/%s/releases?per_page=100&page=%d", githubAPI, repo.Name, page)
		body, err := a.githubRequest(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, false, err
		}
		var batch []githubRelease
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, false, fmt.Errorf("decode releases: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		hasAny = true
		allOld := true
		for _, release := range batch {
			if release.Draft || release.PublishedAt.IsZero() || release.PublishedAt.Before(cutoff) {
				continue
			}
			allOld = false
			releases = append(releases, Release{
				Repo: repo.Name, Description: repo.Description, AvatarURL: repo.AvatarURL,
				Version: release.TagName, Name: release.Name, Body: release.Body,
				URL: release.HTMLURL, PublishedAt: release.PublishedAt,
			})
		}
		if len(batch) < 100 || allOld {
			break
		}
	}
	return releases, !hasAny, nil
}

func (a *app) validCSRF(r *http.Request) bool {
	return r.FormValue("csrf") == a.csrfToken
}

func (a *app) setProgress(progress string) {
	a.mu.Lock()
	a.progress = progress
	a.mu.Unlock()
}

func (a *app) setError(message string) {
	a.mu.Lock()
	a.lastError = message
	a.mu.Unlock()
}

func containsExclusion(exclusions []Exclusion, repo string) bool {
	for _, exclusion := range exclusions {
		if strings.EqualFold(exclusion.Repo, repo) {
			return true
		}
	}
	return false
}

func normalizeRepo(raw string) (string, bool) {
	raw = strings.Trim(strings.TrimSpace(raw), "/")
	parts := strings.Split(raw, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	for _, part := range parts {
		if strings.ContainsAny(part, " \\?#%") {
			return "", false
		}
	}
	return strings.ToLower(raw), true
}

func normalizeToken(raw string) string {
	raw = strings.TrimSpace(raw)
	for _, prefix := range []string{"Bearer ", "bearer ", "token "} {
		raw = strings.TrimPrefix(raw, prefix)
	}
	return strings.TrimSpace(raw)
}

func randomToken() string {
	data := make([]byte, 24)
	if _, err := rand.Read(data); err != nil {
		log.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func (a *app) loadCache() error {
	data, err := os.ReadFile(a.cachePath)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &a.cache)
}

func (a *app) saveCacheLocked() error {
	return writeJSON(a.cachePath, a.cache)
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%d hr ago", int(d.Hours()))
	}
	days := int(d.Hours() / 24)
	if days == 1 {
		return "yesterday"
	}
	return fmt.Sprintf("%d days ago", days)
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https://avatars.githubusercontent.com data:; style-src 'self'; script-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
