package main

import (
	"fmt"
	"github.com/dgraph-io/badger/v4"
	"gitvergo/githubapi"
	"gitvergo/repository"
	"gitvergo/utils"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

var (
	indexTpl    *template.Template
	releasesTpl *template.Template
	loginTpl    *template.Template

	githubToken string
	appUser     string
	appPassword string

	db *badger.DB
)

func init() {
	// Parse templates
	indexTpl = template.Must(template.ParseFiles("templates/index.html"))
	releasesTpl = template.Must(template.ParseFiles("templates/releaseCard.html"))
	loginTpl = template.Must(template.ParseFiles("templates/login.html"))

	// Read environment variables
	githubToken = os.Getenv("GITHUB_API_KEY")
	if githubToken == "" {
		log.Fatal("GITHUB_API_KEY environment variable is not set")
	}
	appUser = os.Getenv("APP_USER")
	appPassword = os.Getenv("APP_PASSWORD")
	if appUser == "" || appPassword == "" {
		log.Fatal("APP_USER and APP_PASSWORD environment variables must be set")
	}
}

func main() {
	// Open Badger DB (data stored in "/tmp/gitver")
	var err error
	db, err = badger.Open(badger.DefaultOptions("/tmp/gitver"))
	if err != nil {
		log.Fatalf("Failed to open Badger DB: %v", err)
	}
	defer db.Close()

	// Public endpoints.
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("/logout", logoutHandler)

	// Protected endpoints.
	http.Handle("/", authenticate(http.HandlerFunc(indexHandler)))
	http.Handle("/releases", authenticate(http.HandlerFunc(releasesHandler)))
	http.Handle("/refresh", authenticate(http.HandlerFunc(refreshHandler)))
	// static
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Server starting on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// loginHandler handles both GET (display form) and POST (process login).
func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		err := loginTpl.Execute(w, nil)
		if err != nil {
			http.Error(w, "Failed to execute login template", http.StatusNotFound)
		}
		return
	} else if r.Method == http.MethodPost {
		loginPostHandler(w, r)
		return
	}
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// loginPostHandler processes the login form
func loginPostHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Parse error", http.StatusBadRequest)
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")

	// Get credentials from env
	appUser := os.Getenv("APP_USER")
	appPassword := os.Getenv("APP_PASSWORD")

	if username == appUser && password == appPassword {
		// Generate JWT token
		token, err := utils.GenerateToken(username)
		if err != nil {
			http.Error(w, "Failed to generate token", http.StatusInternalServerError)
			return
		}

		// Set session cookie with JWT
		http.SetCookie(w, &http.Cookie{
			Name:     "session",
			Value:    token,
			Path:     "/",
			Expires:  time.Now().Add(7 * 24 * time.Hour),
			HttpOnly: true, // Prevent JavaScript access
		})

		http.Redirect(w, r, "/", http.StatusSeeOther)
	} else {
		err := loginTpl.Execute(w, struct{ Error string }{Error: "Invalid credentials"})
		if err != nil {
			return
		}
	}
}

// logoutHandler clears the session cookie
func logoutHandler(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:    "session",
		Value:   "",
		Path:    "/",
		Expires: time.Now().Add(-1 * time.Hour),
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		// Validate token
		token, err := utils.ValidateToken(cookie.Value)
		if err != nil || !token.Valid {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		// Proceed to next handler
		next.ServeHTTP(w, r)
	})
}

// indexHandler serves the main page.
func indexHandler(w http.ResponseWriter, r *http.Request) {
	if err := indexTpl.Execute(w, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// releasesHandler loads releases from Badger and renders the releases fragment.
func releasesHandler(w http.ResponseWriter, r *http.Request) {
	releases, err := repository.GetReleasesFromDB(db)
	if err != nil {
		// If there's an error (e.g. first run), fetch from GitHub.
		releases, err = githubapi.LoadReleases(githubToken)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to load releases: %v", err), http.StatusInternalServerError)
			return
		}
		_ = repository.StoreReleases(releases, db)
	}

	// Pagination: default 10 releases per page.
	pageStr := r.URL.Query().Get("page")
	page := 1
	if pageStr != "" {
		if p, err := strconv.Atoi(pageStr); err == nil && p > 0 {
			page = p
		}
	}
	const perPage = 10
	total := len(releases)
	totalPages := (total + perPage - 1) / perPage
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * perPage
	end := start + perPage
	if end > total {
		end = total
	}
	prevPage := 0
	if page > 1 {
		prevPage = page - 1
	}
	nextPage := 0
	if page < totalPages {
		nextPage = page + 1
	}
	paginated := repository.PaginatedReleases{
		Releases:    releases[start:end],
		CurrentPage: page,
		TotalPages:  totalPages,
		PrevPage:    prevPage,
		NextPage:    nextPage,
	}

	if err := releasesTpl.Execute(w, paginated); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// refreshHandler fetches releases from GitHub, stores them, and returns the updated fragment.
func refreshHandler(w http.ResponseWriter, r *http.Request) {
	releases, err := githubapi.LoadReleases(githubToken)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to refresh releases: %v", err), http.StatusInternalServerError)
		return
	}
	if err := repository.StoreReleases(releases, db); err != nil {
		http.Error(w, fmt.Sprintf("Failed to store releases: %v", err), http.StatusInternalServerError)
		return
	}
	if err := releasesTpl.Execute(w, releases); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
