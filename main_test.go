package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
)

func TestNormalizeRepo(t *testing.T) {
	for input, want := range map[string]string{
		"Owner/Repo":   "owner/repo",
		" owner/repo/": "owner/repo",
	} {
		got, ok := normalizeRepo(input)
		if !ok || got != want {
			t.Errorf("normalizeRepo(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}
	for _, input := range []string{"owner", "owner/repo/extra", "owner/re po", ""} {
		if _, ok := normalizeRepo(input); ok {
			t.Errorf("normalizeRepo(%q) should be invalid", input)
		}
	}
}

func TestContainsExclusionCaseInsensitive(t *testing.T) {
	exclusions := []Exclusion{{Repo: "owner/repo"}}
	if !containsExclusion(exclusions, "OWNER/REPO") {
		t.Error("expected case-insensitive exclusion match")
	}
}

func TestFetchRepoReleasesMarksSuccessfulEmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()

	a := &app{token: "test", client: server.Client()}
	body, err := a.githubRequest(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	var releases []githubRelease
	if err := json.Unmarshal(body, &releases); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(releases, []githubRelease{}) {
		t.Fatalf("releases = %#v, want empty", releases)
	}
}

func TestTimeAgo(t *testing.T) {
	if got := timeAgo(time.Now().Add(-48 * time.Hour)); got != "2 days ago" {
		t.Fatalf("timeAgo() = %q, want %q", got, "2 days ago")
	}
}

type countingMarkdown struct {
	goldmark.Markdown
	calls int
}

func (markdown *countingMarkdown) Convert(source []byte, writer io.Writer, options ...parser.ParseOption) error {
	markdown.calls++
	return markdown.Markdown.Convert(source, writer, options...)
}

func TestReleaseActionMenuOverridesOatDisclosureStyles(t *testing.T) {
	stylesheet, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		selector string
		want     []string
	}{
		{selector: ".release-actions", want: []string{"overflow: visible;"}},
		{selector: ".action-menu", want: []string{"margin: 0;", "animation: none;"}},
	} {
		rule := regexp.MustCompile(regexp.QuoteMeta(test.selector) + `\s*\{[^}]*\}`).FindString(string(stylesheet))
		for _, declaration := range test.want {
			if !strings.Contains(rule, declaration) {
				t.Errorf("%s must override Oat with %q", test.selector, declaration)
			}
		}
	}
}

func TestReleaseTimeline(t *testing.T) {
	for _, test := range []struct {
		name       string
		url        string
		fragment   bool
		wantCards  int
		wantNext   string
		wantQuery  string
		wantText   string
		wantAbsent string
	}{
		{name: "first page", url: "/", wantCards: 20, wantNext: "page=2", wantText: "<!doctype html>", wantAbsent: "v20<"},
		{name: "next fragment", url: "/?page=2&fragment=releases", fragment: true, wantCards: 20, wantNext: "page=3", wantText: "v20<", wantAbsent: "<!doctype html>"},
		{name: "last fragment", url: "/?page=3&fragment=releases", fragment: true, wantCards: 5, wantText: "all caught up", wantAbsent: "load-more-link"},
		{name: "search across pages", url: "/?q=UNIQUE", wantCards: 1, wantText: "v44<", wantAbsent: "v0<"},
		{name: "search pagination", url: "/?q=owner%2FRepo&page=2&fragment=releases", fragment: true, wantCards: 20, wantNext: "q=owner%2FRepo", wantText: "v20<"},
		{name: "search escapes query separators", url: "/?q=owner%2FRepo%26x%23tag", wantQuery: `q=owner%2FRepo%26x%23tag`, wantText: "No matching releases"},
		{name: "empty search", url: "/?q=absent", wantText: "No matching releases", wantAbsent: "load-more-link"},
		{name: "invalid page", url: "/?page=-5", wantCards: 20, wantNext: "page=2", wantText: "v0<"},
		{name: "out of range", url: "/?page=999", wantCards: 5, wantText: "all caught up", wantAbsent: "load-more-link"},
		{name: "no JS navigation", url: "/?page=2&fragment=releases", wantCards: 20, wantText: "<!doctype html>"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tmpl, err := template.New("layout.html").Funcs(template.FuncMap{
				"initial":     func(value string) string { return "R" },
				"reasonLabel": func(value string) string { return value },
			}).ParseFS(assets, "views/*.html")
			if err != nil {
				t.Fatal(err)
			}
			markdown := &countingMarkdown{Markdown: goldmark.New(goldmark.WithExtensions(extension.GFM))}
			application := &app{tmpl: tmpl, markdown: markdown, csrfToken: "test-csrf"}
			for index := range 45 {
				body := "## Changes\n\n- **Fixed** a bug\n\n<script>alert('unsafe')</script>"
				if index == 44 {
					body += "\nunique improvement"
				}
				avatarURL := ""
				if index%2 == 0 {
					avatarURL = "https://example.com/avatar.png"
				}
				application.cache.Releases = append(application.cache.Releases, Release{
					Repo: "owner/Repo", Version: fmt.Sprintf("v%d", index), Body: body,
					AvatarURL:   avatarURL,
					PublishedAt: time.Now().Add(-time.Duration(index) * time.Hour),
				})
			}
			application.cache.Releases = append(application.cache.Releases, Release{
				Repo: "owner/old", Version: "expired", PublishedAt: time.Now().AddDate(0, 0, -15),
			})
			request := httptest.NewRequest(http.MethodGet, test.url, nil)
			if test.fragment {
				request.Header.Set("HX-Request", "true")
			}
			response := httptest.NewRecorder()
			application.index(response, request)
			body := response.Body.String()
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d", response.Code)
			}
			if cards := strings.Count(body, `class="release"`); cards != test.wantCards {
				t.Errorf("cards = %d, want %d", cards, test.wantCards)
			}
			for _, repoName := range regexp.MustCompile(`(?s)<div class="repo-name">.*?</div>`).FindAllString(body, -1) {
				icon := strings.Index(repoName, `<img `)
				if icon == -1 {
					icon = strings.Index(repoName, `<span class="avatar">`)
				}
				link := strings.Index(repoName, `<a href="https://github.com/owner/Repo"`)
				if icon == -1 || link <= icon {
					t.Errorf("repository icon must precede the name and link: %s", repoName)
				}
			}
			menus := regexp.MustCompile(`(?s)<details class="release-actions">.*?</details>`)
			if count := len(menus.FindAllString(body, -1)); count != test.wantCards {
				t.Errorf("closed action menus = %d, want %d", count, test.wantCards)
			}
			for _, menu := range menus.FindAllString(body, -1) {
				for _, want := range []string{`<summary aria-label="Actions for owner/Repo"`, `action="/exclusions" method="post"`, `name="csrf" value="test-csrf"`, `name="repo" value="owner/Repo"`, `name="next" value="/"`, `>Exclude repository</button>`} {
					if !strings.Contains(menu, want) {
						t.Errorf("action menu missing %q", want)
					}
				}
			}
			if strings.Contains(menus.ReplaceAllString(body, ""), `class="exclude-button"`) {
				t.Error("exclusion button must only appear inside a closed action menu")
			}
			if !test.fragment {
				for _, want := range []string{`/static/oat.min.css`, `/static/oat-concrete.css`} {
					if !strings.Contains(body, want) {
						t.Errorf("page missing selected theme asset %q", want)
					}
				}
			}
			if markdown.calls != test.wantCards {
				t.Errorf("Markdown conversions = %d, want %d", markdown.calls, test.wantCards)
			}
			for _, want := range []string{test.wantText, test.wantNext} {
				if want != "" && !strings.Contains(body, want) {
					t.Errorf("response does not contain %q", want)
				}
			}
			for _, absent := range []string{test.wantAbsent, "expired", "<script>alert('unsafe')</script>"} {
				if absent != "" && strings.Contains(body, absent) {
					t.Errorf("response unexpectedly contains %q", absent)
				}
			}
			if test.wantCards > 0 && !strings.Contains(body, "<strong>Fixed</strong>") {
				t.Error("release notes are not rendered as Markdown")
			}
			if test.wantNext != "" && !strings.Contains(body, `hx-select="unset"`) {
				t.Error("infinite-scroll fragments must not inherit the full-page selector")
			}
			if test.wantQuery != "" && !strings.Contains(body, test.wantQuery) {
				t.Errorf("response does not contain encoded query %q", test.wantQuery)
			}
		})
	}
}
